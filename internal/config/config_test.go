package config

import (
	"strings"
	"testing"
	"time"
)

func lookupFrom(m map[string]string) Lookup {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func validEnv() map[string]string {
	return map[string]string{
		"MONGODB_URI": "mongodb://localhost:27017",
		"MONGODB_DB":  "shopbd",
		"JWT_SECRET":  strings.Repeat("s3cr3t-value-", 4),
	}
}

func TestValidConfigWithDefaults(t *testing.T) {
	c, err := FromLookup(lookupFrom(validEnv()))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Port != "8080" || c.Env != "development" {
		t.Errorf("defaults wrong: port=%s env=%s", c.Port, c.Env)
	}
	if c.AccessTTL != 15*time.Minute || c.RefreshTTL != 720*time.Hour {
		t.Errorf("ttl defaults wrong: %v %v", c.AccessTTL, c.RefreshTTL)
	}
	if len(c.CORSOrigins) != 1 || c.CORSOrigins[0] != "http://localhost:3000" {
		t.Errorf("cors default wrong: %v", c.CORSOrigins)
	}
	if !c.AutoCreateIndexes || c.CookieSecure || c.TrustProxy {
		t.Errorf("bool defaults wrong: %+v", c)
	}
}

func TestMissingRequiredValues(t *testing.T) {
	_, err := FromLookup(lookupFrom(map[string]string{}))
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"MONGODB_URI is required", "MONGODB_DB is required", "JWT_SECRET is required"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should contain %q, got %q", want, err.Error())
		}
	}
}

func TestRejectsPlaceholdersAndWeakSecrets(t *testing.T) {
	cases := map[string]map[string]string{
		"short secret":         {"JWT_SECRET": "short"},
		"placeholder secret":   {"JWT_SECRET": "<paste-generated-secret-here-xxxxxxxxxxxx>"},
		"change_me secret":     {"JWT_SECRET": "CHANGE_ME_please_replace_this_value_now"},
		"placeholder mongo":    {"MONGODB_URI": "mongodb+srv://<username>:<password>@<cluster-host>/"},
		"bad mongo scheme":     {"MONGODB_URI": "postgres://x"},
		"bad db name":          {"MONGODB_DB": "my.db"},
		"wildcard cors":        {"CORS_ALLOWED_ORIGINS": "*"},
		"cors with path":       {"CORS_ALLOWED_ORIGINS": "http://localhost:3000/app"},
		"cors bad scheme":      {"CORS_ALLOWED_ORIGINS": "ftp://localhost"},
		"bad access ttl":       {"ACCESS_TOKEN_TTL": "soon"},
		"access ttl too long":  {"ACCESS_TOKEN_TTL": "5h"},
		"refresh below access": {"ACCESS_TOKEN_TTL": "30m", "REFRESH_TOKEN_TTL": "10m"},
		"bad port":             {"PORT": "99999"},
		"bad bool":             {"COOKIE_SECURE": "maybe"},
		"bad env":              {"APP_ENV": "staging"},
	}
	for name, override := range cases {
		t.Run(name, func(t *testing.T) {
			env := validEnv()
			for k, v := range override {
				env[k] = v
			}
			if _, err := FromLookup(lookupFrom(env)); err == nil {
				t.Fatal("expected a validation error")
			}
		})
	}
}

func TestErrorsNeverContainSecrets(t *testing.T) {
	env := validEnv()
	env["JWT_SECRET"] = "tooshort-secret-value"
	env["MONGODB_URI"] = "mongodb://user:hunter2@localhost/<x>"
	_, err := FromLookup(lookupFrom(env))
	if err == nil {
		t.Fatal("expected error")
	}
	for _, leaked := range []string{"tooshort-secret-value", "hunter2"} {
		if strings.Contains(err.Error(), leaked) {
			t.Errorf("error leaks secret value %q: %v", leaked, err)
		}
	}
}

func TestProductionRequiresSecureCookies(t *testing.T) {
	env := validEnv()
	env["APP_ENV"] = "production"
	if _, err := FromLookup(lookupFrom(env)); err == nil {
		t.Fatal("production without COOKIE_SECURE should fail")
	}
	env["COOKIE_SECURE"] = "true"
	if _, err := FromLookup(lookupFrom(env)); err != nil {
		t.Fatalf("production with COOKIE_SECURE should pass: %v", err)
	}
}

func TestOriginsAreNormalized(t *testing.T) {
	env := validEnv()
	env["CORS_ALLOWED_ORIGINS"] = " HTTP://LocalHost:3000/ , https://shop.example.com "
	c, err := FromLookup(lookupFrom(env))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"http://localhost:3000", "https://shop.example.com"}
	if len(c.CORSOrigins) != 2 || c.CORSOrigins[0] != want[0] || c.CORSOrigins[1] != want[1] {
		t.Errorf("got %v want %v", c.CORSOrigins, want)
	}
}

func TestRedisCacheAndRateLimitDefaults(t *testing.T) {
	env := validEnv()
	env["REDIS_URL"] = "redis://localhost:6379"
	c, err := FromLookup(lookupFrom(env))
	if err != nil {
		t.Fatal(err)
	}
	if c.RedisRateLimitURL != env["REDIS_URL"] || c.RedisCacheURL != env["REDIS_URL"] {
		t.Errorf("both purposes should default to REDIS_URL: %q %q", c.RedisRateLimitURL, c.RedisCacheURL)
	}
	if c.RateLimitFailClosed || c.RateLimitLocalDiv != 3 {
		t.Errorf("rate limit defaults wrong: closed=%v div=%d", c.RateLimitFailClosed, c.RateLimitLocalDiv)
	}
	if !c.CacheEnabled || c.CacheMaxDBLoads != 16 || c.CacheLockTTL <= c.CacheLoadTimeout {
		t.Errorf("cache defaults wrong: %+v", c)
	}
}

func TestRedisSeparateURLsAndOverrides(t *testing.T) {
	env := validEnv()
	env["RATELIMIT_REDIS_URL"] = "rediss://rl.example:6380"
	env["CACHE_REDIS_URL"] = "redis://cache.example:6379"
	env["CACHE_PUBLIC_TTL"] = "2m"
	env["CACHE_TTL_JITTER"] = "0.2"
	env["RATELIMIT_FAIL_CLOSED"] = "true"
	c, err := FromLookup(lookupFrom(env))
	if err != nil {
		t.Fatal(err)
	}
	if c.RedisRateLimitURL == c.RedisCacheURL || c.CachePublicTTL != 2*time.Minute || c.CacheJitterPct != 0.2 || !c.RateLimitFailClosed {
		t.Errorf("overrides not applied: %+v", c)
	}
}

func TestRedisTuningRejectsBadValues(t *testing.T) {
	for k, v := range map[string]string{
		"CACHE_REDIS_URL":     "http://not-redis",
		"CACHE_TTL_JITTER":    "0.9",
		"CACHE_LOCK_TTL":      "1s", // must exceed CACHE_LOAD_TIMEOUT (5s default)
		"REDIS_POOL_SIZE":     "0",
		"CACHE_MAX_DB_LOADS":  "abc",
		"REDIS_OP_TIMEOUT":    "1ns",
		"CACHE_ENABLED":       "maybe",
		"CACHE_MAX_WAITERS":   "1",
		"CACHE_PUBLIC_TTL":    "0s",
		"RATELIMIT_REDIS_URL": "localhost:6379",
	} {
		env := validEnv()
		env[k] = v
		if _, err := FromLookup(lookupFrom(env)); err == nil {
			t.Errorf("%s=%q should be rejected", k, v)
		}
	}
}

func TestRedisOTPURL(t *testing.T) {
	// Not set anywhere: empty, so the personal-info feature reports "no Redis".
	c, err := FromLookup(lookupFrom(validEnv()))
	if err != nil || c.RedisOTPURL != "" {
		t.Fatalf("default: url=%q err=%v", c.RedisOTPURL, err)
	}
	// REDIS_URL alone is enough.
	env := validEnv()
	env["REDIS_URL"] = "redis://localhost:6379/0"
	if c, err = FromLookup(lookupFrom(env)); err != nil || c.RedisOTPURL != "redis://localhost:6379/0" {
		t.Fatalf("default from REDIS_URL: url=%q err=%v", c.RedisOTPURL, err)
	}
	// OTP_REDIS_URL overrides it.
	env["OTP_REDIS_URL"] = "rediss://otp.example.com:6380/1"
	if c, err = FromLookup(lookupFrom(env)); err != nil || c.RedisOTPURL != "rediss://otp.example.com:6380/1" {
		t.Fatalf("override: url=%q err=%v", c.RedisOTPURL, err)
	}
	// A bad scheme is rejected at startup.
	env["OTP_REDIS_URL"] = "http://nope"
	if _, err = FromLookup(lookupFrom(env)); err == nil || !strings.Contains(err.Error(), "OTP_REDIS_URL") {
		t.Fatalf("bad scheme: err=%v", err)
	}
}
