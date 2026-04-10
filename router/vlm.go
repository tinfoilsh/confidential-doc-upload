package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
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

func vlmCall(ctx context.Context, url, model, apiKey, imageB64, prompt string, maxTokens int) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"model": model,
		"messages": []map[string]any{{
			"role": "user",
			"content": []map[string]any{
				{"type": "image_url", "image_url": map[string]string{"url": "data:image/png;base64," + imageB64}},
				{"type": "text", "text": prompt},
			},
		}},
		"max_tokens": maxTokens,
	})

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("vlm: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("vlm %d: %s", resp.StatusCode, truncate(string(respBody), 256))
	}

	var r struct {
		Choices []struct {
			Message struct{ Content string } `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respBody, &r); err != nil || len(r.Choices) == 0 {
		return "", fmt.Errorf("vlm: bad response")
	}

	md := strings.TrimSpace(r.Choices[0].Message.Content)
	if strings.HasPrefix(md, "```") {
		if i := strings.Index(md, "\n"); i >= 0 {
			md = md[i+1:]
		}
	}
	md = strings.TrimSuffix(strings.TrimRight(md, "\n"), "```")
	return md, nil
}

func vlmFullPageOCR(ctx context.Context, imageB64 string) (string, error) {
	return vlmCall(ctx,
		vllmURL+"/v1/chat/completions", vllmModel, "",
		imageB64, vlmOCRPrompt, 8000)
}

func vlmVisualExtract(ctx context.Context, imageB64 string) (string, error) {
	if gemmaURL != "" {
		// Remote Gemma on a Tinfoil enclave. The request goes over HTTPS with
		// an API key; the remote enclave's shim validates the key and handles
		// attestation. For full mutual attestation (verifying the remote
		// enclave's identity from this side), add tinfoil-go as a dependency
		// and use verifier/client.TLSBoundRoundTripper as the HTTP transport.
		// TODO: add tinfoil-go TLSBoundRoundTripper for enclave-to-enclave calls
		return vlmCall(ctx,
			gemmaURL+"/v1/chat/completions", gemmaModel, gemmaKey,
			imageB64, vlmVisualPrompt, 4000)
	}
	// Fallback: local Qwen
	return vlmCall(ctx,
		vllmURL+"/v1/chat/completions", vllmModel, "",
		imageB64, vlmVisualPrompt, 4000)
}

type vlmPageFunc func(ctx context.Context, imageB64 string) (string, error)

func vlmParallel(ctx context.Context, images map[int]string, fn vlmPageFunc) (map[int]string, error) {
	results := make(map[int]string)
	var mu sync.Mutex
	var firstErr error

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	sem := make(chan struct{}, maxParallel)

	for idx, img := range images {
		wg.Add(1)
		go func(pageIdx int, b64 string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}

			md, err := fn(ctx, b64)
			mu.Lock()
			if err != nil && firstErr == nil {
				firstErr = fmt.Errorf("page %d: %w", pageIdx, err)
				cancel()
			} else if err == nil {
				results[pageIdx] = md
			}
			mu.Unlock()
		}(idx, img)
	}
	wg.Wait()

	if firstErr != nil {
		return nil, firstErr
	}
	return results, nil
}
