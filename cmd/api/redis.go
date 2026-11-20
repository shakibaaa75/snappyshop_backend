package main

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"shopbd/backend/internal/cache"
	"shopbd/backend/internal/config"
	"shopbd/backend/internal/ratelimit"
	"shopbd/backend/internal/redisx"
)

// redisRuntime holds everything built on Redis. Rate limiting and the cache use
// separate clients, key prefixes and circuit breakers. With one REDIS_URL they
// share a connection pool; give them different URLs (RATELIMIT_REDIS_URL,
// CACHE_REDIS_URL) to move them to separate Redis instances, which is the
// recommended production layout: rate limits must not be evicted under memory
// pressure, cache entries may be.
type redisRuntime struct {
	RateGuard *ratelimit.Guard // nil when no Redis is configured for rate limiting
	Cache     *cache.Cache     // never nil; runs in degraded mode without Redis
	clients   []*redis.Client
}

func (rt *redisRuntime) Close() {
	for _, c := range rt.clients {
		_ = c.Close()
	}
}

// setupRedis never fails because Redis is unreachable: the API starts and the
// guard/cache run in their degraded modes until Redis comes back. It only
// fails on configuration errors (an unparsable URL).
func setupRedis(ctx context.Context, cfg *config.Config, log *slog.Logger) (*redisRuntime, error) {
	rt := &redisRuntime{}
	rcfg := func(url string) redisx.Config {
		return redisx.Config{
			URL: url, PoolSize: cfg.RedisPoolSize, MinIdleConns: cfg.RedisMinIdle,
			DialTimeout: cfg.RedisDialTimeout, ReadTimeout: cfg.RedisOpTimeout * 2,
			WriteTimeout: cfg.RedisOpTimeout * 2, MaxRetries: cfg.RedisMaxRetries,
		}
	}
	byURL := map[string]*redis.Client{}
	open := func(purpose, url string) (*redis.Client, error) {
		if c, ok := byURL[url]; ok {
			return c, nil
		}
		c, err := redisx.Open(rcfg(url))
		if err != nil {
			return nil, err
		}
		byURL[url] = c
		rt.clients = append(rt.clients, c)
		if err := redisx.Ping(ctx, c, 3*time.Second); err != nil {
			log.Warn("redis not reachable at startup; continuing in degraded mode", "purpose", purpose, "error", err.Error())
		}
		if pol := redisx.EvictionPolicy(ctx, c); strings.HasPrefix(pol, "allkeys-") {
			log.Warn("redis evicts any key under memory pressure; rate-limit counters could be evicted. "+
				"Use a separate instance for rate limiting or a volatile-* policy",
				"purpose", purpose, "maxmemory_policy", pol)
		}
		return c, nil
	}

	// Rate limiting.
	if cfg.RedisRateLimitURL != "" {
		c, err := open("ratelimit", cfg.RedisRateLimitURL)
		if err != nil {
			return nil, err
		}
		policy := ratelimit.FallbackLocal
		if cfg.RateLimitFailClosed {
			policy = ratelimit.FallbackClosed
		}
		rt.RateGuard = ratelimit.NewGuard(
			ratelimit.NewRedisBucket(c, "shopbd:rl:", cfg.RedisOpTimeout),
			redisx.NewBreaker(3, 5*time.Second),
			ratelimit.GuardConfig{Policy: policy, LocalDivisor: cfg.RateLimitLocalDiv, LocalMaxKeys: cfg.RateLimitLocalMaxKey},
			log)
		log.Info("rate limiting: shared token buckets in redis",
			"fallback_when_redis_down", map[bool]string{true: "fail closed (503)", false: "strict local limit"}[cfg.RateLimitFailClosed])
	} else if cfg.IsProduction() {
		log.Warn("no Redis URL set: auth rate limits are per instance only; " +
			"with N instances an attacker gets N times the limit")
	}

	// Cache.
	var be cache.Backend
	var br *redisx.Breaker
	if cfg.CacheEnabled && cfg.RedisCacheURL != "" {
		c, err := open("cache", cfg.RedisCacheURL)
		if err != nil {
			return nil, err
		}
		be = cache.NewRedisBackend(c, cfg.RedisOpTimeout)
		br = redisx.NewBreaker(3, 5*time.Second)
	}
	cc := cache.DefaultConfig()
	cc.Prefix = cfg.CacheKeyPrefix
	cc.LockTTL = cfg.CacheLockTTL
	cc.LoadTimeout = cfg.CacheLoadTimeout
	cc.FlightTimeout = 3 * cfg.CacheLoadTimeout
	cc.MaxWaiters = cfg.CacheMaxWaiters
	cc.MaxLoads = cfg.CacheMaxDBLoads
	cc.MaxValueBytes = cfg.CacheMaxValueBytes
	rt.Cache = cache.New(be, br, cc, log)

	// Only public, seller-agnostic or explicitly seller-keyed data may be
	// registered. Checkout prices, stock and carts are deliberately absent:
	// an unregistered namespace cannot be cached.
	for _, name := range []string{"product", "storefront"} {
		if err := rt.Cache.Register(cache.Namespace{
			Name: name, TTL: cfg.CachePublicTTL, NegativeTTL: cfg.CacheNegativeTTL,
			JitterPct: cfg.CacheJitterPct, DegradedTTL: cfg.CacheDegradedTTL,
		}); err != nil {
			return nil, err
		}
	}
	if be == nil {
		log.Info("cache: no Redis URL; running in-process only (singleflight + bounded database loads)")
	}

	go logStats(ctx, log, rt)
	return rt, nil
}

// logStats writes the cache and rate-limit counters once a minute, only when
// something changed, so stampedes, lock contention, timeouts and avoided
// database queries are visible in the logs without extra infrastructure.
func logStats(ctx context.Context, log *slog.Logger, rt *redisRuntime) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	var lastCache, lastRL map[string]int64
	changed := func(a, b map[string]int64) bool {
		for k, v := range a {
			if b[k] != v {
				return true
			}
		}
		return false
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if s := rt.Cache.Stats.Snapshot(); changed(s, lastCache) {
				lastCache = s
				log.Info("cache stats", asAttrs(s)...)
			}
			if rt.RateGuard != nil {
				if s := rt.RateGuard.Stats.Snapshot(); changed(s, lastRL) {
					lastRL = s
					log.Info("rate limiter stats", asAttrs(s)...)
				}
			}
		}
	}
}

func asAttrs(m map[string]int64) []any {
	out := make([]any, 0, len(m)*2)
	for k, v := range m {
		out = append(out, k, v)
	}
	return out
}
