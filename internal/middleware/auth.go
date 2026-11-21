package middleware

import (
	"context"
	"log/slog"
	"net/http"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/httpx"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/service"
)

// AccessAuthenticator verifies access tokens.
type AccessAuthenticator interface {
	AuthenticateAccess(token string) (*service.Claims, error)
}

type claimsKey struct{}

// Authenticate requires a valid access token in the session_token cookie and
// stores the verified claims in the request context. Identity always comes
// from the token, never from the request body or URL.
func Authenticate(a AccessAuthenticator, log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, err := r.Cookie(httpx.SessionCookie)
			if err != nil || c.Value == "" {
				httpx.WriteError(w, r, apperr.Unauthenticated("Authentication required"), log)
				return
			}
			claims, err := a.AuthenticateAccess(c.Value)
			if err != nil {
				httpx.WriteError(w, r, err, log)
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), claimsKey{}, claims)))
		})
	}
}

// ClaimsFrom returns the claims stored by Authenticate.
func ClaimsFrom(ctx context.Context) (*service.Claims, bool) {
	c, ok := ctx.Value(claimsKey{}).(*service.Claims)
	return c, ok && c != nil
}

// RequireRole allows only the listed roles. Use after Authenticate.
func RequireRole(log *slog.Logger, roles ...models.Role) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims, ok := ClaimsFrom(r.Context())
			if !ok {
				httpx.WriteError(w, r, apperr.Unauthenticated("Authentication required"), log)
				return
			}
			for _, role := range roles {
				if claims.Role == role {
					next.ServeHTTP(w, r)
					return
				}
			}
			httpx.WriteError(w, r, apperr.Forbidden("You do not have access to this resource"), log)
		})
	}
}

// AdminGateChecker validates the Super Admin 2FA cookie.
type AdminGateChecker interface {
	Enabled() bool
	ValidGateToken(token string) bool
}

// RequireAdminGate blocks /admin APIs until env login + email OTP succeeded.
// No-op when admin gate is not configured (Enabled() == false).
func RequireAdminGate(g AdminGateChecker, log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if g == nil || !g.Enabled() {
				next.ServeHTTP(w, r)
				return
			}
			c, err := r.Cookie(httpx.AdminGateCookie)
			if err != nil || c.Value == "" || !g.ValidGateToken(c.Value) {
				httpx.WriteError(w, r, apperr.Unauthenticated("Admin verification required"), log)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func OptionalCheckoutAuth(a AccessAuthenticator, log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		authenticated := Authenticate(a, log)(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, err := r.Cookie(httpx.SessionCookie); err == http.ErrNoCookie {
				next.ServeHTTP(w, r)
				return
			}
			authenticated.ServeHTTP(w, r)
		})
	}
}
