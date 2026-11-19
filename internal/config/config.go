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
	RedisURL          string // optional; shared rate limiting across API instances

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
