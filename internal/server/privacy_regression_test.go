package server

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProcessingErrorsKeepPrivateDataOutOfLogs(t *testing.T) {
	const private = "private@example.test customer-content secret-token"
	originalClient, originalLogger := parserHTTPClient, slog.Default()
	t.Cleanup(func() {
		parserHTTPClient = originalClient
		slog.SetDefault(originalLogger)
	})
	for _, transportFailure := range []bool{false, true} {
		name := "parser response body"
		if transportFailure {
			name = "transport exception"
		}
		t.Run(name, func(t *testing.T) {
			var logs, body bytes.Buffer
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
			parserHTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				if transportFailure {
					return nil, errors.New(private)
				}
				return &http.Response{StatusCode: http.StatusUnprocessableEntity,
					Body: io.NopCloser(strings.NewReader(private))}, nil
			})}
			writer := multipart.NewWriter(&body)
			part, err := writer.CreateFormFile("files", "fixture.pdf")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := io.WriteString(part, "fixture"); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, "/v1/convert/file?mode=raw", &body)
			request.Header.Set("Content-Type", writer.FormDataContentType())
			recorder := httptest.NewRecorder()
			handleConvert(recorder, request)
			if recorder.Code != http.StatusBadGateway {
				t.Fatalf("processing path was not reached: status %d", recorder.Code)
			}
			if strings.Contains(logs.String(), private) || strings.Contains(recorder.Body.String(), private) {
				t.Fatal("private backend details escaped through processing logs or response")
			}
		})
	}
}
