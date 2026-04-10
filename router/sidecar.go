package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
)

type ExtractPage struct {
	Page      int    `json:"page"`
	Text      string `json:"text"`
	IsScanned bool   `json:"is_scanned"`
}

type ExtractResult struct {
	Format    string        `json:"format"`
	Pages     []ExtractPage `json:"pages"`
	MDContent string        `json:"md_content"`
	ElapsedS  float64       `json:"elapsed_s"`
}

type RenderPage struct {
	Page  int    `json:"page"`
	Image string `json:"image"`
}

type RenderResult struct {
	Pages     []RenderPage `json:"pages"`
	PageCount int          `json:"page_count"`
	ElapsedS  float64      `json:"elapsed_s"`
}

func sidecarPost(ctx context.Context, endpoint string, data []byte, filename string, extraFields map[string]string) ([]byte, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		return nil, err
	}
	if _, err := fw.Write(data); err != nil {
		return nil, err
	}
	for k, v := range extraFields {
		if err := mw.WriteField(k, v); err != nil {
			return nil, err
		}
	}
	mw.Close()

	req, err := http.NewRequestWithContext(ctx, "POST", sidecarURL+endpoint, &buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sidecar %s: %w", endpoint, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("sidecar %s read: %w", endpoint, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("sidecar %s returned %d: %s", endpoint, resp.StatusCode, truncate(string(body), 256))
	}
	return body, nil
}

func sidecarExtract(ctx context.Context, data []byte, filename string) (ExtractResult, error) {
	body, err := sidecarPost(ctx, "/extract", data, filename, nil)
	if err != nil {
		return ExtractResult{}, err
	}
	var result ExtractResult
	if err := json.Unmarshal(body, &result); err != nil {
		return ExtractResult{}, err
	}
	return result, nil
}

func sidecarRender(ctx context.Context, data []byte, filename string, dpi int) (RenderResult, error) {
	body, err := sidecarPost(ctx, "/render", data, filename, map[string]string{
		"dpi": fmt.Sprintf("%d", dpi),
	})
	if err != nil {
		return RenderResult{}, err
	}
	var result RenderResult
	if err := json.Unmarshal(body, &result); err != nil {
		return RenderResult{}, err
	}
	return result, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
