package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/httpx"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/oauth"
	"shopbd/backend/internal/service"
)

// SocialHandler runs "Continue with Google / Apple".
//
// These routes are browser navigations, not fetch calls, so they never answer
// with JSON: success and failure both end in a redirect to the website.
// A failure lands on /login?error=social_<reason>.
type SocialHandler struct {
	Auth        *AuthHandler // reused so the session cookies match normal login exactly
	Google      *oauth.Google // nil = not configured
	Apple       *oauth.Apple  // nil = not configured
	Signer      *oauth.Signer
	Secure      bool // COOKIE_SECURE
	FrontendURL string
	Log         *slog.Logger
}

/* ---- Google ---- */

func (h *SocialHandler) GoogleStart(w http.ResponseWriter, r *http.Request) {
	if h.Google == nil {
		h.fail(w, r, "unavailable")
		return
	}
	h.begin(w, r, models.ProviderGoogle, func(state, nonce, challenge string) string {
		return h.Google.AuthURL(state, nonce, challenge)
	})
}

func (h *SocialHandler) GoogleCallback(w http.ResponseWriter, r *http.Request) {
	if h.Google == nil {
		h.fail(w, r, "unavailable")
		return
	}
	h.complete(w, r, models.ProviderGoogle, r.URL.Query(),
		func(ctx context.Context, code string, sd *oauth.StateData) (*oauth.Profile, error) {
			return h.Google.Exchange(ctx, code, sd.Verifier, sd.Nonce)
		})
}

/* ---- Apple ---- */

func (h *SocialHandler) AppleStart(w http.ResponseWriter, r *http.Request) {
	if h.Apple == nil {
		h.fail(w, r, "unavailable")
		return
	}
	h.begin(w, r, models.ProviderApple, func(state, nonce, _ string) string {
		return h.Apple.AuthURL(state, nonce)
	})
}

// AppleCallback receives Apple's cross-site POST. It is registered outside the
// CSRF middleware (Apple cannot send our header); the signed state cookie and
// the nonce inside the ID token take that job.
func (h *SocialHandler) AppleCallback(w http.ResponseWriter, r *http.Request) {
	if h.Apple == nil {
		h.fail(w, r, "unavailable")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		h.fail(w, r, "failed")
		return
	}
	form := r.PostForm
	h.complete(w, r, models.ProviderApple, form,
		func(ctx context.Context, code string, sd *oauth.StateData) (*oauth.Profile, error) {
			p, err := h.Apple.Exchange(ctx, code, sd.Nonce)
			if err == nil && p.Name == "" {
				p.Name = appleName(form.Get("user")) // only sent the first time
			}
			return p, err
		})
}

func appleName(userJSON string) string {
	if userJSON == "" {
		return ""
	}
	var u struct {
		Name struct {
			First string `json:"firstName"`
			Last  string `json:"lastName"`
		} `json:"name"`
	}
	if json.Unmarshal([]byte(userJSON), &u) != nil {
		return ""
	}
	return strings.TrimSpace(u.Name.First + " " + u.Name.Last)
}

/* ---- shared flow ---- */

// begin remembers who is leaving (signed cookie) and sends them to the provider.
func (h *SocialHandler) begin(w http.ResponseWriter, r *http.Request, provider string, authURL func(state, nonce, challenge string) string) {
	sd := oauth.StateData{
		Provider: provider,
		State:    oauth.Random(24),
		Nonce:    oauth.Random(24),
		Verifier: oauth.Random(48),
		Next:     safeNext(r.URL.Query().Get("next")),
	}
	sealed, err := h.Signer.Seal(sd)
	if err != nil {
		h.Log.Error("social: seal state", "error", err.Error())
		h.fail(w, r, "failed")
		return
	}
	http.SetCookie(w, h.stateCookie(provider, sealed, 600))
	http.Redirect(w, r, authURL(sd.State, sd.Nonce, oauth.Challenge(sd.Verifier)), http.StatusFound)
}

// complete checks the callback, signs the person in and redirects to the site.
func (h *SocialHandler) complete(
	w http.ResponseWriter, r *http.Request, provider string, params url.Values,
	exchange func(ctx context.Context, code string, sd *oauth.StateData) (*oauth.Profile, error),
) {
	c, cookieErr := r.Cookie(stateCookieName(provider))
	http.SetCookie(w, h.stateCookie(provider, "", -1)) // single use

	if e := params.Get("error"); e != "" {
		if e == "access_denied" || e == "user_cancelled_authorize" {
			h.fail(w, r, "cancelled")
		} else {
			h.Log.Warn("social: provider returned an error", "provider", provider, "error", e)
			h.fail(w, r, "failed")
		}
		return
	}
	if cookieErr != nil {
		h.fail(w, r, "expired")
		return
	}
	sd, err := h.Signer.Open(c.Value)
	state := params.Get("state")
	if err != nil || sd.Provider != provider || state == "" || !oauth.SameString(sd.State, state) {
		h.fail(w, r, "expired")
		return
	}
	code := params.Get("code")
	if code == "" {
		h.fail(w, r, "failed")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	profile, err := exchange(ctx, code, sd)
	if err != nil {
		h.Log.Warn("social: code exchange failed", "provider", provider, "error", err.Error())
		h.fail(w, r, "failed")
		return
	}

	u, pair, err := h.Auth.Svc.LoginWithSocial(ctx, service.SocialProfile{
		Provider:      provider,
		Subject:       profile.Subject,
		Email:         profile.Email,
		EmailVerified: profile.EmailVerified,
		Name:          profile.Name,
	})
	if err != nil {
		h.fail(w, r, reasonFor(err))
		if reasonFor(err) == "failed" {
			h.Log.Error("social: sign-in failed", "provider", provider, "error", err.Error())
		}
		return
	}

	h.Auth.setAuthCookies(w, pair)
	slug := h.Auth.lookupStoreSlug(r.Context(), u)
	http.Redirect(w, r, h.FrontendURL+landing(u, slug, sd.Next), http.StatusSeeOther)
}

func reasonFor(err error) string {
	var ae *apperr.Error
	if errors.As(err, &ae) {
		switch ae.Code {
		case apperr.CodeAccountSuspended:
			return "suspended"
		case service.CodeSocialEmailUnverified:
			return "email_unverified"
		case apperr.CodeForbidden:
			return "not_allowed"
		case apperr.CodeEmailTaken:
			return "email_taken"
		}
	}
	return "failed"
}

func (h *SocialHandler) fail(w http.ResponseWriter, r *http.Request, reason string) {
	http.Redirect(w, r, h.FrontendURL+"/login?error=social_"+reason, http.StatusSeeOther)
}

func stateCookieName(provider string) string { return "oauth_state_" + provider }

// The state cookie is host-only and scoped to the auth routes. Apple answers
// with a cross-site POST, and browsers only send such a cookie when it is
// SameSite=None (which needs HTTPS; Apple requires HTTPS anyway). Google
// comes back with a normal GET, so Lax is enough there.
func (h *SocialHandler) stateCookie(provider, value string, maxAge int) *http.Cookie {
	secure, same := h.Secure, http.SameSiteLaxMode
	if provider == models.ProviderApple {
		secure, same = true, http.SameSiteNoneMode
	}
	return &http.Cookie{
		Name:     stateCookieName(provider),
		Value:    value,
		Path:     httpx.RefreshCookiePath,
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   secure,
		SameSite: same,
	}
}

// safeNext only allows paths on our own site (no open redirect).
func safeNext(next string) string {
	if len(next) == 0 || len(next) > 512 || next[0] != '/' || strings.HasPrefix(next, "//") ||
		strings.ContainsAny(next, "\\\r\n") {
		return ""
	}
	return next
}

// landing mirrors the frontend's role-based redirect after login.
func landing(u *models.User, storeSlug, next string) string {
	if next != "" {
		return next
	}
	if u.Role == models.RoleSeller {
		if storeSlug != "" {
			return "/seller/" + storeSlug + "/dashboard"
		}
		return "/seller/setup"
	}
	return "/account"
}
