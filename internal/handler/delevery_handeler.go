package handler

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/httpx"
	"shopbd/backend/internal/middleware"
	"shopbd/backend/internal/service"
)

// DeliveryHandler serves a seller's delivery settings (authenticated) and the
// public per-store charges the storefront shows on product pages and in the
// cart.
type DeliveryHandler struct {
	Svc *service.DeliveryService
	Log *slog.Logger
}

// deliveryBodyLimit leaves room for the maximum number of per-product
// overrides (each is roughly 100 bytes of JSON).
const deliveryBodyLimit = 1 << 19 // 512 KiB

// Get — GET /api/v1/seller/delivery-settings
func (h *DeliveryHandler) Get(w http.ResponseWriter, r *http.Request) {
	claims, ok := middleware.ClaimsFrom(r.Context())
	if !ok {
		httpx.WriteError(w, r, apperr.Unauthenticated("Authentication required"), h.Log)
		return
	}
	out, err := h.Svc.Get(r.Context(), claims.UserID)
	if err != nil {
		httpx.WriteError(w, r, err, h.Log)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// Save — PUT /api/v1/seller/delivery-settings
// The seller ID always comes from the login token, never from the request, so
// a seller can only ever change their own charges.
func (h *DeliveryHandler) Save(w http.ResponseWriter, r *http.Request) {
	claims, ok := middleware.ClaimsFrom(r.Context())
	if !ok {
		httpx.WriteError(w, r, apperr.Unauthenticated("Authentication required"), h.Log)
		return
	}
	var body service.DeliverySettings
	if err := httpx.DecodeJSON(w, r, &body, deliveryBodyLimit); err != nil {
		httpx.WriteError(w, r, err, h.Log)
		return
	}
	out, err := h.Svc.Save(r.Context(), claims.UserID, body)
	if err != nil {
		httpx.WriteError(w, r, err, h.Log)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// Public — GET /api/v1/store/{slug}/delivery-settings (no auth)
func (h *DeliveryHandler) Public(w http.ResponseWriter, r *http.Request) {
	out, err := h.Svc.PublicBySlug(r.Context(), chi.URLParam(r, "slug"))
	if err != nil {
		httpx.WriteError(w, r, err, h.Log)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}
