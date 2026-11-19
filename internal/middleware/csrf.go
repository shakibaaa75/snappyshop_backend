package middleware

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/httpx"
)

// CSRFHeader is the custom header every state-changing request must carry.
// Browsers only allow cross-origin pages to add it after a CORS preflight,
// which the API only approves for configured frontend origins.
const CSRFHeader = "X-Requested-With"

// CSRF protects cookie-authenticated, state-changing requests (anything other
// than GET/HEAD/OPTIONS). It rejects requests whose Origin header is present
// but not an allowed frontend origin, and requests missing the custom header.
// SameSite=Lax cookies are an additional layer.
func CSRF(allowedOrigins []string, log *slog.Logger) func(http.Handler) http.Handler {
	return CSRFWith(allowedOrigins, nil, log)
}

// CSRFWith is CSRF plus an optional extra rule for origins that are not in the
// fixed list. Stores on a seller's own domain use it: their origin is only
// known at run time, so extra reports whether an origin is a connected custom
// domain. A nil extra behaves exactly like CSRF.
func CSRFWith(allowedOrigins []string, extra func(ctx context.Context, origin string) bool, log *slog.Logger) func(http.Handler) http.Handler {
	allowed := make(map[string]struct{}, len(allowedOrigins))
	for _, o := range allowedOrigins {
		allowed[strings.ToLower(o)] = struct{}{}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet, http.MethodHead, http.MethodOptions:
				next.ServeHTTP(w, r)
				return
			}
			if origin := r.Header.Get("Origin"); origin != "" {
				if _, ok := allowed[strings.ToLower(origin)]; !ok {
					if extra == nil || !extra(r.Context(), origin) {
						httpx.WriteError(w, r, apperr.CSRFRejected("Origin not allowed"), log)
						return
					}
				}
			}
			if strings.TrimSpace(r.Header.Get(CSRFHeader)) == "" {
				httpx.WriteError(w, r, apperr.CSRFRejected("Missing "+CSRFHeader+" header"), log)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
