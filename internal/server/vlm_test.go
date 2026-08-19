package server

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
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
