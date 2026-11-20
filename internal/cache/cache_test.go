package cache

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

var bg = context.Background()

func TestStampedeSingleInstance_1000Requests_OneDBQuery(t *testing.T) {
	be := newFake()
	c := newTestCache(t, be, nil)
	l := &loader{delay: 100 * time.Millisecond}

	vals, errs := burst(1000, func(int) ([]byte, error) { return c.Fetch(bg, "product", "p1", "", l.load) })

	if got := l.calls.Load(); got != 1 {
		t.Fatalf("expected exactly 1 database query for 1000 requests, got %d", got)
	}
	for i := range vals {
		if errs[i] != nil || string(vals[i]) != "v1" {
			t.Fatalf("request %d: %q %v", i, vals[i], errs[i])
		}
	}
	if c.Stats.Coalesced.Load() == 0 {
		t.Fatal("expected coalesced requests (singleflight)")
	}
	if be.raw(c.mustTarget(t, "product", "p1").dataKey) == nil {
		t.Fatal("value should have been written to Redis")
	}
}

func TestStampedeAcrossFiveInstances_OneDBQuery(t *testing.T) {
	be := newFake() // one shared Redis, five separate API instances
	l := &loader{delay: 150 * time.Millisecond}
	caches := make([]*Cache, 5)
	for i := range caches {
		caches[i] = newTestCache(t, be, nil)
	}
	vals, errs := burst(1000, func(i int) ([]byte, error) {
		return caches[i%5].Fetch(bg, "product", "p1", "", l.load)
	})
	if got := l.calls.Load(); got != 1 {
		t.Fatalf("expected 1 database query across 5 instances, got %d", got)
	}
	for i := range vals {
		if errs[i] != nil || string(vals[i]) != "v1" {
			t.Fatalf("request %d: %q %v", i, vals[i], errs[i])
		}
	}
	var contended int64
	for _, c := range caches {
		contended += c.Stats.LockContended.Load()
	}
	if contended == 0 {
		t.Fatal("expected other instances to wait on the lock")
	}
}

func TestLockHolderCrash_RecoversAfterExpiry(t *testing.T) {
	be := newFake()
	c := newTestCache(t, be, nil)
	tg := c.mustTarget(t, "product", "p1")
	be.locks[tg.lockKey] = fakeLock{token: "crashed-instance", exp: time.Now().Add(150 * time.Millisecond)}

	l := &loader{}
	start := time.Now()
	v, err := c.Fetch(bg, "product", "p1", "", l.load)
	if err != nil || string(v) != "v1" {
		t.Fatalf("got %q %v", v, err)
	}
	if time.Since(start) < 100*time.Millisecond {
		t.Fatal("should have waited for the dead lock to expire")
	}
	if l.calls.Load() != 1 {
		t.Fatalf("calls=%d", l.calls.Load())
	}
}

func TestRedisOutage_SameKey_NoFlood(t *testing.T) {
	be := newFake()
	be.down.Store(true)
	c := newTestCache(t, be, nil)
	l := &loader{delay: 50 * time.Millisecond}

	vals, errs := burst(1000, func(int) ([]byte, error) { return c.Fetch(bg, "product", "p1", "", l.load) })
	for i := range vals {
		if errs[i] != nil || string(vals[i]) != "v1" {
			t.Fatalf("request %d: %q %v", i, vals[i], errs[i])
		}
	}
	if got := l.calls.Load(); got != 1 {
		t.Fatalf("Redis outage must not multiply database queries: got %d", got)
	}
}

func TestRedisOutage_ManyKeys_BoundedConcurrentLoads(t *testing.T) {
	be := newFake()
	be.down.Store(true)
	c := newTestCache(t, be, func(cfg *Config) { cfg.MaxLoads = 4 })
	l := &loader{delay: 100 * time.Millisecond}

	_, errs := burst(200, func(i int) ([]byte, error) {
		return c.Fetch(bg, "product", fmt.Sprintf("p%d", i), "", l.load)
	})
	if p := l.peak.Load(); p > 4 {
		t.Fatalf("concurrent database loads must stay <= 4, peak was %d", p)
	}
	var busy, ok int
	for _, e := range errs {
		switch {
		case e == nil:
			ok++
		case errors.Is(e, ErrBusy):
			busy++
		default:
			t.Fatalf("unexpected error %v", e)
		}
	}
	if busy == 0 || ok == 0 {
		t.Fatalf("expected a mix of served and shed requests: ok=%d busy=%d", ok, busy)
	}
}

func TestSlowDatabase_TimesOutAndReleasesLock(t *testing.T) {
	be := newFake()
	c := newTestCache(t, be, func(cfg *Config) { cfg.LoadTimeout = 50 * time.Millisecond })
	slow := &loader{delay: 2 * time.Second}

	_, err := c.Fetch(bg, "product", "p1", "", slow.load)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline exceeded, got %v", err)
	}
	if owner := be.lockOwner(c.mustTarget(t, "product", "p1").lockKey); owner != "" {
		t.Fatalf("lock must be released after a failed load, owner=%q", owner)
	}
	fast := &loader{}
	if v, err := c.Fetch(bg, "product", "p1", "", fast.load); err != nil || string(v) != "v1" {
		t.Fatalf("recovery failed: %q %v", v, err)
	}
}

func TestConcurrentUpdate_StaleWriteIsDropped(t *testing.T) {
	be := newFake()
	c := newTestCache(t, be, nil)
	entered := make(chan struct{})
	release := make(chan struct{})
	oldLoad := func(ctx context.Context) ([]byte, error) {
		close(entered)
		<-release
		return []byte("old-price"), nil
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := c.Fetch(bg, "product", "p1", "product:p1", oldLoad); err != nil {
			t.Error(err)
		}
	}()
	<-entered
	// The seller edits the product while the loader still holds old data.
	if err := c.Invalidate(bg, "product:p1"); err != nil {
		t.Fatal(err)
	}
	close(release)
	<-done

	if be.raw(c.mustTarget(t, "product", "p1").dataKey) != nil {
		t.Fatal("stale data must not be written to Redis after an invalidation")
	}
	if c.Stats.StaleWritesDrop.Load() != 1 {
		t.Fatalf("StaleWritesDrop=%d", c.Stats.StaleWritesDrop.Load())
	}
	fresh := &loader{value: "new-price"}
	if v, _ := c.Fetch(bg, "product", "p1", "product:p1", fresh.load); string(v) != "new-price" {
		t.Fatalf("got %q", v)
	}
}

func TestInvalidateMakesCachedEntryAMiss(t *testing.T) {
	be := newFake()
	c := newTestCache(t, be, nil)
	l := &loader{}
	v1, _ := c.Fetch(bg, "product", "p1", "product:p1", l.load)
	v1b, _ := c.Fetch(bg, "product", "p1", "product:p1", l.load)
	if string(v1) != "v1" || string(v1b) != "v1" || l.calls.Load() != 1 {
		t.Fatalf("second read should be a hit: %q %q calls=%d", v1, v1b, l.calls.Load())
	}
	if err := c.Invalidate(bg, "product:p1"); err != nil {
		t.Fatal(err)
	}
	if v, _ := c.Fetch(bg, "product", "p1", "product:p1", l.load); string(v) != "v2" {
		t.Fatalf("after invalidation want fresh load, got %q", v)
	}
}

func TestFailedPopulation_StillServesCaller(t *testing.T) {
	be := newFake()
	be.failSet.Store(true)
	c := newTestCache(t, be, nil)
	l := &loader{}
	v, err := c.Fetch(bg, "product", "p1", "", l.load)
	if err != nil || string(v) != "v1" {
		t.Fatalf("got %q %v", v, err)
	}
	if c.Stats.PopulateErrors.Load() != 1 {
		t.Fatalf("PopulateErrors=%d", c.Stats.PopulateErrors.Load())
	}
}

func TestNegativeResultIsCachedBriefly(t *testing.T) {
	be := newFake()
	c := newTestCache(t, be, nil)
	var calls atomic.Int64
	missing := func(context.Context) ([]byte, error) { calls.Add(1); return nil, ErrNotFound }
	for i := 0; i < 5; i++ {
		if _, err := c.Fetch(bg, "product", "ghost", "", missing); !errors.Is(err, ErrNotFound) {
			t.Fatalf("want ErrNotFound, got %v", err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("nonexistent product must hit the database once, got %d", calls.Load())
	}
	if c.Stats.NegativeHits.Load() != 4 {
		t.Fatalf("NegativeHits=%d", c.Stats.NegativeHits.Load())
	}
}

func TestLockRelease_IsOwnershipSafe(t *testing.T) {
	be := newFake()
	c := newTestCache(t, be, func(cfg *Config) { cfg.LockTTL = 40 * time.Millisecond })
	lockKey := c.mustTarget(t, "product", "p1").lockKey
	slow := func(ctx context.Context) ([]byte, error) {
		time.Sleep(80 * time.Millisecond) // our lock has expired by now
		if ok, _ := be.TryLock(ctx, lockKey, "other-instance", 5*time.Second); !ok {
			t.Error("test setup: other instance should get the expired lock")
		}
		time.Sleep(20 * time.Millisecond)
		return []byte("x"), nil
	}
	if _, err := c.Fetch(bg, "product", "p1", "", slow); err != nil {
		t.Fatal(err)
	}
	if owner := be.lockOwner(lockKey); owner != "other-instance" {
		t.Fatalf("a finishing loader must not delete another instance's lock, owner=%q", owner)
	}
}

func TestWaiterLimit_ShedsExcessRequests(t *testing.T) {
	be := newFake()
	c := newTestCache(t, be, func(cfg *Config) { cfg.MaxWaiters = 10 })
	l := &loader{delay: 300 * time.Millisecond}
	_, errs := burst(60, func(int) ([]byte, error) { return c.Fetch(bg, "product", "p1", "", l.load) })
	var busy, ok int
	for _, e := range errs {
		if errors.Is(e, ErrBusy) {
			busy++
		} else if e == nil {
			ok++
		} else {
			t.Fatal(e)
		}
	}
	if busy == 0 || ok+busy != 60 {
		t.Fatalf("ok=%d busy=%d", ok, busy)
	}
	if l.calls.Load() != 1 {
		t.Fatalf("calls=%d", l.calls.Load())
	}
}

func TestCancelledWaiterReturnsPromptly_LoadContinues(t *testing.T) {
	be := newFake()
	c := newTestCache(t, be, nil)
	l := &loader{delay: 300 * time.Millisecond}
	ctx, cancel := context.WithTimeout(bg, 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.Fetch(ctx, "product", "p1", "", l.load)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 200*time.Millisecond {
		t.Fatalf("err=%v after %v", err, time.Since(start))
	}
	v, err := c.Fetch(bg, "product", "p1", "", l.load)
	if err != nil || string(v) != "v1" || l.calls.Load() != 1 {
		t.Fatalf("shared load should have survived the cancelled waiter: %q %v calls=%d", v, err, l.calls.Load())
	}
}

func TestStaleWhileRevalidate_ServesStaleAndRefreshesOnce(t *testing.T) {
	be := newFake()
	c := newTestCache(t, be, nil)
	l := &loader{}
	if v, _ := c.Fetch(bg, "page", "home", "", l.load); string(v) != "v1" {
		t.Fatalf("got %q", v)
	}
	time.Sleep(40 * time.Millisecond) // past SoftTTL
	if v, _ := c.Fetch(bg, "page", "home", "", l.load); string(v) != "v1" {
		t.Fatalf("stale value should be served immediately, got %q", v)
	}
	deadline := time.Now().Add(2 * time.Second)
	for l.calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if l.calls.Load() != 2 {
		t.Fatalf("expected one background refresh, calls=%d", l.calls.Load())
	}
	if c.Stats.StaleServed.Load() < 1 {
		t.Fatal("StaleServed not counted")
	}
}

func TestRedisOutageAtInvalidate_IsRetried(t *testing.T) {
	be := newFake()
	c := newTestCache(t, be, nil)
	be.down.Store(true)
	if err := c.Invalidate(bg, "product:p1"); err == nil {
		t.Fatal("expected an error while Redis is down")
	}
	be.down.Store(false)
	deadline := time.Now().Add(3 * time.Second)
	for be.version(c.cfg.Prefix+"v:product:p1") == "" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if be.version(c.cfg.Prefix+"v:product:p1") == "" {
		t.Fatal("invalidation was not retried after Redis came back")
	}
}

func TestUnregisteredNamespaceCannotBeCached(t *testing.T) {
	c := newTestCache(t, newFake(), nil)
	l := &loader{}
	_, err := c.Fetch(bg, "checkout", "cart-1", "", l.load)
	if !errors.Is(err, ErrUnknownNamespace) || l.calls.Load() != 0 {
		t.Fatalf("checkout data must not be cacheable: err=%v calls=%d", err, l.calls.Load())
	}
}

func TestScopedKeyNeverSharedBetweenSellers(t *testing.T) {
	a, _ := ScopedKey("sellerA", "products")
	b, _ := ScopedKey("sellerB", "products")
	if a == b {
		t.Fatal("different sellers produced the same key")
	}
	if _, err := ScopedKey("", "products"); err == nil {
		t.Fatal("empty seller id must be rejected")
	}
	if _, err := ScopedKey("a:b", "x"); err == nil {
		t.Fatal("separator in seller id must be rejected")
	}
}

func TestTTLJitterSpreadsExpiry(t *testing.T) {
	seen := map[time.Duration]bool{}
	for i := 0; i < 1000; i++ {
		d := jitter(time.Minute, 0.1)
		if d < 54*time.Second || d > 66*time.Second {
			t.Fatalf("jitter out of range: %v", d)
		}
		seen[d] = true
	}
	if len(seen) < 100 {
		t.Fatalf("jitter not spreading: %d distinct values", len(seen))
	}
}

func TestOversizeValueIsNotCached(t *testing.T) {
	be := newFake()
	c := newTestCache(t, be, func(cfg *Config) { cfg.MaxValueBytes = 4 })
	l := &loader{value: "much-too-large"}
	if v, err := c.Fetch(bg, "product", "p1", "", l.load); err != nil || string(v) != "much-too-large" {
		t.Fatalf("%q %v", v, err)
	}
	if c.Stats.OversizeSkipped.Load() != 1 || be.raw(c.mustTarget(t, "product", "p1").dataKey) != nil {
		t.Fatal("oversize value must be served but not stored")
	}
}

func (c *Cache) mustTarget(t *testing.T, ns, key string) target {
	t.Helper()
	tg, err := c.target(ns, key, "")
	if err != nil {
		t.Fatal(err)
	}
	return tg
}
