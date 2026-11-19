package handler

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/httpx"
	"shopbd/backend/internal/middleware"
	"shopbd/backend/internal/shipping"
)

// ShippingHandler wires parcel booking. The seller ID always comes from the
// authenticated claims.
type ShippingHandler struct {
	Svc          *shipping.Service
	ProviderName func(provider string) string
	Log          *slog.Logger
}

type shipBody struct {
	ConnectionID string `json:"connectionId"`
}

type shipManyBody struct {
	OrderIDs     []string `json:"orderIds"`
	ConnectionID string   `json:"connectionId"`
}

func (h *ShippingHandler) fail(w http.ResponseWriter, r *http.Request, err error) {
	httpx.WriteError(w, r, err, h.Log)
}

func (h *ShippingHandler) sellerID(w http.ResponseWriter, r *http.Request) (string, bool) {
	claims, ok := middleware.ClaimsFrom(r.Context())
	if !ok {
		h.fail(w, r, apperr.Unauthenticated("Authentication required"))
		return "", false
	}
	return claims.UserID, true
}

// Ship — POST /api/v1/seller/orders/{id}/ship
func (h *ShippingHandler) Ship(w http.ResponseWriter, r *http.Request) {
	seller, ok := h.sellerID(w, r)
	if !ok {
		return
	}
	var body shipBody
	if err := httpx.DecodeJSON(w, r, &body, 4<<10); err != nil {
		h.fail(w, r, err)
		return
	}
	if body.ConnectionID == "" {
		h.fail(w, r, apperr.Validation(map[string]string{"connectionId": "Choose a courier"}))
		return
	}
	v, err := h.Svc.Ship(r.Context(), seller, chi.URLParam(r, "id"), body.ConnectionID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"shipment": v})
}

// ShipMany — POST /api/v1/seller/orders/ship
func (h *ShippingHandler) ShipMany(w http.ResponseWriter, r *http.Request) {
	seller, ok := h.sellerID(w, r)
	if !ok {
		return
	}
	var body shipManyBody
	if err := httpx.DecodeJSON(w, r, &body, 16<<10); err != nil {
		h.fail(w, r, err)
		return
	}
	if body.ConnectionID == "" {
		h.fail(w, r, apperr.Validation(map[string]string{"connectionId": "Choose a courier"}))
		return
	}
	res, err := h.Svc.ShipMany(r.Context(), seller, body.OrderIDs, body.ConnectionID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"results": res})
}

// List — GET /api/v1/seller/shipments
func (h *ShippingHandler) List(w http.ResponseWriter, r *http.Request) {
	seller, ok := h.sellerID(w, r)
	if !ok {
		return
	}
	items, err := h.Svc.List(r.Context(), seller, h.ProviderName)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"shipments": items})
}
