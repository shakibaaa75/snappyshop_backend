package handler

import (
	"log/slog"
	"net/http"

	"shopbd/backend/internal/httpx"
	"shopbd/backend/internal/service"
)

// EmailVerificationHandler serves the sign-up email verification endpoints.
type EmailVerificationHandler struct {
	Svc *service.EmailVerificationService
	Log *slog.Logger
}

type sendCodeRequest struct {
	Email string `json:"email"`
}

type verifyCodeRequest struct {
	Email string `json:"email"`
	Code  string `json:"code"`
}

func (h *EmailVerificationHandler) fail(w http.ResponseWriter, r *http.Request, err error) {
	httpx.WriteError(w, r, err, h.Log)
}

// SendCode handles POST /auth/email/send-code.
func (h *EmailVerificationHandler) SendCode(w http.ResponseWriter, r *http.Request) {
	var req sendCodeRequest
	if err := httpx.DecodeJSON(w, r, &req, httpx.MaxBodyBytes); err != nil {
		h.fail(w, r, err)
		return
	}
	resendAfter, err := h.Svc.SendCode(r.Context(), req.Email)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]int{"resendAfter": resendAfter})
}

// VerifyCode handles POST /auth/email/verify-code.
func (h *EmailVerificationHandler) VerifyCode(w http.ResponseWriter, r *http.Request) {
	var req verifyCodeRequest
	if err := httpx.DecodeJSON(w, r, &req, httpx.MaxBodyBytes); err != nil {
		h.fail(w, r, err)
		return
	}
	token, err := h.Svc.VerifyCode(r.Context(), req.Email, req.Code)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"verificationToken": token})
}
