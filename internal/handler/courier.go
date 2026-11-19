package handler

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/courierconn"
	"shopbd/backend/internal/httpx"
	"shopbd/backend/internal/middleware"
)

// CourierHandler wires the seller courier-connection endpoints. The seller
// ID is always taken from the authenticated claims, never from the request.
type CourierHandler struct {
	Svc *courierconn.Service
	Log *slog.Logger
}

const courierBodyLimit = 16 << 10 // credentials are tiny; keep the limit small

type connectCourierBody struct {
	Provider    string            `json:"provider"`
	Label       string            `json:"label"`
	Credentials map[string]string `json:"credentials"`
}

type updateCourierCredentialsBody struct {
	Credentials map[string]string `json:"credentials"`
}

func (h *CourierHandler) fail(w http.ResponseWriter, r *http.Request, err error) {
	httpx.WriteError(w, r, err, h.Log)
}

func (h *CourierHandler) sellerID(w http.ResponseWriter, r *http.Request) (string, bool) {
	claims, ok := middleware.ClaimsFrom(r.Context())
	if !ok {
		h.fail(w, r, apperr.Unauthenticated("Authentication required"))
		return "", false
	}
	return claims.UserID, true
}

// Providers — GET /api/v1/seller/couriers/providers
func (h *CourierHandler) Providers(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.sellerID(w, r); !ok {
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"providers": h.Svc.Providers()})
}

// List — GET /api/v1/seller/couriers
func (h *CourierHandler) List(w http.ResponseWriter, r *http.Request) {
	seller, ok := h.sellerID(w, r)
	if !ok {
		return
	}
	items, err := h.Svc.List(r.Context(), seller)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"connections": items})
}

// Connect — POST /api/v1/seller/couriers
func (h *CourierHandler) Connect(w http.ResponseWriter, r *http.Request) {
	seller, ok := h.sellerID(w, r)
	if !ok {
		return
	}
	var body connectCourierBody
	if err := httpx.DecodeJSON(w, r, &body, courierBodyLimit); err != nil {
		h.fail(w, r, err)
		return
	}
	v, err := h.Svc.Connect(r.Context(), seller, courierconn.ConnectInput{
		Provider:    body.Provider,
		Label:       body.Label,
		Credentials: body.Credentials,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"connection": v})
}

// Test — POST /api/v1/seller/couriers/{id}/test
func (h *CourierHandler) Test(w http.ResponseWriter, r *http.Request) {
	seller, ok := h.sellerID(w, r)
	if !ok {
		return
	}
	v, err := h.Svc.Test(r.Context(), seller, chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"connection": v})
}

// UpdateCredentials — PUT /api/v1/seller/couriers/{id}/credentials
func (h *CourierHandler) UpdateCredentials(w http.ResponseWriter, r *http.Request) {
	seller, ok := h.sellerID(w, r)
	if !ok {
		return
	}
	var body updateCourierCredentialsBody
	if err := httpx.DecodeJSON(w, r, &body, courierBodyLimit); err != nil {
		h.fail(w, r, err)
		return
	}
	v, err := h.Svc.UpdateCredentials(r.Context(), seller, chi.URLParam(r, "id"), body.Credentials)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"connection": v})
}

// Disconnect — DELETE /api/v1/seller/couriers/{id}
func (h *CourierHandler) Disconnect(w http.ResponseWriter, r *http.Request) {
	seller, ok := h.sellerID(w, r)
	if !ok {
		return
	}
	if err := h.Svc.Disconnect(r.Context(), seller, chi.URLParam(r, "id")); err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusNoContent, nil)
}
