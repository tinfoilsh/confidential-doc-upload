package server

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/tinfoilsh/tinfoil-go"
)

var (
	vlmModel = envOr("VLM_MODEL", "gemma4-31b")
	vlmKey   = envOr("TINFOIL_API_KEY", "")

	tinfoilVLM *tinfoil.Client
)

const vlmOCRPrompt = "Convert this page to markdown. Do not miss any text and only output the bare markdown!"

const vlmVisualPrompt = `You are a document element extractor. Your job is to extract ONLY structured visual elements from this page.

EXTRACT these element types:
- TABLE: reproduce as a markdown table (header row + data rows)
- CHART/GRAPH: state the title, axes, data series names, and approximate values and trends.
- DIAGRAM/FIGURE: briefly describe what is depicted (arrows, boxes, flow)
- LATEX FORMULAS: write the formula in LaTeX format

DO NOT extract:
- Regular text paragraphs
- Code listing
- Highlighted text boxes
- Section headings
- References or citations
- Captions (unless part of a figure)

Format each element as:
[TYPE]: content

If the page contains NONE of the above elements, respond with exactly: NONE`

func initTinfoilClient() {
	if vlmKey == "" {
		slog.Warn("no TINFOIL_API_KEY set, VLM calls will fail")
		return
	}
	client, err := tinfoil.NewClient(
		option.WithAPIKey(vlmKey),
	)
	if err != nil {
		slog.Error("failed to create Tinfoil client", "err", err)
		return
	}
	tinfoilVLM = client
	slog.Info("tinfoil VLM client initialized", "model", vlmModel)
}

func vlmCall(ctx context.Context, imageB64, prompt string, maxTokens int) (string, error) {
	if tinfoilVLM == nil {
		return "", fmt.Errorf("VLM client not initialized (missing TINFOIL_API_KEY?)")
	}
	resp, err := tinfoilVLM.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model: vlmModel,
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.UserMessage([]openai.ChatCompletionContentPartUnionParam{
				openai.ImageContentPart(openai.ChatCompletionContentPartImageImageURLParam{
					URL: "data:image/png;base64," + imageB64,
				}),
				openai.TextContentPart(prompt),
			}),
		},
		MaxTokens:        openai.Int(int64(maxTokens)),
		FrequencyPenalty: openai.Float(0.3),
	})
	if err != nil {
		return "", fmt.Errorf("vlm: %w", err)
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("vlm: empty response")
	}

	md := strings.TrimSpace(resp.Choices[0].Message.Content)
	if strings.HasPrefix(md, "```") {
		if i := strings.Index(md, "\n"); i >= 0 {
			md = md[i+1:]
		}
	}
	md = strings.TrimSuffix(strings.TrimRight(md, "\n"), "```")
	md = truncateRepetition(md)
	return md, nil
}

// truncateRepetition removes repetition loops from VLM output while preserving
// content before and after each loop. Single left-to-right scan: at each byte
// position, check all window sizes for 3+ consecutive repeats. When found,
// excise the repeated block and continue from the same position.
func truncateRepetition(s string) string {
	const minWindow = 10
	const maxWindow = 200
	const minRepeats = 3

	b := []byte(s)
	changed := false
	i := 0
	for i+minWindow*minRepeats <= len(b) {
		bestLen := 0
		for window := minWindow; window <= maxWindow && i+window*minRepeats <= len(b); window++ {
			repeats := 1
			for j := i + window; j+window <= len(b); j += window {
				if string(b[j:j+window]) == string(b[i:i+window]) {
					repeats++
				} else {
					break
				}
			}
			if repeats >= minRepeats {
				blockLen := window * repeats
				if blockLen > bestLen {
					bestLen = blockLen
				}
			}
		}
		if bestLen > 0 {
			slog.Warn("excised VLM repetition loop",
				"at_byte", i, "removed_bytes", bestLen, "text_len", len(b))
			b = append(b[:i], b[i+bestLen:]...)
			changed = true
		} else {
			i++
		}
	}

	if changed {
		return strings.TrimSpace(string(b))
	}
	return s
}

func vlmFullPageOCR(ctx context.Context, imageB64 string) (string, error) {
	return vlmCall(ctx, imageB64, vlmOCRPrompt, 8000)
}

func vlmVisualExtract(ctx context.Context, imageB64 string) (string, error) {
	return vlmCall(ctx, imageB64, vlmVisualPrompt, 4000)
}

type vlmPageFunc func(ctx context.Context, imageB64 string) (string, error)

type vlmWorkItem struct {
	image string
	fn    vlmPageFunc
	kind  string // "ocr" or "visual"
}

type vlmResult struct {
	text string
	err  error
}

func vlmParallelMixed(ctx context.Context, work map[int]vlmWorkItem) map[int]vlmResult {
	results := make(map[int]vlmResult)
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, maxParallel)

	for idx, item := range work {
		wg.Add(1)
		go func(pageIdx int, w vlmWorkItem) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				mu.Lock()
				results[pageIdx] = vlmResult{err: ctx.Err()}
				mu.Unlock()
				return
			}

			text, err := w.fn(ctx, w.image)
			mu.Lock()
			results[pageIdx] = vlmResult{text: text, err: err}
			mu.Unlock()
		}(idx, item)
	}
	wg.Wait()
	return results
}
