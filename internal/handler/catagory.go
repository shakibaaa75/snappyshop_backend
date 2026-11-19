package handler

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/httpx"
	"shopbd/backend/internal/middleware"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/service"
)

// CategoryHandler wires the category endpoints.
type CategoryHandler struct {
	Svc         *service.CategoryService
	StoreDesign *service.StoreDesignService
	Log         *slog.Logger
}

// ---------- DTO types ----------

type CategoryDTO struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Slug     string `json:"slug"`
	Emoji    string `json:"emoji"`
	ImageURL string `json:"imageUrl"`
	Position int    `json:"position"`
}

func toCategoryDTO(c *models.Category) CategoryDTO {
	return CategoryDTO{
		ID:       c.ID,
		Name:     c.Name,
		Slug:     c.Slug,
		Emoji:    c.Emoji,
		ImageURL: c.ImageURL,
		Position: c.Position,
	}
}

// ---------- Request body ----------

type categoryBody struct {
	Name        string `json:"name"`
	Slug        string `json:"slug"`
	Emoji       string `json:"emoji"`
	ImageURL    string `json:"imageUrl"`
	RemoveImage bool   `json:"removeImage"` // clears the picture; empty imageUrl alone keeps it
	Position    int    `json:"position"`
}

func (b categoryBody) toInput() service.CategoryInput {
	return service.CategoryInput{
		Name:        b.Name,
		Slug:        b.Slug,
		Emoji:       b.Emoji,
		ImageURL:    b.ImageURL,
		RemoveImage: b.RemoveImage,
		Position:    b.Position,
	}
}

// ---------- HTTP handlers ----------

func (h *CategoryHandler) fail(w http.ResponseWriter, r *http.Request, err error) {
	httpx.WriteError(w, r, err, h.Log)
}

// List — GET /api/v1/seller/categories
func (h *CategoryHandler) List(w http.ResponseWriter, r *http.Request) {
	claims, ok := middleware.ClaimsFrom(r.Context())
	if !ok {
		h.fail(w, r, apperr.Unauthenticated("Authentication required"))
		return
	}
	items, err := h.Svc.List(r.Context(), claims.UserID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := make([]CategoryDTO, 0, len(items))
	for _, c := range items {
		out = append(out, toCategoryDTO(c))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"categories": out})
}

// Create — POST /api/v1/seller/categories
func (h *CategoryHandler) Create(w http.ResponseWriter, r *http.Request) {
	claims, ok := middleware.ClaimsFrom(r.Context())
	if !ok {
		h.fail(w, r, apperr.Unauthenticated("Authentication required"))
		return
	}
	var body categoryBody
	if err := httpx.DecodeJSON(w, r, &body, httpx.MaxBodyBytes); err != nil {
		h.fail(w, r, err)
		return
	}
	c, err := h.Svc.Create(r.Context(), claims.UserID, body.toInput())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"category": toCategoryDTO(c)})
}

// Update — PUT /api/v1/seller/categories/{id}
func (h *CategoryHandler) Update(w http.ResponseWriter, r *http.Request) {
	claims, ok := middleware.ClaimsFrom(r.Context())
	if !ok {
		h.fail(w, r, apperr.Unauthenticated("Authentication required"))
		return
	}
	id := chi.URLParam(r, "id")
	var body categoryBody
	if err := httpx.DecodeJSON(w, r, &body, httpx.MaxBodyBytes); err != nil {
		h.fail(w, r, err)
		return
	}
	c, err := h.Svc.Update(r.Context(), claims.UserID, id, body.toInput())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"category": toCategoryDTO(c)})
}

// Delete — DELETE /api/v1/seller/categories/{id}
func (h *CategoryHandler) Delete(w http.ResponseWriter, r *http.Request) {
	claims, ok := middleware.ClaimsFrom(r.Context())
	if !ok {
		h.fail(w, r, apperr.Unauthenticated("Authentication required"))
		return
	}
	id := chi.URLParam(r, "id")
	if err := h.Svc.Delete(r.Context(), claims.UserID, id); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// PublicList — GET /api/v1/store/{slug}/categories (public, no auth)
func (h *CategoryHandler) PublicList(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	if slug == "" {
		h.fail(w, r, apperr.BadRequest("Missing slug"))
		return
	}
	sellerID, err := h.StoreDesign.SellerIDBySlug(r.Context(), slug)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if sellerID == "" {
		h.fail(w, r, apperr.NotFound())
		return
	}
	items, err := h.Svc.List(r.Context(), sellerID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := make([]CategoryDTO, 0, len(items))
	for _, c := range items {
		out = append(out, toCategoryDTO(c))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"categories": out})
}
