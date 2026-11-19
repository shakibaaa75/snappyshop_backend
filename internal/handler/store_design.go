package handler

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/httpx"
	"shopbd/backend/internal/middleware"
	"shopbd/backend/internal/service"
)

type StoreDesignHandler struct {
	Svc *service.StoreDesignService
	Log *slog.Logger
}

type designBody struct {
	Design map[string]any `json:"design"`
}

type designResponse struct {
	Design map[string]any `json:"design"`
}

// draftResponse includes slug-change cooldown info for the seller UI.
type draftResponse struct {
	Status           string         `json:"status,omitempty"`
	Design           map[string]any `json:"design"`
	CanChangeSlug    bool           `json:"canChangeSlug"`
	NextSlugChangeAt *time.Time     `json:"nextSlugChangeAt,omitempty"`
	CurrentSlug      string         `json:"currentSlug,omitempty"`

	// Shop-name rules (one change after registration, 13-day lock).
	CurrentName          string     `json:"currentName,omitempty"`
	CanChangeName        bool       `json:"canChangeName"`
	NameChangesRemaining int        `json:"nameChangesRemaining"`
	NextNameChangeAt     *time.Time `json:"nextNameChangeAt,omitempty"`
	NameChangeDisclaimer string     `json:"nameChangeDisclaimer,omitempty"`
}

// withMeta copies the change-lock metadata into a response.
func withMeta(r draftResponse, m service.SlugChangeInfo) draftResponse {
	r.CanChangeSlug = m.CanChangeSlug
	r.NextSlugChangeAt = m.NextSlugChangeAt
	r.CurrentSlug = m.CurrentSlug
	r.CurrentName = m.CurrentName
	r.CanChangeName = m.CanChangeName
	r.NameChangesRemaining = m.NameChangesRemaining
	r.NextNameChangeAt = m.NextNameChangeAt
	r.NameChangeDisclaimer = m.NameChangeDisclaimer
	return r
}

func (h *StoreDesignHandler) fail(w http.ResponseWriter, r *http.Request, err error) {
	httpx.WriteError(w, r, err, h.Log)
}

// GetDraft — GET /api/v1/seller/store-design
func (h *StoreDesignHandler) GetDraft(w http.ResponseWriter, r *http.Request) {
	claims, ok := middleware.ClaimsFrom(r.Context())
	if !ok {
		h.fail(w, r, apperr.Unauthenticated("Authentication required"))
		return
	}
	res, err := h.Svc.GetDraftWithMeta(r.Context(), claims.UserID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, withMeta(draftResponse{Design: res.Design}, res.Meta))
}

// SaveDraft — PUT /api/v1/seller/store-design
func (h *StoreDesignHandler) SaveDraft(w http.ResponseWriter, r *http.Request) {
	claims, ok := middleware.ClaimsFrom(r.Context())
	if !ok {
		h.fail(w, r, apperr.Unauthenticated("Authentication required"))
		return
	}
	var body designBody
	if err := httpx.DecodeJSON(w, r, &body, httpx.MaxBodyBytes); err != nil {
		h.fail(w, r, err)
		return
	}
	if body.Design == nil {
		h.fail(w, r, apperr.BadRequest("Missing design"))
		return
	}
	if err := h.Svc.SaveDraft(r.Context(), claims.UserID, body.Design); err != nil {
		h.fail(w, r, err)
		return
	}
	// Return fresh cooldown metadata so the UI can update immediately.
	res, err := h.Svc.GetDraftWithMeta(r.Context(), claims.UserID)
	if err != nil {
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "saved"})
		return
	}
	httpx.WriteJSON(w, http.StatusOK, withMeta(draftResponse{Status: "saved", Design: res.Design}, res.Meta))
}

// Publish — POST /api/v1/seller/store-design/publish
func (h *StoreDesignHandler) Publish(w http.ResponseWriter, r *http.Request) {
	claims, ok := middleware.ClaimsFrom(r.Context())
	if !ok {
		h.fail(w, r, apperr.Unauthenticated("Authentication required"))
		return
	}
	var body designBody
	if err := httpx.DecodeJSON(w, r, &body, httpx.MaxBodyBytes); err != nil {
		h.fail(w, r, err)
		return
	}
	if body.Design == nil {
		h.fail(w, r, apperr.BadRequest("Missing design"))
		return
	}
	if err := h.Svc.Publish(r.Context(), claims.UserID, body.Design); err != nil {
		h.fail(w, r, err)
		return
	}
	res, err := h.Svc.GetDraftWithMeta(r.Context(), claims.UserID)
	if err != nil {
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "published"})
		return
	}
	httpx.WriteJSON(w, http.StatusOK, withMeta(draftResponse{Status: "published", Design: res.Design}, res.Meta))
}

// ChangeSlug — PUT /api/v1/seller/store-address
// Body: { "storeSlug": "new-address" }
// Previous address becomes permanently invalid (no redirect, no store page).
func (h *StoreDesignHandler) ChangeSlug(w http.ResponseWriter, r *http.Request) {
	claims, ok := middleware.ClaimsFrom(r.Context())
	if !ok {
		h.fail(w, r, apperr.Unauthenticated("Authentication required"))
		return
	}
	var body struct {
		StoreSlug string `json:"storeSlug"`
	}
	if err := httpx.DecodeJSON(w, r, &body, httpx.MaxBodyBytes); err != nil {
		h.fail(w, r, err)
		return
	}
	info, err := h.Svc.ChangeSlug(r.Context(), claims.UserID, body.StoreSlug)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, withMeta(draftResponse{Status: "updated"}, *info))
}

// ChangeName — PUT /api/v1/seller/store-name
// Body: { "storeName": "New name", "storeSlug": "optional-url",
//
//	"acknowledgeLock": true }
//
// The seller must have accepted the 13-day disclaimer (acknowledgeLock). The
// shop URL is rebuilt from the new name at once; the old URL returns 404.
func (h *StoreDesignHandler) ChangeName(w http.ResponseWriter, r *http.Request) {
	claims, ok := middleware.ClaimsFrom(r.Context())
	if !ok {
		h.fail(w, r, apperr.Unauthenticated("Authentication required"))
		return
	}
	var body struct {
		StoreName       string `json:"storeName"`
		StoreSlug       string `json:"storeSlug"`
		AcknowledgeLock bool   `json:"acknowledgeLock"`
	}
	if err := httpx.DecodeJSON(w, r, &body, httpx.MaxBodyBytes); err != nil {
		h.fail(w, r, err)
		return
	}
	info, err := h.Svc.ChangeStoreName(r.Context(), claims.UserID, service.ChangeNameInput{
		Name:             body.StoreName,
		Slug:             body.StoreSlug,
		AcknowledgedLock: body.AcknowledgeLock,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, withMeta(draftResponse{Status: "updated"}, *info))
}

// PublicStore — GET /api/v1/store/:slug (no auth)
func (h *StoreDesignHandler) PublicStore(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	design, err := h.Svc.PublicStore(r.Context(), slug)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if design == nil {
		h.fail(w, r, apperr.NotFound())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, designResponse{Design: design})
}
