package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (roundTrip roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

func TestParserPostPreservesBackpressure(t *testing.T) {
	originalWork := parserHTTPClient
	t.Cleanup(func() { parserHTTPClient = originalWork })

	parserHTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Header:     http.Header{"Retry-After": []string{"5"}},
			Body:       io.NopCloser(strings.NewReader("parser busy")),
		}, nil
	})}

	_, err := parserPost(context.Background(), "/v1/extract", []byte("document"), "document.pdf")
	var responseError *parserResponseError
	if !errors.As(err, &responseError) {
		t.Fatalf("parserPost() error = %v, want parserResponseError", err)
	}
	if responseError.StatusCode != http.StatusTooManyRequests || responseError.RetryAfterSeconds != 5 {
		t.Fatalf("parser response metadata = %#v", responseError)
	}
}

func TestParserHealthUsesIndependentClient(t *testing.T) {
	originalWork := parserHTTPClient
	originalHealth := parserHealthClient
	t.Cleanup(func() {
		parserHTTPClient = originalWork
		parserHealthClient = originalHealth
	})

	parserHTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("health probe used the conversion client")
		return nil, nil
	})}
	parserHealthClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"status":"ok"}`)),
		}, nil
	})}

	if !parserHealthy() {
		t.Fatal("parserHealthy() rejected successful independent probe")
	}
}
