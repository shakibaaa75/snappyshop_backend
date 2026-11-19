package handler

import (
	"log/slog"
	"net/http"
	"time"

	"shopbd/backend/internal/httpx"
	"shopbd/backend/internal/service"
)

type AdminAuthHandler struct {
	Svc     *service.AdminAuthService
	Cookies httpx.CookieSettings
	Log     *slog.Logger
}

type adminLoginReq struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type adminOTPReq struct {
	Email string `json:"email"`
	Code  string `json:"code"`
}

func (h *AdminAuthHandler) Login(w http.ResponseWriter, r *http.Request) {
	var req adminLoginReq
	if err := httpx.DecodeJSON(w, r, &req, httpx.MaxBodyBytes); err != nil {
		httpx.WriteError(w, r, err, h.Log)
		return
	}
	if err := h.Svc.Login(r.Context(), req.Email, req.Password); err != nil {
		httpx.WriteError(w, r, err, h.Log)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"message": "Verification code sent to your email",
	})
}

func (h *AdminAuthHandler) VerifyOTP(w http.ResponseWriter, r *http.Request) {
	var req adminOTPReq
	if err := httpx.DecodeJSON(w, r, &req, httpx.MaxBodyBytes); err != nil {
		httpx.WriteError(w, r, err, h.Log)
		return
	}
	u, pair, err := h.Svc.VerifyOTP(r.Context(), req.Email, req.Code)
	if err != nil {
		httpx.WriteError(w, r, err, h.Log)
		return
	}
	accessTTL := time.Until(pair.AccessExpiresAt)
	if accessTTL < time.Minute {
		accessTTL = 15 * time.Minute
	}
	refreshTTL := time.Until(pair.RefreshExpiresAt)
	if refreshTTL < time.Minute {
		refreshTTL = 7 * 24 * time.Hour
	}
	h.Cookies.SetSession(w, pair.Access, accessTTL)
	h.Cookies.SetRefresh(w, pair.Refresh, refreshTTL)
	if gate, ttl, err := h.Svc.IssueGateToken(); err == nil {
		h.Cookies.SetAdminGate(w, gate, ttl)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"user": map[string]any{
			"id":        u.ID,
			"name":      u.Name,
			"email":     u.Email,
			"role":      string(u.Role),
			"createdAt": u.CreatedAt.UTC().Format(time.RFC3339),
		},
	})
}

// Status reports whether the admin 2FA gate cookie is present and valid.
func (h *AdminAuthHandler) Status(w http.ResponseWriter, r *http.Request) {
	required := h.Svc != nil && h.Svc.Enabled()
	ok := !required
	if required {
		if c, err := r.Cookie(httpx.AdminGateCookie); err == nil {
			ok = h.Svc.ValidGateToken(c.Value)
		}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": ok, "required": required})
}

// LogoutAdmin clears only the admin gate cookie.
func (h *AdminAuthHandler) LogoutAdmin(w http.ResponseWriter, r *http.Request) {
	h.Cookies.ClearAdminGate(w)
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
