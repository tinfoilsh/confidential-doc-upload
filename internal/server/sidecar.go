package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type ExtractPage struct {
	Page      int    `json:"page"`
	Text      string `json:"text"`
	MDContent string `json:"md_content,omitempty"`
	IsScanned bool   `json:"is_scanned"`
}

type ExtractResult struct {
	Format    string        `json:"format"`
	Pages     []ExtractPage `json:"pages"`
	MDContent string        `json:"md_content"`
	PageCount int           `json:"page_count"`
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

func isPDF(filename string) bool {
	return strings.HasSuffix(strings.ToLower(filename), ".pdf")
}

const pdfParserBin = "pdfparser"

// parsePDF runs the pdfparser binary in a sandboxed child process.
func parsePDF(ctx context.Context, data []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, pdfParserBin, args...)
	cmd.Stdin = bytes.NewReader(data)
	cmd.Env = []string{} // empty env: no secrets
	cmd.Dir = os.TempDir()
	// CLONE_NEWNET requires CAP_SYS_ADMIN; enable when running privileged
	// cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWNET}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		slog.Error("parser subprocess failed", "err", err, "stderr", stderr.String())
		return nil, fmt.Errorf("parser: %w: %s", err, stderr.String())
	}
	return stdout.Bytes(), nil
}

func sidecarExtract(ctx context.Context, data []byte, filename string) (ExtractResult, error) {
	if isPDF(filename) {
		return pdfExtract(ctx, data)
	}
	return sidecarExtractHTTP(ctx, data, filename)
}

func sidecarRender(ctx context.Context, data []byte, filename string, dpi int) (RenderResult, error) {
	if isPDF(filename) {
		return pdfRender(ctx, data, dpi)
	}
	return sidecarRenderHTTP(ctx, data, filename, dpi)
}

func pdfExtract(ctx context.Context, data []byte) (ExtractResult, error) {
	body, err := parsePDF(ctx, data)
	if err != nil {
		return ExtractResult{}, err
	}

	var parsed struct {
		Format    string `json:"format"`
		PageCount int    `json:"page_count"`
		Pages     []struct {
			Page      int    `json:"page"`
			MDContent string `json:"md_content"`
			IsScanned bool   `json:"is_scanned"`
		} `json:"pages"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return ExtractResult{}, fmt.Errorf("parse output: %w", err)
	}

	result := ExtractResult{
		Format:    "pdf",
		PageCount: parsed.PageCount,
	}
	for _, p := range parsed.Pages {
		result.Pages = append(result.Pages, ExtractPage{
			Page:      p.Page,
			Text:      p.MDContent,
			MDContent: p.MDContent,
			IsScanned: p.IsScanned,
		})
	}
	return result, nil
}

func pdfRender(ctx context.Context, data []byte, dpi int) (RenderResult, error) {
	body, err := parsePDF(ctx, data, "--render", fmt.Sprintf("--dpi=%d", dpi))
	if err != nil {
		return RenderResult{}, err
	}

	var parsed struct {
		PageCount int `json:"page_count"`
		Pages     []struct {
			Page  int    `json:"page"`
			Image string `json:"image"`
		} `json:"pages"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return RenderResult{}, fmt.Errorf("parse output: %w", err)
	}

	result := RenderResult{PageCount: parsed.PageCount}
	for _, p := range parsed.Pages {
		result.Pages = append(result.Pages, RenderPage{
			Page:  p.Page,
			Image: p.Image,
		})
	}
	return result, nil
}

// sidecarExtractHTTP falls back to the Python sidecar for non-PDF formats.
func sidecarExtractHTTP(ctx context.Context, data []byte, filename string) (ExtractResult, error) {
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

func sidecarRenderHTTP(ctx context.Context, data []byte, filename string, dpi int) (RenderResult, error) {
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

func sidecarPost(ctx context.Context, endpoint string, data []byte, filename string, extraFields map[string]string) ([]byte, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

	fw, err := mw.CreateFormFile("file", filepath.Base(filename))
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

	const maxResponseBytes = 512 * 1024 * 1024
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("sidecar %s read: %w", endpoint, err)
	}
	if len(body) > maxResponseBytes {
		return nil, fmt.Errorf("sidecar %s: response too large (>512MB)", endpoint)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("sidecar %s returned %d: %s", endpoint, resp.StatusCode, truncate(string(body), 256))
	}
	return body, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
