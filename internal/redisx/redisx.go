// Package redisx builds Redis clients from configuration and provides the
// circuit breaker shared by the rate limiter and the cache.
//
// Design rules:
//   - A Redis outage must never crash the API. Open does not ping; go-redis
//     connects lazily, and callers treat every error as "Redis unavailable".
//   - Timeouts are short and explicit so a hung Redis cannot hold request
//     goroutines.
//   - Rate-limit data and cache data use separate clients and key prefixes,
//     so they can be moved to separate Redis instances without code changes
//     (set RATELIMIT_REDIS_URL and CACHE_REDIS_URL to different values).
package redisx

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// Config holds connection settings. Zero values keep go-redis defaults.
type Config struct {
	URL          string
	PoolSize     int
	MinIdleConns int
	DialTimeout  time.Duration
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	MaxRetries   int
}

// Open parses the URL and returns a client. It does not connect: a bad URL is
// a configuration bug and fails here, an unreachable server is not.
func Open(cfg Config) (*redis.Client, error) {
	opt, err := redis.ParseURL(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("parse redis url: %w", err)
	}
	if cfg.PoolSize > 0 {
		opt.PoolSize = cfg.PoolSize
	}
	if cfg.MinIdleConns > 0 {
		opt.MinIdleConns = cfg.MinIdleConns
	}
	if cfg.DialTimeout > 0 {
		opt.DialTimeout = cfg.DialTimeout
	}
	if cfg.ReadTimeout > 0 {
		opt.ReadTimeout = cfg.ReadTimeout
	}
	if cfg.WriteTimeout > 0 {
		opt.WriteTimeout = cfg.WriteTimeout
	}
	if cfg.MaxRetries > 0 {
		opt.MaxRetries = cfg.MaxRetries
	}
	return redis.NewClient(opt), nil
}

// Ping checks the connection with a bounded timeout. Startup code logs the
// result but keeps going when it fails.
func Ping(ctx context.Context, c *redis.Client, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return c.Ping(ctx).Err()
}

// EvictionPolicy returns the server's maxmemory-policy, or "" when the
// provider does not allow CONFIG GET (some managed plans block it).
func EvictionPolicy(ctx context.Context, c *redis.Client) string {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	res, err := c.ConfigGet(ctx, "maxmemory-policy").Result()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(res["maxmemory-policy"])
}

// Breaker stops calling an unhealthy Redis for a cooldown period, so an outage
// costs one fast failure per request instead of a full timeout per request.
// After the cooldown a single failure re-opens it immediately.
type Breaker struct {
	threshold int32
	cooldown  time.Duration
	fails     atomic.Int32
	openUntil atomic.Int64 // unix nanos; 0 = closed
	now       func() time.Time
}

func NewBreaker(threshold int, cooldown time.Duration) *Breaker {
	if threshold < 1 {
		threshold = 3
	}
	if cooldown <= 0 {
		cooldown = 5 * time.Second
	}
	return &Breaker{threshold: int32(threshold), cooldown: cooldown, now: time.Now}
}

// Allow reports whether Redis may be called right now.
func (b *Breaker) Allow() bool {
	return b.now().UnixNano() >= b.openUntil.Load()
}

// Open reports whether the breaker is currently refusing calls.
func (b *Breaker) Open() bool { return !b.Allow() }

func (b *Breaker) Success() { b.fails.Store(0) }

func (b *Breaker) Failure() {
	if b.fails.Add(1) >= b.threshold {
		b.openUntil.Store(b.now().Add(b.cooldown).UnixNano())
		b.fails.Store(b.threshold - 1) // half-open: one more failure re-opens
	}
}
