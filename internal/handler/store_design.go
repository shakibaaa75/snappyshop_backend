package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/cache"
	"shopbd/backend/internal/httpx"
	"shopbd/backend/internal/middleware"
	"shopbd/backend/internal/revalidate"
	"shopbd/backend/internal/service"
)

type StoreDesignHandler struct {
	Svc *service.StoreDesignService
	Log *slog.Logger

	// Cache is the shared cache (Redis). Nil = every public request reads
	// MongoDB, exactly as before.
	Cache *cache.Cache

	// Revalidate tells the Next.js storefront to refresh given stores at
	// once. Nil = Next refreshes on its own timer only.
	Revalidate *revalidate.Client
}

// InvalidateIdentity drops the cached slug lookups on every API instance and
// the cached design of the one store involved. The router calls it when a shop
// name/URL changes or an admin changes a seller's visibility. Old and new
// addresses are sent to Next so the retired address stops being served.
func (h *StoreDesignHandler) InvalidateIdentity(sellerID string, slugs ...string) {
	invalidateSlugs(h.Cache)
	invalidateStoreDesign(h.Cache, sellerID)
	h.Revalidate.Notify(slugs...)
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
	// Is this the first publish? Then a cached "not found" for the address
	// must go too, so the new store never shows a 404. Unknown means yes.
	firstPublish := true
	if slug, err := h.Svc.CurrentSlugBySeller(r.Context(), claims.UserID); err == nil && slug != "" {
		if id, err := h.Svc.SellerIDBySlug(r.Context(), slug); err == nil && id != "" {
			firstPublish = false
		}
	}
	if err := h.Svc.Publish(r.Context(), claims.UserID, body.Design); err != nil {
		h.fail(w, r, err)
		return
	}
	if firstPublish {
		invalidateSlugs(h.Cache)
	}
	invalidateStoreDesign(h.Cache, claims.UserID) // only THIS store's design
	if slug, err := h.Svc.CurrentSlugBySeller(r.Context(), claims.UserID); err == nil {
		h.Revalidate.Notify(slug)
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
//
// The published design is the same for every visitor, so it is shared through
// the cache. The address is checked first through the shared slug lookup (a
// shop name/URL change and an admin visibility change invalidate slugsTag, so
// a retired URL answers 404 at once). The design itself is cached per store,
// so a publish only clears that one store.
//
// A 200 answer carries Cache-Control and an ETag so Next.js and a CDN can
// reuse it; a 404 never does.
func (h *StoreDesignHandler) PublicStore(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	design, err := h.publicDesign(r.Context(), slug)
	if err != nil {
		if errors.Is(err, cache.ErrBusy) {
			w.Header().Set("Retry-After", "2")
			h.fail(w, r, apperr.New(http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE",
				"The store is busy. Please try again shortly."))
			return
		}
		h.fail(w, r, err)
		return
	}
	if design == nil {
		h.fail(w, r, apperr.NotFound())
		return
	}
	writeCachedJSON(w, r, designResponse{Design: design})
}

// publicDesign returns the published design for a slug, or nil when unknown.
func (h *StoreDesignHandler) publicDesign(ctx context.Context, slug string) (map[string]any, error) {
	load := func(ctx context.Context) (map[string]any, error) {
		d, err := h.Svc.PublicStore(ctx, slug)
		if err != nil {
			return nil, err
		}
		if d == nil {
			return nil, cache.ErrNotFound
		}
		return d, nil
	}
	direct := func() (map[string]any, error) {
		d, err := load(ctx)
		if errors.Is(err, cache.ErrNotFound) {
			return nil, nil
		}
		return d, err
	}
	key := strings.ToLower(strings.TrimSpace(slug))
	if h.Cache == nil || key == "" {
		return direct()
	}
	// 1. Resolve the address (shared tag): unknown, retired, unpublished and
	//    hidden stores stop here with nil, exactly as before.
	sellerID, err := cachedSellerID(ctx, h.Cache, h.Svc, key)
	if err != nil {
		return nil, err
	}
	if sellerID == "" {
		return nil, nil
	}
	// 2. The design, cached under this store's own tag. The address is part
	//    of the key, so a renamed store starts from a fresh entry.
	ckey, kerr := cache.ScopedKey(sellerID, "design", key)
	if kerr != nil {
		return direct() // unusual seller id: skip the cache, never fail
	}
	d, err := cache.Get(ctx, h.Cache, "storefront", ckey, storeDesignTag(sellerID), load)
	switch {
	case err == nil:
		return d, nil
	case errors.Is(err, cache.ErrNotFound):
		return nil, nil
	case errors.Is(err, cache.ErrInvalidKey):
		return direct()
	}
	return nil, err
}
