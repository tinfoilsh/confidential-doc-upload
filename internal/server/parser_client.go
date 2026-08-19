package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"
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
}

type RenderPage struct {
	Page  int    `json:"page"`
	Image string `json:"image"`
}

type RenderResult struct {
	Pages     []RenderPage `json:"pages"`
	PageCount int          `json:"page_count"`
}

type parserResponseError struct {
	StatusCode        int
	RetryAfterSeconds int
	Message           string
}

func (responseError *parserResponseError) Error() string {
	return fmt.Sprintf("parser returned %d: %s", responseError.StatusCode, responseError.Message)
}

var parserSocketPath = envOr("PARSER_SOCKET", "/run/docparser/parser.sock")

var parserHTTPClient = &http.Client{
	Timeout:   310 * time.Second,
	Transport: parserTransport(8),
}

// Health probes use an independent connection budget, so queued conversions
// cannot make a healthy broker appear unavailable under load.
var parserHealthClient = &http.Client{
	Timeout:   2 * time.Second,
	Transport: parserTransport(1),
}

func parserTransport(maxConnections int) *http.Transport {
	return &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "unix", parserSocketPath)
		},
		MaxConnsPerHost:     maxConnections,
		MaxIdleConnsPerHost: 2,
		IdleConnTimeout:     30 * time.Second,
	}
}

func sidecarExtract(ctx context.Context, data []byte, filename string) (ExtractResult, error) {
	body, err := parserPost(ctx, "/v1/extract", data, filename)
	if err != nil {
		return ExtractResult{}, err
	}
	var result ExtractResult
	if err := json.Unmarshal(body, &result); err != nil {
		return ExtractResult{}, fmt.Errorf("decode parser extraction: %w", err)
	}
	normalizeExtractResult(&result)
	return result, nil
}

// The Python document parsers return Markdown as md_content, while the PDF
// parser returns plain text in text. Normalize both onto Text for the existing
// conversion pipeline without discarding the richer Markdown field.
func normalizeExtractResult(result *ExtractResult) {
	for index := range result.Pages {
		if result.Pages[index].Text == "" {
			result.Pages[index].Text = result.Pages[index].MDContent
		}
	}
}

func sidecarRender(ctx context.Context, data []byte, filename string, dpi int) (RenderResult, error) {
	body, err := parserPost(ctx, "/v1/render?dpi="+url.QueryEscape(strconv.Itoa(dpi)), data, filename)
	if err != nil {
		return RenderResult{}, err
	}
	var result RenderResult
	if err := json.Unmarshal(body, &result); err != nil {
		return RenderResult{}, fmt.Errorf("decode parser render: %w", err)
	}
	return result, nil
}

func parserPost(ctx context.Context, endpoint string, data []byte, filename string) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://parser"+endpoint, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/octet-stream")
	request.Header.Set("X-Document-Name", base64.RawURLEncoding.EncodeToString([]byte(filename)))

	response, err := parserHTTPClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("parser request: %w", err)
	}
	defer response.Body.Close()

	maxResponseBytes := int64(boundedEnvInt("PARSER_MAX_OUTPUT_MB", 256, 1, 256)) * 1024 * 1024
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read parser response: %w", err)
	}
	if int64(len(body)) > maxResponseBytes {
		return nil, fmt.Errorf("parser response exceeds %d MiB", maxResponseBytes/(1024*1024))
	}
	if response.StatusCode != http.StatusOK {
		retryAfter, _ := strconv.Atoi(response.Header.Get("Retry-After"))
		if retryAfter < 1 || retryAfter > 60 {
			retryAfter = 0
		}
		return nil, &parserResponseError{
			StatusCode:        response.StatusCode,
			RetryAfterSeconds: retryAfter,
			Message:           truncate(string(body), 256),
		}
	}
	return body, nil
}

func parserHealthy() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://parser/health", nil)
	if err != nil {
		return false
	}
	response, err := parserHealthClient.Do(request)
	if err != nil {
		return false
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1024))
	_ = response.Body.Close()
	return response.StatusCode == http.StatusOK
}

func truncate(value string, maximum int) string {
	if len(value) <= maximum {
		return value
	}
	return value[:maximum]
}
