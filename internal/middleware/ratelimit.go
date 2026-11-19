package middleware

import (
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/httpx"
)

// KeyedLimiter is an in-memory token-bucket limiter keyed by an arbitrary
// string (client IP, email, ...). It is per-process: with several API
// instances each one enforces its own limit.
type KeyedLimiter struct {
	mu        sync.Mutex
	entries   map[string]*limiterEntry
	limit     rate.Limit
	burst     int
	ttl       time.Duration
	lastSweep time.Time
	now       func() time.Time
}

type limiterEntry struct {
	lim      *rate.Limiter
	lastSeen time.Time
}

// NewKeyedLimiter allows `burst` immediate requests per key and then one more
// every `every`.
func NewKeyedLimiter(every time.Duration, burst int) *KeyedLimiter {
	ttl := 10 * time.Minute
	if need := 2 * time.Duration(burst) * every; need > ttl {
		ttl = need
	}
	return &KeyedLimiter{
		entries:   make(map[string]*limiterEntry),
		limit:     rate.Every(every),
		burst:     burst,
		ttl:       ttl,
		lastSweep: time.Now(),
		now:       time.Now,
	}
}

// Allow reports whether the key may proceed; if not, how long to wait.
func (k *KeyedLimiter) Allow(key string) (bool, time.Duration) {
	k.mu.Lock()
	defer k.mu.Unlock()
	now := k.now()
	if now.Sub(k.lastSweep) > time.Minute {
		for key, e := range k.entries {
			if now.Sub(e.lastSeen) > k.ttl {
				delete(k.entries, key)
			}
		}
		k.lastSweep = now
	}
	e, ok := k.entries[key]
	if !ok {
		e = &limiterEntry{lim: rate.NewLimiter(k.limit, k.burst)}
		k.entries[key] = e
	}
	e.lastSeen = now
	r := e.lim.ReserveN(now, 1)
	if !r.OK() {
		return false, k.ttl
	}
	if d := r.DelayFrom(now); d > 0 {
		r.CancelAt(now)
		return false, d
	}
	return true, 0
}

// ClientIP returns the client address. X-Forwarded-For is only consulted when
// trustProxy is set (the API runs behind a trusted reverse proxy); the last
// entry is used because that is the one appended by the nearest proxy.
func ClientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			if ip := strings.TrimSpace(parts[len(parts)-1]); ip != "" {
				return ip
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// RateLimit limits requests per client IP.
func RateLimit(l *KeyedLimiter, trustProxy bool, log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if ok, wait := l.Allow(ClientIP(r, trustProxy)); !ok {
				httpx.WriteError(w, r, apperr.RateLimited(wait), log)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
