// Package config loads and validates configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Env               string
	Port              string
	MongoURI          string
	MongoDB           string
	JWTSecret         []byte
	AccessTTL         time.Duration
	RefreshTTL        time.Duration
	CORSOrigins       []string
	CookieSecure      bool
	CookieDomain      string
	AutoCreateIndexes bool
	TrustProxy        bool
	RedisURL          string // optional; default Redis for both rate limiting and cache

	// Redis, split by purpose. Rate-limit data and cache data use different
	// URLs when set, so they can live on separate instances (rate limits must
	// never be evicted; cache entries may be). Both default to RedisURL.
	RedisRateLimitURL string
	RedisCacheURL     string
	RedisPoolSize     int
	RedisMinIdle      int
	RedisDialTimeout  time.Duration
	RedisOpTimeout    time.Duration // per-command budget for cache and limiter calls
	RedisMaxRetries   int

	// Rate limiting (token buckets in Redis).
	RateLimitFailClosed  bool // true: refuse (503) when Redis is down; false: strict local fallback
	RateLimitLocalDiv    int  // fallback tightening; set to roughly the number of API instances
	RateLimitLocalMaxKey int

	// Cache.
	CacheEnabled       bool
	CacheKeyPrefix     string
	CacheLockTTL       time.Duration
	CacheLoadTimeout   time.Duration
	CacheMaxWaiters    int
	CacheMaxDBLoads    int
	CacheMaxValueBytes int
	CachePublicTTL     time.Duration // TTL for public, mutable data (products, storefronts)
	CacheNegativeTTL   time.Duration // TTL for "does not exist"
	CacheJitterPct     float64       // +/- fraction applied to every TTL
	CacheDegradedTTL   time.Duration // in-process reuse while Redis is down

	SMTPHost     string
	SMTPPort     string
	SMTPUsername string
	SMTPPassword string
	SMTPFrom     string

	// Super Admin gate (optional). When set, /admin requires this login + email OTP.
	AdminEmail    string
	AdminPassword string
}

func (c *Config) IsProduction() bool { return c.Env == "production" }

type Lookup func(key string) (string, bool)

func FromLookup(lookup Lookup) (*Config, error) {
	var errs []error
	get := func(key, def string) string {
		if v, ok := lookup(key); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
		return def
	}
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	c := &Config{}

	c.Env = get("APP_ENV", "development")
	switch c.Env {
	case "development", "production", "test":
	default:
		fail("APP_ENV must be development, production or test")
	}

	c.Port = get("PORT", "8080")
	if p, err := strconv.Atoi(c.Port); err != nil || p < 1 || p > 65535 {
		fail("PORT must be a number between 1 and 65535")
	}

	c.MongoURI = get("MONGODB_URI", "")
	switch {
	case c.MongoURI == "":
		fail("MONGODB_URI is required")
	case !strings.HasPrefix(c.MongoURI, "mongodb://") && !strings.HasPrefix(c.MongoURI, "mongodb+srv://"):
		fail("MONGODB_URI must start with mongodb:// or mongodb+srv://")
	case strings.Contains(c.MongoURI, "<") || strings.Contains(c.MongoURI, ">"):
		fail("MONGODB_URI still contains placeholder values")
	}

	c.MongoDB = get("MONGODB_DB", "")
	switch {
	case c.MongoDB == "":
		fail("MONGODB_DB is required")
	case len(c.MongoDB) > 38 || strings.ContainsAny(c.MongoDB, `/\. "$*<>:|?`):
		fail("MONGODB_DB is not a valid database name")
	}

	secret := get("JWT_SECRET", "")
	switch {
	case secret == "":
		fail("JWT_SECRET is required")
	case len(secret) < 32:
		fail("JWT_SECRET must be at least 32 bytes long")
	case strings.ContainsAny(secret, "<>") || strings.Contains(strings.ToLower(secret), "change_me"):
		fail("JWT_SECRET still contains a placeholder value")
	}
	c.JWTSecret = []byte(secret)

	var err error
	if c.AccessTTL, err = time.ParseDuration(get("ACCESS_TOKEN_TTL", "15m")); err != nil {
		fail("ACCESS_TOKEN_TTL is not a valid duration (example: 15m)")
	} else if c.AccessTTL < time.Minute || c.AccessTTL > time.Hour {
		fail("ACCESS_TOKEN_TTL must be between 1m and 1h")
	}
	if c.RefreshTTL, err = time.ParseDuration(get("REFRESH_TOKEN_TTL", "720h")); err != nil {
		fail("REFRESH_TOKEN_TTL is not a valid duration (example: 720h)")
	} else if c.RefreshTTL < c.AccessTTL || c.RefreshTTL > 90*24*time.Hour {
		fail("REFRESH_TOKEN_TTL must be at least ACCESS_TOKEN_TTL and at most 2160h")
	}

	origins, oerr := parseOrigins(get("CORS_ALLOWED_ORIGINS", "http://localhost:3000"))
	if oerr != nil {
		fail("%v", oerr)
	}
	c.CORSOrigins = origins

	if c.CookieSecure, err = parseBool(get("COOKIE_SECURE", "false")); err != nil {
		fail("COOKIE_SECURE must be true or false")
	}
	c.CookieDomain = get("COOKIE_DOMAIN", "")
	if strings.ContainsAny(c.CookieDomain, " /;") {
		fail("COOKIE_DOMAIN is not a valid domain")
	}
	if c.AutoCreateIndexes, err = parseBool(get("AUTO_CREATE_INDEXES", "true")); err != nil {
		fail("AUTO_CREATE_INDEXES must be true or false")
	}
	if c.TrustProxy, err = parseBool(get("TRUST_PROXY", "false")); err != nil {
		fail("TRUST_PROXY must be true or false")
	}

	c.RedisURL = get("REDIS_URL", "")
	if c.RedisURL != "" && !strings.HasPrefix(c.RedisURL, "redis://") && !strings.HasPrefix(c.RedisURL, "rediss://") {
		fail("REDIS_URL must start with redis:// or rediss://")
	}

	c.loadRedisTuning(get, fail)

	if c.IsProduction() && !c.CookieSecure {
		fail("COOKIE_SECURE must be true when APP_ENV=production")
	}

	// SMTP Config
	c.SMTPHost = get("SMTP_HOST", "")
	c.SMTPPort = get("SMTP_PORT", "587")
	c.SMTPUsername = get("SMTP_USERNAME", "")
	c.SMTPPassword = get("SMTP_PASSWORD", "")
	c.SMTPFrom = get("SMTP_FROM", c.SMTPUsername)
	c.AdminEmail = strings.ToLower(strings.TrimSpace(get("ADMIN_EMAIL", "")))
	c.AdminPassword = get("ADMIN_PASSWORD", "")
	if c.SMTPHost != "" && (c.SMTPUsername == "" || c.SMTPPassword == "") {
		fail("SMTP_USERNAME and SMTP_PASSWORD are required when SMTP_HOST is set")
	}

	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return c, nil
}

// loadRedisTuning reads the Redis, rate-limit and cache settings. Every value
// has a safe default, so setting only REDIS_URL is enough to get started.
func (c *Config) loadRedisTuning(get func(key, def string) string, fail func(format string, args ...any)) {
	redisURL := func(key string) string {
		v := get(key, c.RedisURL)
		if v != "" && !strings.HasPrefix(v, "redis://") && !strings.HasPrefix(v, "rediss://") {
			fail("%s must start with redis:// or rediss://", key)
		}
		return v
	}
	c.RedisRateLimitURL = redisURL("RATELIMIT_REDIS_URL")
	c.RedisCacheURL = redisURL("CACHE_REDIS_URL")

	intVal := func(key string, def, min, max int) int {
		n, err := strconv.Atoi(get(key, strconv.Itoa(def)))
		if err != nil || n < min || n > max {
			fail("%s must be a whole number between %d and %d", key, min, max)
			return def
		}
		return n
	}
	durVal := func(key, def string, min, max time.Duration) time.Duration {
		d, err := time.ParseDuration(get(key, def))
		if err != nil || d < min || d > max {
			fail("%s must be a duration between %s and %s (example: %s)", key, min, max, def)
			d, _ = time.ParseDuration(def)
		}
		return d
	}

	c.RedisPoolSize = intVal("REDIS_POOL_SIZE", 10, 1, 500)
	c.RedisMinIdle = intVal("REDIS_MIN_IDLE_CONNS", 1, 0, 100)
	c.RedisDialTimeout = durVal("REDIS_DIAL_TIMEOUT", "2s", 100*time.Millisecond, 30*time.Second)
	c.RedisOpTimeout = durVal("REDIS_OP_TIMEOUT", "150ms", 20*time.Millisecond, 5*time.Second)
	c.RedisMaxRetries = intVal("REDIS_MAX_RETRIES", 1, 0, 5)

	var err error
	if c.RateLimitFailClosed, err = parseBool(get("RATELIMIT_FAIL_CLOSED", "false")); err != nil {
		fail("RATELIMIT_FAIL_CLOSED must be true or false")
	}
	c.RateLimitLocalDiv = intVal("RATELIMIT_LOCAL_DIVISOR", 3, 1, 100)
	c.RateLimitLocalMaxKey = intVal("RATELIMIT_LOCAL_MAX_KEYS", 50000, 1000, 1000000)

	if c.CacheEnabled, err = parseBool(get("CACHE_ENABLED", "true")); err != nil {
		fail("CACHE_ENABLED must be true or false")
	}
	c.CacheKeyPrefix = get("CACHE_KEY_PREFIX", "shopbd:cache:")
	if strings.ContainsAny(c.CacheKeyPrefix, " \t\r\n") {
		fail("CACHE_KEY_PREFIX must not contain whitespace")
	}
	c.CacheLoadTimeout = durVal("CACHE_LOAD_TIMEOUT", "5s", 500*time.Millisecond, 30*time.Second)
	c.CacheLockTTL = durVal("CACHE_LOCK_TTL", "10s", time.Second, 2*time.Minute)
	if c.CacheLockTTL <= c.CacheLoadTimeout {
		fail("CACHE_LOCK_TTL must be longer than CACHE_LOAD_TIMEOUT")
	}
	c.CacheMaxWaiters = intVal("CACHE_MAX_WAITERS", 2000, 10, 100000)
	c.CacheMaxDBLoads = intVal("CACHE_MAX_DB_LOADS", 16, 1, 500)
	c.CacheMaxValueBytes = intVal("CACHE_MAX_VALUE_BYTES", 256<<10, 1024, 8<<20)
	c.CachePublicTTL = durVal("CACHE_PUBLIC_TTL", "60s", time.Second, 24*time.Hour)
	c.CacheNegativeTTL = durVal("CACHE_NEGATIVE_TTL", "30s", time.Second, time.Hour)
	c.CacheDegradedTTL = durVal("CACHE_DEGRADED_TTL", "5s", 0, time.Minute)
	pct, perr := strconv.ParseFloat(get("CACHE_TTL_JITTER", "0.1"), 64)
	if perr != nil || pct < 0 || pct > 0.5 {
		fail("CACHE_TTL_JITTER must be a number between 0 and 0.5")
		pct = 0.1
	}
	c.CacheJitterPct = pct
}

func parseBool(s string) (bool, error) { return strconv.ParseBool(s) }

func parseOrigins(raw string) ([]string, error) {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if strings.Contains(part, "*") {
			return nil, errors.New("CORS_ALLOWED_ORIGINS must not contain wildcards")
		}
		u, err := url.Parse(part)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
			(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
			return nil, fmt.Errorf("CORS_ALLOWED_ORIGINS entry %q is not a valid origin (expected scheme://host[:port])", part)
		}
		out = append(out, strings.ToLower(u.Scheme+"://"+u.Host))
	}
	if len(out) == 0 {
		return nil, errors.New("CORS_ALLOWED_ORIGINS must list at least one origin")
	}
	return out, nil
}
