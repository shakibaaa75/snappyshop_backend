package handler

import (
	"log/slog"
	"net/http"

	"shopbd/backend/internal/httpx"
	"shopbd/backend/internal/plans"
	"shopbd/backend/internal/service"
)

// PlanCatalogHandler serves the plan prices and features.
//
//	GET  /api/v1/plans              (public: the pricing page)
//	GET  /api/v1/admin/plans        (admin: editor)
//	PUT  /api/v1/admin/plans        (admin: save)
//	POST /api/v1/admin/plans/reset  (admin: back to the built-in plans)
type PlanCatalogHandler struct {
	Svc *service.PlanCatalogService
	Log *slog.Logger
}

func (h *PlanCatalogHandler) fail(w http.ResponseWriter, r *http.Request, err error) {
	httpx.WriteError(w, r, err, h.Log)
}

// Public — GET /plans
func (h *PlanCatalogHandler) Public(w http.ResponseWriter, r *http.Request) {
	// Prices must be fresh, so browsers and proxies should not keep a copy.
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, http.StatusOK, h.Svc.PublicView())
}

// AdminGet — GET /admin/plans
func (h *PlanCatalogHandler) AdminGet(w http.ResponseWriter, r *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, h.Svc.AdminView())
}

// AdminUpdate — PUT /admin/plans
func (h *PlanCatalogHandler) AdminUpdate(w http.ResponseWriter, r *http.Request) {
	adminID, ok := adminClaims(r)
	if !ok {
		return
	}
	var body plans.Config
	if err := httpx.DecodeJSON(w, r, &body, httpx.MaxBodyBytes); err != nil {
		h.fail(w, r, err)
		return
	}
	view, err := h.Svc.Update(r.Context(), adminID, body, clientIP(r))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, view)
}

// AdminReset — POST /admin/plans/reset
func (h *PlanCatalogHandler) AdminReset(w http.ResponseWriter, r *http.Request) {
	adminID, ok := adminClaims(r)
	if !ok {
		return
	}
	view, err := h.Svc.Reset(r.Context(), adminID, clientIP(r))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, view)
}
