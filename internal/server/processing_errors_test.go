package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/tinfoilsh/confidential-doc-upload/internal/processing"
)

const privateErrorFixture = "private@example.test /secret/customer.pdf secret-token"

func uploadRequest(t *testing.T, mode string) *http.Request {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("files", privateErrorFixture+".pdf")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(part, privateErrorFixture); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/convert/file?mode="+mode, &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	return request
}

func TestUploadErrorsAreClassifiedWithoutPrivateData(t *testing.T) {
	original := parserHTTPClient
	originalLogger := slog.Default()
	t.Cleanup(func() {
		parserHTTPClient = original
		slog.SetDefault(originalLogger)
	})
	tests := []struct {
		name, mode, body string
		status           int
		transportError   error
		want             processing.Code
	}{
		{"legacy extraction rejection", "raw", privateErrorFixture, 422, nil, processing.ExtractionFailed},
		{"render rejection", "images", privateErrorFixture, 422, nil, processing.RenderFailed},
		{"timeout", "raw", `{"code":"document_parser_timeout","message":"` + privateErrorFixture + `"}`, 422, nil, processing.ParserTimeout},
		{"output limit", "images", `{"code":"document_parser_output_limit"}`, 422, nil, processing.ParserOutputLimit},
		{"empty", "raw", `{"code":"document_empty"}`, 422, nil, processing.Empty},
		{"parser process failure", "images", `{"code":"document_parser_failed"}`, 422, nil, processing.RenderFailed},
		{"untrusted code", "raw", `{"code":"` + privateErrorFixture + `"}`, 422, nil, processing.ExtractionFailed},
		{"wrong subsystem code", "raw", `{"code":"document_ocr_failed"}`, 422, nil, processing.ExtractionFailed},
		{"invalid parser json", "raw", privateErrorFixture, 200, nil, processing.ParserInvalidResponse},
		{"parser unavailable", "raw", privateErrorFixture, 503, nil, processing.ParserUnavailable},
		{"transport failure", "raw", "", 0, errors.New(privateErrorFixture), processing.ParserUnavailable},
		{"transport timeout", "raw", "", 0, context.DeadlineExceeded, processing.ParserTimeout},
		{"cancellation", "raw", "", 0, context.Canceled, processing.Canceled},
		{"oversized error", "raw", strings.Repeat(privateErrorFixture, maxParserErrorBytes), 422, nil, processing.ParserOutputLimit},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
			parserHTTPClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				if tc.mode == "images" && request.URL.Path == "/v1/extract" {
					return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"format":"pdf","pages":[{"page":1,"text":"public","is_scanned":false}],"page_count":1}`))}, nil
				}
				if tc.transportError != nil {
					return nil, tc.transportError
				}
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			})}
			recorder := httptest.NewRecorder()
			handleConvert(recorder, uploadRequest(t, tc.mode))
			assertProcessingResponse(t, recorder, http.StatusBadGateway, tc.want)
			if strings.Contains(logs.String(), privateErrorFixture) {
				t.Fatalf("private data reached logs: %s", logs.String())
			}
			if !strings.Contains(logs.String(), string(tc.want)) {
				t.Fatal("safe failure category missing from logs")
			}
		})
	}
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, family := range families {
		if family.GetName() != "router_document_processing_errors_total" {
			continue
		}
		found = true
		for _, metric := range family.Metric {
			if len(metric.Label) != 1 || metric.Label[0].GetName() != "code" {
				t.Fatal("unexpected document metric labels")
			}
			code := processing.Code(metric.Label[0].GetValue())
			if processing.Normalize(code) != code {
				t.Fatalf("unbounded error label: %q", code)
			}
		}
	}
	if !found {
		t.Fatal("processing error metric was not emitted")
	}
}

func assertProcessingResponse(t *testing.T, recorder *httptest.ResponseRecorder, status int, code processing.Code) {
	t.Helper()
	if recorder.Code != status {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, status, recorder.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["code"] != string(code) || body["message"] != processing.Message(code) || body["error"] != body["message"] || body["object"] != "error" || body["param"] != nil {
		t.Fatalf("unexpected error envelope: %s", recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), privateErrorFixture) {
		t.Fatal("private data reached response")
	}
}

func TestParserBusyPreservesRetryWithoutReadingPrivateBody(t *testing.T) {
	original := parserHTTPClient
	t.Cleanup(func() { parserHTTPClient = original })
	for _, retry := range []string{"5", "-1", "99999", "private"} {
		parserHTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusTooManyRequests,
				Header: http.Header{"Retry-After": {retry}}, Body: io.NopCloser(errorReader{errors.New(privateErrorFixture)})}, nil
		})}
		recorder := httptest.NewRecorder()
		handleConvert(recorder, uploadRequest(t, "raw"))
		assertProcessingResponse(t, recorder, http.StatusTooManyRequests, processing.ParserBusy)
		want := ""
		if retry == "5" {
			want = retry
		}
		if got := recorder.Header().Get("Retry-After"); got != want {
			t.Fatalf("Retry-After = %q, want %q", got, want)
		}
	}
}

func TestSuccessfulConversionContractIsUnchanged(t *testing.T) {
	original := parserHTTPClient
	t.Cleanup(func() { parserHTTPClient = original })
	parserHTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"format":"text","md_content":"expected document text"}`))}, nil
	})}
	recorder := httptest.NewRecorder()
	handleConvert(recorder, uploadRequest(t, "raw"))
	var result struct {
		Status   string `json:"status"`
		Document struct {
			Content string `json:"md_content"`
		} `json:"document"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != http.StatusOK || result.Status != "success" || result.Document.Content != "expected document text" {
		t.Fatalf("successful conversion changed: %s", recorder.Body.String())
	}
}
