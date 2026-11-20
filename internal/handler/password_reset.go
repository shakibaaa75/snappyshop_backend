package handler

import (
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"
	"time"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/httpx"
	"shopbd/backend/internal/middleware"
	"shopbd/backend/internal/ratelimit"
	"shopbd/backend/internal/service"
	"shopbd/backend/internal/validation"
)

// Limits for the password reset endpoints, counted in Redis so they hold
// across API instances. They sit on top of the per-process limiter in the
// router; the five-guesses-per-code cap in the service is the hard guarantee.
const (
	forgotIPLimit      = 10
	forgotIPWindow     = time.Hour
	forgotEmailLimit   = 3 // reset emails per address, stops mail-bombing
	forgotEmailWindow  = 15 * time.Minute
	confirmIPLimit     = 20
	confirmIPWindow    = 15 * time.Minute
	confirmEmailLimit  = 10
	confirmEmailWindow = 15 * time.Minute
)

type PasswordResetHandler struct {
	Svc *service.PasswordResetService
	Log *slog.Logger

	// Limiter is shared between instances (Redis). Nil means only the
	// per-process limiter in the router applies.
	Limiter    ratelimit.Limiter
	TrustProxy bool
}

type forgotPasswordRequest struct {
	Email string `json:"email"`
}

type resetPasswordRequest struct {
	Email       string `json:"email"`
	Code        string `json:"code"`
	NewPassword string `json:"newPassword"`
}

func (h *PasswordResetHandler) fail(w http.ResponseWriter, r *http.Request, err error) {
	httpx.WriteError(w, r, err, h.Log)
}

// allow counts the request against an IP limit and an email limit. The email
// key is derived from what the caller typed, never from whether an account
// exists, so a refusal says nothing about the account. If Redis cannot be
// reached it lets the request through and logs: the service's per-code attempt
// cap still protects every code, and a Redis outage must not lock people out.
func (h *PasswordResetHandler) allow(r *http.Request, action, email string,
	ipLimit int, ipWindow time.Duration, emailLimit int, emailWindow time.Duration) (bool, time.Duration) {
	if h.Limiter == nil {
		return true, 0
	}
	sum := sha256.Sum256([]byte(validation.NormalizeEmail(email)))
	checks := []struct {
		key    string
		limit  int
		window time.Duration
	}{
		{"pwreset:" + action + ":ip:" + middleware.ClientIP(r, h.TrustProxy), ipLimit, ipWindow},
		{"pwreset:" + action + ":email:" + hex.EncodeToString(sum[:16]), emailLimit, emailWindow},
	}
	for _, c := range checks {
		d, err := h.Limiter.Allow(r.Context(), c.key, c.limit, c.window)
		if err != nil {
			h.Log.Warn("password reset rate limiter unavailable", "action", action, "error", err.Error())
			continue
		}
		if !d.Allowed {
			return false, d.RetryAfter
		}
	}
	return true, 0
}

func (h *PasswordResetHandler) ForgotPassword(w http.ResponseWriter, r *http.Request) {
	var req forgotPasswordRequest
	if err := httpx.DecodeJSON(w, r, &req, httpx.MaxBodyBytes); err != nil {
		h.fail(w, r, err)
		return
	}
	if req.Email == "" {
		h.fail(w, r, apperr.Validation(map[string]string{"email": "Email is required"}))
		return
	}
	// Nothing below depends on whether the address has an account, and the
	// service does all account-dependent work in the background, so the
	// response and its timing are the same for every address. Over the limit
	// the answer is still the same 204; the request is just not processed.
	if ok, _ := h.allow(r, "forgot", req.Email, forgotIPLimit, forgotIPWindow, forgotEmailLimit, forgotEmailWindow); ok {
		h.Svc.RequestResetAsync(req.Email)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *PasswordResetHandler) ResetPassword(w http.ResponseWriter, r *http.Request) {
	var req resetPasswordRequest
	if err := httpx.DecodeJSON(w, r, &req, httpx.MaxBodyBytes); err != nil {
		h.fail(w, r, err)
		return
	}
	if ok, wait := h.allow(r, "confirm", req.Email, confirmIPLimit, confirmIPWindow, confirmEmailLimit, confirmEmailWindow); !ok {
		h.fail(w, r, apperr.RateLimited(wait))
		return
	}
	if err := h.Svc.ConfirmReset(r.Context(), req.Email, req.Code, req.NewPassword); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type setPasswordRequest struct {
	Code        string `json:"code"`
	NewPassword string `json:"newPassword"`
}

// RequestSetPassword (POST /auth/password/set/request, signed in) emails a
// code to the account's own address. It is for accounts that signed up with
// Google / Apple and have no password yet.
func (h *PasswordResetHandler) RequestSetPassword(w http.ResponseWriter, r *http.Request) {
	claims, ok := middleware.ClaimsFrom(r.Context())
	if !ok {
		h.fail(w, r, apperr.Unauthenticated("Authentication required"))
		return
	}
	if allowed, wait := h.allow(r, "setpw-request", claims.UserID, forgotIPLimit, forgotIPWindow, forgotEmailLimit, forgotEmailWindow); !allowed {
		h.fail(w, r, apperr.RateLimited(wait))
		return
	}
	if err := h.Svc.RequestSetPassword(r.Context(), claims.UserID); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// SetPassword (POST /auth/password/set, signed in) adds a password to an
// account that has none, after the emailed code is confirmed. Google / Apple
// sign-in keeps working, and so does the current session.
func (h *PasswordResetHandler) SetPassword(w http.ResponseWriter, r *http.Request) {
	claims, ok := middleware.ClaimsFrom(r.Context())
	if !ok {
		h.fail(w, r, apperr.Unauthenticated("Authentication required"))
		return
	}
	var req setPasswordRequest
	if err := httpx.DecodeJSON(w, r, &req, httpx.MaxBodyBytes); err != nil {
		h.fail(w, r, err)
		return
	}
	if allowed, wait := h.allow(r, "setpw-confirm", claims.UserID, confirmIPLimit, confirmIPWindow, confirmEmailLimit, confirmEmailWindow); !allowed {
		h.fail(w, r, apperr.RateLimited(wait))
		return
	}
	if err := h.Svc.SetInitialPassword(r.Context(), claims.UserID, req.Code, req.NewPassword); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
