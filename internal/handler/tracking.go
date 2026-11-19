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

// TrackingHandler serves a seller's pixel settings (authenticated) and the
// public per-store config the storefront needs to load the right pixels.
type TrackingHandler struct {
	Svc *service.TrackingService
	Log *slog.Logger
}

type trackingSettingsBody struct {
	MetaPixelID    string `json:"metaPixelId"`
	TikTokPixelID  string `json:"tiktokPixelId"`
	RequireConsent bool   `json:"requireConsent"`
}

// Get — GET /api/v1/seller/tracking-settings
func (h *TrackingHandler) Get(w http.ResponseWriter, r *http.Request) {
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

// Save — PUT /api/v1/seller/tracking-settings
// The seller ID always comes from the login token, never from the request, so
// a seller can only ever change their own store.
func (h *TrackingHandler) Save(w http.ResponseWriter, r *http.Request) {
	claims, ok := middleware.ClaimsFrom(r.Context())
	if !ok {
		httpx.WriteError(w, r, apperr.Unauthenticated("Authentication required"), h.Log)
		return
	}
	var body trackingSettingsBody
	if err := httpx.DecodeJSON(w, r, &body, 1<<12); err != nil {
		httpx.WriteError(w, r, err, h.Log)
		return
	}
	out, err := h.Svc.Save(r.Context(), claims.UserID, service.TrackingSettings{
		MetaPixelID:    body.MetaPixelID,
		TikTokPixelID:  body.TikTokPixelID,
		RequireConsent: body.RequireConsent,
	})
	if err != nil {
		httpx.WriteError(w, r, err, h.Log)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// Public — GET /api/v1/store/{slug}/tracking (no auth)
func (h *TrackingHandler) Public(w http.ResponseWriter, r *http.Request) {
	out, err := h.Svc.PublicBySlug(r.Context(), chi.URLParam(r, "slug"))
	if err != nil {
		httpx.WriteError(w, r, err, h.Log)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}
