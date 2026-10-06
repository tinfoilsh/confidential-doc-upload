package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/tinfoilsh/confidential-doc-upload/internal/processing"
	"github.com/tinfoilsh/tinfoil-go"
)

func TestVLMParallelMixedUsesServiceWideGate(t *testing.T) {
	originalGate := vlmGate
	vlmGate = make(chan struct{}, 2)
	t.Cleanup(func() { vlmGate = originalGate })

	var active atomic.Int32
	var maximum atomic.Int32
	started := make(chan struct{}, 4)
	release := make(chan struct{})
	work := make(map[int]vlmWorkItem)
	for index := range 4 {
		work[index] = vlmWorkItem{fn: func(context.Context, string) (string, error) {
			current := active.Add(1)
			for {
				previous := maximum.Load()
				if current <= previous || maximum.CompareAndSwap(previous, current) {
					break
				}
			}
			started <- struct{}{}
			<-release
			active.Add(-1)
			return "ok", nil
		}}
	}

	done := make(chan struct{})
	go func() {
		vlmParallelMixed(context.Background(), work)
		close(done)
	}()

	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("VLM workers did not fill the service-wide gate")
		}
	}
	select {
	case <-started:
		t.Fatal("more VLM calls started than the service-wide budget")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("VLM work did not finish after releasing the gate")
	}
	if got := maximum.Load(); got != 2 {
		t.Fatalf("maximum VLM concurrency = %d, want 2", got)
	}
}

func TestVLMErrorsDoNotExposeUpstreamDetails(t *testing.T) {
	originalClient := tinfoilVLM.Load()
	originalHealth := vlmHealth.Load()
	originalSince := unhealthySince.Load()
	t.Cleanup(func() {
		tinfoilVLM.Store(originalClient)
		vlmHealth.Store(originalHealth)
		unhealthySince.Store(originalSince)
	})
	for _, tc := range []struct {
		name   string
		status int
		body   string
		err    error
		want   processing.Code
	}{
		{"client rejection", 400, `{"error":{"message":"` + privateErrorFixture + `"}}`, nil, processing.OCRFailed},
		{"auth", 401, privateErrorFixture, nil, processing.OCRUnavailable},
		{"permission", 403, privateErrorFixture, nil, processing.OCRUnavailable},
		{"server", 500, privateErrorFixture, nil, processing.OCRUnavailable},
		{"empty choices", 200, `{"choices":[]}`, nil, processing.OCRFailed},
		{"transport", 0, "", errors.New(privateErrorFixture), processing.OCRUnavailable},
		{"timeout", 0, "", context.DeadlineExceeded, processing.OCRTimeout},
		{"canceled", 0, "", context.Canceled, processing.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			unhealthySince.Store(0)
			client := openai.NewClient(option.WithAPIKey("test-key"), option.WithMaxRetries(0),
				option.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
					if tc.err != nil {
						return nil, tc.err
					}
					return &http.Response{StatusCode: tc.status, Request: request,
						Header: http.Header{"Content-Type": {"application/json"}},
						Body:   io.NopCloser(strings.NewReader(tc.body))}, nil
				})}))
			tinfoilVLM.Store(&tinfoil.Client{Client: &client})
			_, err := vlmCall(context.Background(), "ocr", "test-image", "test-prompt", 1)
			if err == nil || processing.CodeOf(err) != tc.want {
				t.Fatalf("VLM error = %v, want %s", err, tc.want)
			}
			if strings.Contains(err.Error(), privateErrorFixture) {
				t.Fatal("upstream error text exposed")
			}
			recorder := httptest.NewRecorder()
			writeProcessingError(recorder, http.StatusBadGateway, processing.CodeOf(err))
			assertProcessingResponse(t, recorder, http.StatusBadGateway, tc.want)
		})
	}
	unhealthySince.Store(0)
	tinfoilVLM.Store(nil)
	_, err := vlmCall(context.Background(), "ocr", "test-image", "test-prompt", 1)
	if processing.CodeOf(err) != processing.OCRUnavailable {
		t.Fatalf("uninitialized client: %v", err)
	}
}
