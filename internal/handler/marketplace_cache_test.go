package handler

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"shopbd/backend/internal/cache"
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

// versionBackend is the smallest cache.Backend that supports version tags:
// enough to model several API instances sharing one Redis.
type versionBackend struct {
	mu   sync.Mutex
	vers map[string]string
	n    int
	down bool
}

func (v *versionBackend) MGet(_ context.Context, keys ...string) ([][]byte, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.down {
		return nil, errors.New("redis down")
	}
	out := make([][]byte, len(keys))
	for i, k := range keys {
		if s, ok := v.vers[k]; ok {
			out[i] = []byte(s)
		}
	}
	return out, nil
}
func (v *versionBackend) BumpVersion(_ context.Context, key string, _ time.Duration) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.down {
		return errors.New("redis down")
	}
	v.n++
	v.vers[key] = fmt.Sprintf("%d", v.n)
	return nil
}
func (v *versionBackend) SetIfVersion(context.Context, string, []byte, time.Duration, string, string) (bool, error) {
	return false, errors.New("unused")
}
func (v *versionBackend) TryLock(context.Context, string, string, time.Duration) (bool, error) {
	return false, errors.New("unused")
}
func (v *versionBackend) Unlock(context.Context, string, string) error { return nil }

func newInstance(be cache.Backend, calls *int32) *MarketplaceHandler {
	return &MarketplaceHandler{
		Cache:       cache.New(be, nil, cache.DefaultConfig(), nil),
		verInterval: time.Millisecond,
		build: func(context.Context) (*marketSnapshot, error) {
			atomic.AddInt32(calls, 1)
			return &marketSnapshot{builtAt: time.Now()}, nil
		},
	}
}

// A shop rename handled by instance A must show up on instance B at once, not
// when B's 30-second snapshot expires.
func TestInvalidationOnOneInstanceReachesTheOthers(t *testing.T) {
	be := &versionBackend{vers: map[string]string{}}
	var aCalls, bCalls int32
	a, b := newInstance(be, &aCalls), newInstance(be, &bCalls)
	for _, h := range []*MarketplaceHandler{a, b} {
		if _, err := h.snapshot(); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(5 * time.Millisecond)
	if _, _ = b.snapshot(); atomic.LoadInt32(&bCalls) != 1 {
		t.Fatalf("no invalidation yet: B must reuse its snapshot, builds=%d", bCalls)
	}

	a.InvalidateSnapshot()
	time.Sleep(5 * time.Millisecond)
	if _, err := b.snapshot(); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&bCalls); got != 2 {
		t.Fatalf("B should rebuild after A invalidated, builds=%d", got)
	}
	if _, _ = b.snapshot(); atomic.LoadInt32(&bCalls) != 2 {
		t.Fatal("B must not rebuild again for the same invalidation")
	}
}

func TestSnapshotKeepsServingWhenRedisIsDown(t *testing.T) {
	be := &versionBackend{vers: map[string]string{}}
	var calls int32
	h := newInstance(be, &calls)
	if _, err := h.snapshot(); err != nil {
		t.Fatal(err)
	}
	be.mu.Lock()
	be.down = true
	be.mu.Unlock()
	time.Sleep(5 * time.Millisecond)
	for i := 0; i < 10; i++ {
		if _, err := h.snapshot(); err != nil {
			t.Fatalf("a Redis outage must not break the marketplace: %v", err)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("a Redis outage must not trigger rebuilds, builds=%d", got)
	}
}
