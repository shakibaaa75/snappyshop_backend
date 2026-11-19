package handler

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"shopbd/backend/internal/httpx"
	"shopbd/backend/internal/service"
)

// GetSellerSubscription — GET /admin/sellers/{id}/subscription
func (h *AdminHandler) GetSellerSubscription(w http.ResponseWriter, r *http.Request) {
	view, err := h.Svc.SellerSubscription(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, view)
}

type assignSubBody struct {
	Plan     string `json:"plan"`     // pro | business
	Interval string `json:"interval"` // monthly | yearly
	Days     int    `json:"days"`     // optional custom length; 0 = one interval
	Free     bool   `json:"free"`     // true = granted for free
	Extend   bool   `json:"extend"`   // add to the current period of the same plan
	Reason   string `json:"reason"`
}

// AssignSubscription — POST /admin/sellers/{id}/subscription
func (h *AdminHandler) AssignSubscription(w http.ResponseWriter, r *http.Request) {
	adminID, ok := adminClaims(r)
	if !ok {
		return
	}
	var body assignSubBody
	if err := httpx.DecodeJSON(w, r, &body, httpx.MaxBodyBytes); err != nil {
		h.fail(w, r, err)
		return
	}
	view, err := h.Svc.AssignSubscription(r.Context(), service.AssignInput{
		SellerID: chi.URLParam(r, "id"), AdminID: adminID,
		Plan: body.Plan, Interval: body.Interval, Days: body.Days,
		Free: body.Free, ExtendOld: body.Extend,
	}, body.Reason, clientIP(r))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, view)
}

type subActiveBody struct {
	Active bool   `json:"active"`
	Reason string `json:"reason"`
}

// SetSubscriptionActive — POST /admin/sellers/{id}/subscription/active
func (h *AdminHandler) SetSubscriptionActive(w http.ResponseWriter, r *http.Request) {
	adminID, ok := adminClaims(r)
	if !ok {
		return
	}
	var body subActiveBody
	if err := httpx.DecodeJSON(w, r, &body, httpx.MaxBodyBytes); err != nil {
		h.fail(w, r, err)
		return
	}
	view, err := h.Svc.SetSubscriptionActive(r.Context(), chi.URLParam(r, "id"),
		adminID, body.Reason, clientIP(r), body.Active)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, view)
}

type extendSubBody struct {
	Days   int    `json:"days"`
	Reason string `json:"reason"`
}

// ExtendSubscription — POST /admin/sellers/{id}/subscription/extend
func (h *AdminHandler) ExtendSubscription(w http.ResponseWriter, r *http.Request) {
	adminID, ok := adminClaims(r)
	if !ok {
		return
	}
	var body extendSubBody
	if err := httpx.DecodeJSON(w, r, &body, httpx.MaxBodyBytes); err != nil {
		h.fail(w, r, err)
		return
	}
	view, err := h.Svc.ExtendSubscription(r.Context(), chi.URLParam(r, "id"),
		adminID, body.Reason, clientIP(r), body.Days)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, view)
}

type removeSubBody struct {
	Reason string `json:"reason"`
}

// RemoveSubscription — POST /admin/sellers/{id}/subscription/remove
func (h *AdminHandler) RemoveSubscription(w http.ResponseWriter, r *http.Request) {
	adminID, ok := adminClaims(r)
	if !ok {
		return
	}
	var body removeSubBody
	if err := httpx.DecodeJSON(w, r, &body, httpx.MaxBodyBytes); err != nil {
		h.fail(w, r, err)
		return
	}
	view, err := h.Svc.RemoveSubscription(r.Context(), chi.URLParam(r, "id"),
		adminID, body.Reason, clientIP(r))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, view)
}

type revokeOfferBody struct {
	Reason string `json:"reason"`
}

// RevokeSubscriptionOffer — POST /admin/sellers/{id}/subscription/grants/{grantId}/revoke
// Withdraws a free plan the seller has not claimed yet.
func (h *AdminHandler) RevokeSubscriptionOffer(w http.ResponseWriter, r *http.Request) {
	adminID, ok := adminClaims(r)
	if !ok {
		return
	}
	var body revokeOfferBody
	if err := httpx.DecodeJSON(w, r, &body, httpx.MaxBodyBytes); err != nil {
		h.fail(w, r, err)
		return
	}
	view, err := h.Svc.RevokeOffer(r.Context(), chi.URLParam(r, "id"), chi.URLParam(r, "grantId"),
		adminID, body.Reason, clientIP(r))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, view)
}
