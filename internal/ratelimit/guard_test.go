package ratelimit

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"shopbd/backend/internal/redisx"
)

// memBucket is an in-memory BucketBackend with the same token-bucket maths as
// the Lua script, shared between Guards to model several API instances.
type memBucket struct {
	mu    sync.Mutex
	now   func() time.Time
	state map[string]*localBucket
	keys  []string
}

func newMemBucket() *memBucket {
	return &memBucket{now: time.Now, state: map[string]*localBucket{}}
}

func (m *memBucket) Take(_ context.Context, key string, spec Spec) (bool, time.Duration, error) {
	m.mu.Lock()
	m.keys = append(m.keys, key)
	m.mu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.state[key]
	now := m.now()
	if !ok {
		b = &localBucket{tokens: float64(spec.Capacity), last: now}
		m.state[key] = b
	}
	b.tokens += float64(now.Sub(b.last)) / float64(spec.Every)
	if cp := float64(spec.Capacity); b.tokens > cp {
		b.tokens = cp
	}
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0, nil
	}
	return false, time.Duration((1 - b.tokens) * float64(spec.Every)), nil
}

type failingBucket struct{ calls atomic.Int64 }

func (f *failingBucket) Take(context.Context, string, Spec) (bool, time.Duration, error) {
	f.calls.Add(1)
	return false, 0, errors.New("redis down")
}

var spec8 = Spec{Capacity: 8, Every: time.Minute}

func TestLocalBucketRefillsOverTime(t *testing.T) {
	l := newLocalBuckets(100)
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	s := Spec{Capacity: 3, Every: 10 * time.Second}
	for i := 0; i < 3; i++ {
		if ok, _, _ := l.take("k", s, now); !ok {
			t.Fatalf("burst request %d should pass", i+1)
		}
	}
	ok, retry, _ := l.take("k", s, now)
	if ok || retry <= 0 || retry > 10*time.Second {
		t.Fatalf("4th must be refused with a retry hint, ok=%v retry=%v", ok, retry)
	}
	if ok, _, _ := l.take("k", s, now.Add(10*time.Second)); !ok {
		t.Fatal("one token should have refilled after Every")
	}
	if ok, _, _ := l.take("k", s, now.Add(10*time.Second)); ok {
		t.Fatal("only one token should have refilled")
	}
}

func TestGuardSharesLimitAcrossInstances(t *testing.T) {
	shared := newMemBucket()
	a := NewGuard(shared, nil, GuardConfig{}, nil)
	b := NewGuard(shared, nil, GuardConfig{}, nil)
	allowed := 0
	for i := 0; i < 20; i++ {
		g := a
		if i%2 == 1 {
			g = b
		}
		if g.Allow(context.Background(), "login:ip:1.2.3.4", spec8).Allowed {
			allowed++
		}
	}
	if allowed != 8 {
		t.Fatalf("two instances together must admit exactly the capacity (8), got %d", allowed)
	}
}

func TestGuardRedisDown_StrictLocalFallback(t *testing.T) {
	g := NewGuard(&failingBucket{}, redisx.NewBreaker(100, time.Hour), GuardConfig{Policy: FallbackLocal, LocalDivisor: 4}, nil)
	allowed := 0
	var last Verdict
	for i := 0; i < 8; i++ {
		last = g.Allow(context.Background(), "k", spec8)
		if last.Allowed {
			allowed++
		}
	}
	if allowed != 2 { // capacity 8 / divisor 4
		t.Fatalf("strict fallback should admit 8/4=2, got %d", allowed)
	}
	if !last.Degraded || last.Unavailable {
		t.Fatalf("verdict should be degraded but not unavailable: %+v", last)
	}
	if last.RetryAfter < 3*time.Minute { // refill interval is multiplied by the divisor (4 x 1m)
		t.Fatalf("fallback must refill slower than the shared limit, retry=%v", last.RetryAfter)
	}
}

func TestGuardRedisDown_FailClosedPolicy(t *testing.T) {
	g := NewGuard(&failingBucket{}, nil, GuardConfig{Policy: FallbackClosed}, nil)
	v := g.Allow(context.Background(), "k", spec8)
	if v.Allowed || !v.Unavailable || v.RetryAfter <= 0 {
		t.Fatalf("fail-closed must refuse with 503 semantics: %+v", v)
	}
}

func TestGuardBreakerStopsCallingRedis(t *testing.T) {
	fb := &failingBucket{}
	g := NewGuard(fb, redisx.NewBreaker(2, time.Hour), GuardConfig{LocalDivisor: 1}, nil)
	for i := 0; i < 50; i++ {
		g.Allow(context.Background(), "k", spec8)
	}
	if got := fb.calls.Load(); got != 2 {
		t.Fatalf("after 2 failures the breaker should stop calls to Redis, got %d", got)
	}
	if g.Stats.BreakerSkips.Load() != 48 {
		t.Fatalf("BreakerSkips=%d", g.Stats.BreakerSkips.Load())
	}
}

func TestGuardLocalTableFull_FailsClosedNotOpen(t *testing.T) {
	g := NewGuard(&failingBucket{}, redisx.NewBreaker(100, time.Hour), GuardConfig{}, nil)
	g.local = newLocalBuckets(2)
	for _, k := range []string{"a", "b"} {
		if !g.Allow(context.Background(), k, spec8).Allowed {
			t.Fatal("first requests should pass")
		}
	}
	v := g.Allow(context.Background(), "c", spec8)
	if v.Allowed || !v.Unavailable {
		t.Fatalf("a full fallback table must refuse new keys: %+v", v)
	}
}

func TestGuardInvalidSpecNeverMeansUnlimited(t *testing.T) {
	g := NewGuard(newMemBucket(), nil, GuardConfig{}, nil)
	for _, s := range []Spec{{}, {Capacity: 0, Every: time.Second}, {Capacity: 5, Every: 0}} {
		if v := g.Allow(context.Background(), "k", s); v.Allowed {
			t.Fatalf("invalid spec %+v was allowed", s)
		}
	}
}

func TestGuardWithoutRedisUsesFullLocalSpec(t *testing.T) {
	g := NewGuard(nil, nil, GuardConfig{LocalDivisor: 4}, nil)
	n := 0
	for i := 0; i < 20; i++ {
		if g.Allow(context.Background(), "k", spec8).Allowed {
			n++
		}
	}
	if n != 8 {
		t.Fatalf("with no Redis configured the spec applies unchanged, got %d", n)
	}
}

func TestKeyedAllowDoesNotLeakEmailIntoRedis(t *testing.T) {
	mem := newMemBucket()
	k := &KeyedAllow{Guard: NewGuard(mem, nil, GuardConfig{}, nil), Name: "login-email", Spec: spec8}
	k.Allow("victim@example.com")
	if len(mem.keys) != 1 || strings.Contains(mem.keys[0], "victim") || strings.Contains(mem.keys[0], "@") {
		t.Fatalf("raw email must not be a Redis key: %v", mem.keys)
	}
}
