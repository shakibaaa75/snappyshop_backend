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
	"shopbd/backend/internal/validation"
)

// SocialHandler runs "Continue with Google / Apple".
//
// These routes are browser navigations, not fetch calls, so they never answer
// with JSON: success and failure both end in a redirect to the website.
// A failure lands on /login?error=social_<reason>.
type SocialHandler struct {
	Auth        *AuthHandler  // reused so the session cookies match normal login exactly
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
		Provider:            provider,
		Subject:             profile.Subject,
		Email:               profile.Email,
		EmailVerified:       profile.EmailVerified,
		Name:                profile.Name,
		AuthenticatedUserID: h.sessionUserID(r),
	})
	if err != nil {
		var lr *service.LinkRequiredError
		if errors.As(err, &lr) {
			// The email belongs to an existing account. Nothing is linked and
			// nobody is signed in yet: remember what the provider confirmed in
			// a signed cookie and ask the owner to prove who they are.
			sealed, serr := h.Signer.SealLink(oauth.PendingLink{
				Provider: lr.Link.Provider, Subject: lr.Link.Subject,
				UserID: lr.Link.UserID, Email: lr.Link.Email, Name: lr.Link.Name,
			})
			if serr != nil {
				h.Log.Error("social: seal link", "error", serr.Error())
				h.fail(w, r, "failed")
				return
			}
			http.SetCookie(w, h.linkCookie(sealed, int(linkCookieTTL.Seconds())))
			h.fail(w, r, "link_required")
			return
		}
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
		case service.CodeSocialAlreadyLinked:
			return "already_linked"
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

/* ---- linking Google / Apple to an existing account ---- */

const (
	linkCookieName = "oauth_link"
	linkCookieTTL  = 10 * time.Minute
)

// linkCookie carries the signed "pending link". It is host-only, HttpOnly and
// scoped to the auth routes, like the state cookie.
func (h *SocialHandler) linkCookie(value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     linkCookieName,
		Value:    value,
		Path:     httpx.RefreshCookiePath,
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   h.Secure,
		SameSite: http.SameSiteLaxMode,
	}
}

// sessionUserID returns the account this browser is signed in to, or "".
func (h *SocialHandler) sessionUserID(r *http.Request) string {
	c, err := r.Cookie(httpx.SessionCookie)
	if err != nil || c.Value == "" {
		return ""
	}
	claims, err := h.Auth.Svc.AuthenticateAccess(c.Value)
	if err != nil {
		return ""
	}
	return claims.UserID
}

func (h *SocialHandler) pending(r *http.Request) (*oauth.PendingLink, error) {
	c, err := r.Cookie(linkCookieName)
	if err != nil {
		return nil, apperr.BadRequest("No sign-in is waiting to be connected. Please try Continue with Google again.")
	}
	l, err := h.Signer.OpenLink(c.Value)
	if err != nil {
		return nil, apperr.BadRequest("This request expired. Please try Continue with Google again.")
	}
	return l, nil
}

type pendingLinkResponse struct {
	Provider string `json:"provider"`
	Email    string `json:"email"`
}

// PendingLink (GET /auth/social/pending) tells the login page which provider
// and account are waiting for the owner to confirm. It reveals nothing the
// person did not just prove to the provider: the email is their own.
func (h *SocialHandler) PendingLink(w http.ResponseWriter, r *http.Request) {
	l, err := h.pending(r)
	if err != nil {
		httpx.WriteError(w, r, err, h.Log)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, pendingLinkResponse{Provider: l.Provider, Email: l.Email})
}

// CancelLink (DELETE /auth/social/pending) forgets the pending link.
func (h *SocialHandler) CancelLink(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, h.linkCookie("", -1))
	w.WriteHeader(http.StatusNoContent)
}

type confirmLinkRequest struct {
	// Password of the existing account. May be empty when the browser already
	// has a session for that account (accounts without a password).
	Password string `json:"password"`
}

// ConfirmLink (POST /auth/social/link) connects the waiting Google / Apple
// identity to the existing account after the owner proves who they are, then
// signs them in to that same account.
func (h *SocialHandler) ConfirmLink(w http.ResponseWriter, r *http.Request) {
	var req confirmLinkRequest
	if err := httpx.DecodeJSON(w, r, &req, httpx.MaxBodyBytes); err != nil {
		httpx.WriteError(w, r, err, h.Log)
		return
	}
	l, err := h.pending(r)
	if err != nil {
		httpx.WriteError(w, r, err, h.Log)
		return
	}
	// Same per-email guess limit as the password login form, so this endpoint
	// cannot be used as a faster way to try passwords.
	if h.Auth.EmailLimiter != nil {
		if ok, wait := h.Auth.EmailLimiter.Allow(validation.NormalizeEmail(l.Email)); !ok {
			httpx.WriteError(w, r, apperr.RateLimited(wait), h.Log)
			return
		}
	}
	u, pair, err := h.Auth.Svc.LinkSocial(r.Context(), service.SocialLink{
		Provider: l.Provider, Subject: l.Subject, UserID: l.UserID, Email: l.Email, Name: l.Name,
	}, req.Password, h.sessionUserID(r))
	if err != nil {
		httpx.WriteError(w, r, err, h.Log)
		return
	}
	http.SetCookie(w, h.linkCookie("", -1)) // single use
	h.Auth.setAuthCookies(w, pair)
	slug := h.Auth.lookupStoreSlug(r.Context(), u)
	httpx.WriteJSON(w, http.StatusOK, userResponse{User: toUserDTO(u, slug)})
}
