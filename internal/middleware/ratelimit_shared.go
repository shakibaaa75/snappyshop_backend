package middleware

import (
	"log/slog"
	"net/http"
	"strconv"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/httpx"
	"shopbd/backend/internal/ratelimit"
)

// SharedRateLimit limits requests per client IP with a token bucket that is
// shared by all API instances through Redis. name separates the buckets of
// different endpoints (login, forgot-password, ...).
//
// If Redis is down the Guard falls back to a strict local bucket, or refuses
// the request when the fail-closed policy is set; a refusal for that reason is
// a 503, not a 429, so clients and dashboards can tell them apart.
func SharedRateLimit(g *ratelimit.Guard, name string, spec ratelimit.Spec, trustProxy bool, log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			v := g.Allow(r.Context(), name+":ip:"+ClientIP(r, trustProxy), spec)
			if v.Allowed {
				next.ServeHTTP(w, r)
				return
			}
			WriteLimitRefusal(w, r, v, log)
		})
	}
}

// WriteLimitRefusal renders a refusal: 429 when the limit was reached, 503
// when protection could not be maintained.
func WriteLimitRefusal(w http.ResponseWriter, r *http.Request, v ratelimit.Verdict, log *slog.Logger) {
	if v.Unavailable {
		secs := int(v.RetryAfter.Seconds())
		if secs < 1 {
			secs = 30
		}
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		httpx.WriteError(w, r, apperr.New(http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE",
			"This action is temporarily unavailable. Please try again shortly."), log)
		return
	}
	httpx.WriteError(w, r, apperr.RateLimited(v.RetryAfter), log)
}
