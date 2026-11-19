package httpx

import (
	"net/http"
	"time"
)

const (
	// SessionCookie holds the short-lived access JWT. The name matches the
	// cookie the existing Next.js api-client.ts already reads.
	SessionCookie = "session_token"
	// RefreshCookie holds the opaque rotating refresh token.
	RefreshCookie = "refresh_token"
	// AdminGateCookie is set only after Super Admin env login + email OTP.
	// Required for /api/v1/admin/* when ADMIN_EMAIL is configured.
	AdminGateCookie = "admin_gate"

	// APIPrefix is the base path of the API; the refresh cookie is scoped to
	// the auth routes below it.
	APIPrefix         = "/api/v1"
	RefreshCookiePath = APIPrefix + "/auth"
)

// CookieSettings controls cookie attributes.
type CookieSettings struct {
	Secure bool
	Domain string
}

func (c CookieSettings) base(name, value, path string) *http.Cookie {
	return &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     path,
		Domain:   c.Domain,
		HttpOnly: true,
		Secure:   c.Secure,
		SameSite: http.SameSiteLaxMode,
	}
}

// SetSession sets the access-token cookie.
func (c CookieSettings) SetSession(w http.ResponseWriter, token string, ttl time.Duration) {
	ck := c.base(SessionCookie, token, "/")
	ck.MaxAge = int(ttl.Seconds())
	http.SetCookie(w, ck)
}

// SetRefresh sets the refresh-token cookie, scoped to the auth routes.
func (c CookieSettings) SetRefresh(w http.ResponseWriter, token string, ttl time.Duration) {
	ck := c.base(RefreshCookie, token, RefreshCookiePath)
	ck.MaxAge = int(ttl.Seconds())
	http.SetCookie(w, ck)
}

// SetAdminGate marks that Super Admin 2FA completed successfully.
func (c CookieSettings) SetAdminGate(w http.ResponseWriter, token string, ttl time.Duration) {
	ck := c.base(AdminGateCookie, token, "/")
	ck.MaxAge = int(ttl.Seconds())
	http.SetCookie(w, ck)
}

// ClearAdminGate removes the Super Admin gate cookie.
func (c CookieSettings) ClearAdminGate(w http.ResponseWriter) {
	ck := c.base(AdminGateCookie, "", "/")
	ck.MaxAge = -1
	ck.Expires = time.Unix(0, 0)
	http.SetCookie(w, ck)
}

// Clear removes both authentication cookies.
func (c CookieSettings) Clear(w http.ResponseWriter) {
	for _, ck := range []*http.Cookie{
		c.base(SessionCookie, "", "/"),
		c.base(RefreshCookie, "", RefreshCookiePath),
		c.base(AdminGateCookie, "", "/"),
	} {
		ck.MaxAge = -1
		ck.Expires = time.Unix(0, 0)
		http.SetCookie(w, ck)
	}
}
