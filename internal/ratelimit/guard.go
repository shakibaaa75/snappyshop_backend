package ratelimit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"shopbd/backend/internal/redisx"
)

// FallbackPolicy decides what happens when Redis cannot be used.
type FallbackPolicy int

const (
	// FallbackLocal enforces a STRICT per-instance bucket. It is weaker than
	// the shared limit in one way that cannot be fixed without Redis: each
	// instance counts alone, so N instances admit up to N times the local
	// limit. The local limit is therefore tightened by LocalDivisor.
	FallbackLocal FallbackPolicy = iota
	// FallbackClosed refuses the request (503) when Redis is down.
	FallbackClosed
)

// Verdict is the outcome of one rate-limit check.
type Verdict struct {
	Allowed    bool
	RetryAfter time.Duration
	// Degraded: Redis was unavailable and the strict local fallback decided.
	Degraded bool
	// Unavailable: the request was refused because protection could not be
	// maintained (fail-closed policy or local fallback saturated). Handlers
	// answer 503, not 429.
	Unavailable bool
}

// GuardConfig tunes the fallback behaviour.
type GuardConfig struct {
	Policy FallbackPolicy
	// LocalDivisor tightens the local fallback: capacity/divisor tokens and
	// a refill interval multiplied by divisor. Set it near the number of API
	// instances. Minimum 1.
	LocalDivisor int
	// LocalMaxKeys bounds fallback memory. When full, new keys are refused
	// (fail closed) instead of silently dropping limits.
	LocalMaxKeys int
}

// GuardStats are cumulative counters, safe to read at any time.
type GuardStats struct {
	Checks       atomic.Int64
	Denied       atomic.Int64
	RedisErrors  atomic.Int64
	BreakerSkips atomic.Int64
	LocalChecks  atomic.Int64
	FailClosed   atomic.Int64
}

func (s *GuardStats) Snapshot() map[string]int64 {
	return map[string]int64{
		"checks":        s.Checks.Load(),
		"denied":        s.Denied.Load(),
		"redis_errors":  s.RedisErrors.Load(),
		"breaker_skips": s.BreakerSkips.Load(),
		"local_checks":  s.LocalChecks.Load(),
		"fail_closed":   s.FailClosed.Load(),
	}
}

// Guard applies token buckets, shared through Redis, with a controlled
// fallback. primary may be nil (Redis not configured): then the local bucket
// is the only limiter and runs at the full, un-tightened spec, which matches
// the behaviour before Redis was introduced.
type Guard struct {
	primary BucketBackend
	breaker *redisx.Breaker
	local   *localBuckets
	cfg     GuardConfig
	log     *slog.Logger
	Stats   GuardStats
}

func NewGuard(primary BucketBackend, breaker *redisx.Breaker, cfg GuardConfig, log *slog.Logger) *Guard {
	if cfg.LocalDivisor < 1 {
		cfg.LocalDivisor = 1
	}
	if cfg.LocalMaxKeys < 1000 {
		cfg.LocalMaxKeys = 50000
	}
	if breaker == nil {
		breaker = redisx.NewBreaker(3, 5*time.Second)
	}
	if log == nil {
		log = slog.Default()
	}
	return &Guard{primary: primary, breaker: breaker, local: newLocalBuckets(cfg.LocalMaxKeys), cfg: cfg, log: log}
}

// Allow takes one token for key.
func (g *Guard) Allow(ctx context.Context, key string, spec Spec) Verdict {
	g.Stats.Checks.Add(1)
	v := g.decide(ctx, key, spec)
	if !v.Allowed {
		g.Stats.Denied.Add(1)
	}
	return v
}

func (g *Guard) decide(ctx context.Context, key string, spec Spec) Verdict {
	if !spec.valid() {
		// A misconfigured spec must never turn into "unlimited".
		g.Stats.FailClosed.Add(1)
		return Verdict{Unavailable: true, RetryAfter: time.Minute}
	}
	if g.primary == nil {
		return g.localDecision(key, spec, false)
	}
	if g.breaker.Allow() {
		allowed, retry, err := g.primary.Take(ctx, key, spec)
		if err == nil {
			g.breaker.Success()
			return Verdict{Allowed: allowed, RetryAfter: retry}
		}
		g.breaker.Failure()
		g.Stats.RedisErrors.Add(1)
		g.log.Warn("rate limiter: redis unavailable, using fallback", "error", err.Error())
	} else {
		g.Stats.BreakerSkips.Add(1)
	}
	// Redis is down: apply the configured policy.
	if g.cfg.Policy == FallbackClosed {
		g.Stats.FailClosed.Add(1)
		return Verdict{Unavailable: true, Degraded: true, RetryAfter: 30 * time.Second}
	}
	return g.localDecision(key, spec, true)
}

func (g *Guard) localDecision(key string, spec Spec, degraded bool) Verdict {
	g.Stats.LocalChecks.Add(1)
	if degraded && g.cfg.LocalDivisor > 1 {
		d := g.cfg.LocalDivisor
		spec = Spec{Capacity: max(1, spec.Capacity/d), Every: spec.Every * time.Duration(d)}
	}
	allowed, retry, ok := g.local.take(key, spec, time.Now())
	if !ok {
		// Table full of live buckets: refuse rather than lose track of limits.
		g.Stats.FailClosed.Add(1)
		return Verdict{Unavailable: true, Degraded: degraded, RetryAfter: 30 * time.Second}
	}
	return Verdict{Allowed: allowed, RetryAfter: retry, Degraded: degraded}
}

// KeyedAllow adapts a Guard to the Allow(key) (bool, wait) shape used by the
// per-email login limiter. The raw key (an email) is hashed before it reaches
// Redis, so addresses are not stored there.
type KeyedAllow struct {
	Guard *Guard
	Name  string
	Spec  Spec
}

func (k *KeyedAllow) Allow(key string) (bool, time.Duration) {
	v := k.Guard.Allow(context.Background(), k.Name+":"+HashKey(key), k.Spec)
	return v.Allowed, v.RetryAfter
}

// HashKey shortens and anonymises a key (email, user id) for use in Redis.
func HashKey(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:16])
}

/* ---------------- local fallback buckets ---------------- */

type localBucket struct {
	tokens float64
	last   time.Time
	idleAt time.Duration // idle time after which the bucket is indistinguishable from full
}

type localBuckets struct {
	mu        sync.Mutex
	m         map[string]*localBucket
	max       int
	lastSweep time.Time
}

func newLocalBuckets(max int) *localBuckets {
	return &localBuckets{m: make(map[string]*localBucket), max: max, lastSweep: time.Now()}
}

// take returns ok=false when the table is full of live buckets and key is new.
func (l *localBuckets) take(key string, spec Spec, now time.Time) (allowed bool, retry time.Duration, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if len(l.m) >= l.max || now.Sub(l.lastSweep) > time.Minute {
		l.sweep(now)
	}
	b, exists := l.m[key]
	if !exists {
		if len(l.m) >= l.max {
			return false, 0, false
		}
		b = &localBucket{tokens: float64(spec.Capacity), last: now}
		l.m[key] = b
	}
	b.idleAt = spec.fullRefill()
	if el := now.Sub(b.last); el > 0 {
		b.tokens += float64(el) / float64(spec.Every)
		if cp := float64(spec.Capacity); b.tokens > cp {
			b.tokens = cp
		}
	}
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0, true
	}
	need := 1 - b.tokens
	return false, time.Duration(need * float64(spec.Every)), true
}

func (l *localBuckets) sweep(now time.Time) {
	for k, b := range l.m {
		if now.Sub(b.last) >= b.idleAt {
			delete(l.m, k)
		}
	}
	l.lastSweep = now
}
