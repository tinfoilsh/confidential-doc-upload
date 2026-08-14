package server

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (roundTrip roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
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
