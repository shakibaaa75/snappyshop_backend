package handler

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSnapshotIsReusedWithinTTL(t *testing.T) {
	var calls int32
	h := &MarketplaceHandler{build: func(context.Context) (*marketSnapshot, error) {
		atomic.AddInt32(&calls, 1)
		return &marketSnapshot{builtAt: time.Now()}, nil
	}}
	for i := 0; i < 5; i++ {
		if _, err := h.snapshot(); err != nil {
			t.Fatalf("snapshot: %v", err)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected 1 build within TTL, got %d", got)
	}
}

func TestSnapshotConcurrentRequestsShareOneBuild(t *testing.T) {
	var calls int32
	h := &MarketplaceHandler{build: func(context.Context) (*marketSnapshot, error) {
		atomic.AddInt32(&calls, 1)
		time.Sleep(20 * time.Millisecond)
		return &marketSnapshot{builtAt: time.Now()}, nil
	}}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := h.snapshot(); err != nil {
				t.Errorf("snapshot: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected 1 build for 50 concurrent requests, got %d", got)
	}
}

func TestSnapshotRebuildsAfterTTLAndServesStaleOnFailure(t *testing.T) {
	var calls int32
	h := &MarketplaceHandler{build: func(context.Context) (*marketSnapshot, error) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			// Already expired when stored, so the next call must rebuild.
			return &marketSnapshot{builtAt: time.Now().Add(-2 * marketSnapshotTTL)}, nil
		}
		return nil, errors.New("mongo down")
	}}
	first, err := h.snapshot()
	if err != nil {
		t.Fatalf("first snapshot: %v", err)
	}
	second, err := h.snapshot()
	if err != nil {
		t.Fatalf("expected stale snapshot instead of error, got %v", err)
	}
	if first != second {
		t.Fatal("expected the stale snapshot to be served")
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("expected a rebuild attempt after TTL, got %d builds", got)
	}
}

func TestSnapshotErrorWhenNothingCached(t *testing.T) {
	h := &MarketplaceHandler{build: func(context.Context) (*marketSnapshot, error) {
		return nil, errors.New("mongo down")
	}}
	if _, err := h.snapshot(); err == nil {
		t.Fatal("expected an error when there is no cached snapshot")
	}
}
