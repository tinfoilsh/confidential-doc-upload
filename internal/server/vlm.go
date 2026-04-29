package server

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/tinfoilsh/tinfoil-go"
)

var (
	vlmModel = envOr("VLM_MODEL", "gemma4-31b")
	vlmKey   = envOr("TINFOIL_API_KEY", "")

	tinfoilVLM *tinfoil.Client

	// lastVLMSuccess is the unix-nano timestamp of the most recent successful
	// VLM round-trip (real call or background probe). vlmHealthy() reads this
	// to decide if the SDK is currently usable — a non-nil tinfoilVLM pointer
	// is not sufficient because the SDK can lose its enclave connection,
	// fail cert rotation, or be pointed at a stale enclave host.
	lastVLMSuccess atomic.Int64

	// vlmProbeInterval is how often the background watchdog forces a real
	// inference roundtrip. Keep short enough that Betterstack catches an
	// outage within ~1 minute.
	vlmProbeInterval = time.Duration(envInt("VLM_PROBE_INTERVAL_SECONDS", 30)) * time.Second

	// vlmHealthyWindow is the staleness threshold for lastVLMSuccess. If we
	// haven't seen a successful call (real or probe) in this window, we
	// report degraded.
	vlmHealthyWindow = time.Duration(envInt("VLM_HEALTHY_WINDOW_SECONDS", 90)) * time.Second
)

// 1x1 transparent PNG used by the startup/background probes. We need a real
// inference roundtrip — not just a TLS dial — so SDK-level failures (lost
// enclave connection, cert rotation gave up, model rotated to a new enclave
// we can't reach) actually surface in /health.
const probePNGBase64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="

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
	lastVLMSuccess.Store(time.Now().UnixNano())

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

// vlmHealthy reports whether the VLM has succeeded recently. It is the
// canonical signal for /health: a non-nil tinfoilVLM pointer alone is
// insufficient because the SDK can lose its enclave connection, fail
// attestation re-verification on cert rotation, or be pointing at a stale
// enclave host after the model migrated to a new one.
//
// "Recent" is defined by vlmHealthyWindow (default 90s). lastVLMSuccess is
// updated by both real traffic (vlmCall) and the background probe, so an
// idle service still reports correctly as long as the watchdog is running.
func vlmHealthy() bool {
	if tinfoilVLM == nil {
		return false
	}
	last := lastVLMSuccess.Load()
	if last == 0 {
		return false
	}
	return time.Since(time.Unix(0, last)) <= vlmHealthyWindow
}

// vlmProbe forces a real inference roundtrip with a 1x1 PNG. Used at
// startup to seed lastVLMSuccess and by the background watchdog to flip
// /health to degraded within vlmHealthyWindow when the VLM is broken.
func vlmProbe(ctx context.Context) error {
	if tinfoilVLM == nil {
		return fmt.Errorf("VLM client not initialized")
	}
	probeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	_, err := vlmCall(probeCtx, probePNGBase64, "Reply with the single word OK.", 8)
	return err
}

// vlmWatchdog runs forever, probing the VLM at vlmProbeInterval. Failures
// are logged but never crash the router — when the upstream enclave comes
// back, /health flips healthy on the next successful probe.
func vlmWatchdog(ctx context.Context) {
	if vlmProbeInterval <= 0 {
		return
	}
	t := time.NewTicker(vlmProbeInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := vlmProbe(ctx); err != nil {
			slog.Warn("vlm probe failed", "err", err,
				"healthy", vlmHealthy(), "model", vlmModel)
		}
	}
}
