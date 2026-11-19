// Package handler contains the HTTP handlers.
package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/httpx"
	"shopbd/backend/internal/middleware"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/service"
	"shopbd/backend/internal/validation"
)

// UserDTO matches the frontend `User` type (plus an optional name).
type UserDTO struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Email     string `json:"email"`
	Role      string `json:"role"`
	StoreSlug string `json:"storeSlug,omitempty"`
	CreatedAt string `json:"createdAt"`
}

type userResponse struct {
	User UserDTO `json:"user"`
}

func toUserDTO(u *models.User, storeSlug string) UserDTO {
	return UserDTO{
		ID:        u.ID,
		Name:      u.Name,
		Email:     u.Email,
		Role:      string(u.Role),
		StoreSlug: storeSlug,
		CreatedAt: u.CreatedAt.UTC().Format(time.RFC3339),
	}
}

type AuthHandler struct {
	Svc          *service.AuthService
	StoreDesign  *service.StoreDesignService
	Cookies      httpx.CookieSettings
	EmailLimiter *middleware.KeyedLimiter
	Log          *slog.Logger
}

type registerRequest struct {
	Name              string `json:"name"`
	Email             string `json:"email"`
	Password          string `json:"password"`
	ConfirmPassword   string `json:"confirmPassword"`
	Role              string `json:"role"`
	VerificationToken string `json:"verificationToken"`
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (h *AuthHandler) setAuthCookies(w http.ResponseWriter, p *service.TokenPair) {
	h.Cookies.SetSession(w, p.Access, h.Svc.AccessTTL())
	h.Cookies.SetRefresh(w, p.Refresh, h.Svc.RefreshTTL())
}

func (h *AuthHandler) fail(w http.ResponseWriter, r *http.Request, err error) {
	httpx.WriteError(w, r, err, h.Log)
}

// lookupStoreSlug returns the seller's store slug, or "" if they haven't
// set one up yet (or if the user isn't a seller).
func (h *AuthHandler) lookupStoreSlug(ctx context.Context, u *models.User) string {
	if h.StoreDesign == nil || u == nil || u.Role != models.RoleSeller {
		return ""
	}
	design, err := h.StoreDesign.GetDraft(ctx, u.ID)
	if err != nil || design == nil {
		return ""
	}
	slug, _ := design["storeSlug"].(string)
	return slug
}

func (h *AuthHandler) Register(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if err := httpx.DecodeJSON(w, r, &req, httpx.MaxBodyBytes); err != nil {
		h.fail(w, r, err)
		return
	}
	u, pair, err := h.Svc.Register(r.Context(), validation.RegisterInput{
		Name: req.Name, Email: req.Email, Password: req.Password,
		ConfirmPassword: req.ConfirmPassword, Role: req.Role,
		VerificationToken: req.VerificationToken,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.setAuthCookies(w, pair)
	slug := h.lookupStoreSlug(r.Context(), u)
	httpx.WriteJSON(w, http.StatusCreated, userResponse{User: toUserDTO(u, slug)})
}

func (h *AuthHandler) Login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := httpx.DecodeJSON(w, r, &req, httpx.MaxBodyBytes); err != nil {
		h.fail(w, r, err)
		return
	}
	if h.EmailLimiter != nil {
		if key := validation.NormalizeEmail(req.Email); key != "" {
			if ok, wait := h.EmailLimiter.Allow(key); !ok {
				h.fail(w, r, apperr.RateLimited(wait))
				return
			}
		}
	}
	u, pair, err := h.Svc.Login(r.Context(), validation.LoginInput{Email: req.Email, Password: req.Password})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.setAuthCookies(w, pair)
	slug := h.lookupStoreSlug(r.Context(), u)
	httpx.WriteJSON(w, http.StatusOK, userResponse{User: toUserDTO(u, slug)})
}

func (h *AuthHandler) Refresh(w http.ResponseWriter, r *http.Request) {
	raw := ""
	if c, err := r.Cookie(httpx.RefreshCookie); err == nil {
		raw = c.Value
	}
	u, pair, err := h.Svc.Refresh(r.Context(), raw)
	if err != nil {
		var ae *apperr.Error
		if errors.As(err, &ae) && (ae.Status == http.StatusUnauthorized || ae.Status == http.StatusForbidden) {
			h.Cookies.Clear(w)
		}
		h.fail(w, r, err)
		return
	}
	h.setAuthCookies(w, pair)
	slug := h.lookupStoreSlug(r.Context(), u)
	httpx.WriteJSON(w, http.StatusOK, userResponse{User: toUserDTO(u, slug)})
}

func (h *AuthHandler) Logout(w http.ResponseWriter, r *http.Request) {
	raw := ""
	if c, err := r.Cookie(httpx.RefreshCookie); err == nil {
		raw = c.Value
	}
	if err := h.Svc.Logout(r.Context(), raw); err != nil {
		h.fail(w, r, err)
		return
	}
	h.Cookies.Clear(w)
	w.WriteHeader(http.StatusNoContent)
}

func (h *AuthHandler) Me(w http.ResponseWriter, r *http.Request) {
	claims, ok := middleware.ClaimsFrom(r.Context())
	if !ok {
		h.fail(w, r, apperr.Unauthenticated("Authentication required"))
		return
	}
	u, err := h.Svc.Me(r.Context(), claims.UserID)
	if err != nil {
		h.Cookies.Clear(w)
		h.fail(w, r, err)
		return
	}
	slug := h.lookupStoreSlug(r.Context(), u)
	httpx.WriteJSON(w, http.StatusOK, userResponse{User: toUserDTO(u, slug)})
}
