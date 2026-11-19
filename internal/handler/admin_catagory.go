package handler

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/httpx"
	"shopbd/backend/internal/taxonomy"
)

// AdminCategoryHandler is the Category Management API (admin only).
type AdminCategoryHandler struct {
	Svc *taxonomy.Admin
	Log *slog.Logger
}

func (h *AdminCategoryHandler) fail(w http.ResponseWriter, r *http.Request, err error) {
	var taken *taxonomy.NameTakenError
	var has *taxonomy.HasProductsError
	switch {
	case errors.Is(err, taxonomy.ErrNotFound):
		err = apperr.NotFound()
	case errors.Is(err, taxonomy.ErrMainTypeLocked):
		err = apperr.BadRequest("Main types are fixed and cannot be changed")
	case errors.Is(err, taxonomy.ErrInvalidImage):
		err = apperr.BadRequest("Image address must start with https://")
	case errors.Is(err, taxonomy.ErrSameCategory):
		err = apperr.BadRequest("Choose a different category")
	case errors.Is(err, taxonomy.ErrDifferentMain):
		err = apperr.BadRequest("Both categories must be in the same main type. Move one first.")
	case errors.Is(err, taxonomy.ErrUnknownMain):
		err = apperr.BadRequest("Unknown main type")
	case errors.Is(err, taxonomy.ErrInvalidName):
		err = apperr.BadRequest(fmt.Sprintf("Name must be %d-%d characters and contain letters", taxonomy.MinNameLen, taxonomy.MaxNameLen))
	case errors.As(err, &taken):
		err = apperr.BadRequest(fmt.Sprintf("\"%s\" already exists in that main type (id: %s). Merge into it instead.", taken.Existing.Name, taken.Existing.ID))
	case errors.As(err, &has):
		err = apperr.BadRequest(fmt.Sprintf("This category has %d products. Choose a category to move them to.", has.Count))
	}
	httpx.WriteError(w, r, err, h.Log)
}

// List — GET /admin/categories : every category grouped by Main Type.
func (h *AdminCategoryHandler) List(w http.ResponseWriter, r *http.Request) {
	mains, unseen, err := h.Svc.List(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"mainTypes": mains, "unseenCount": unseen})
}

// New — GET /admin/categories/new : newly created categories (who, which
// store, when, where) with similar-name suggestions. unseenCount = badge.
func (h *AdminCategoryHandler) New(w http.ResponseWriter, r *http.Request) {
	items, err := h.Svc.Unseen(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"categories": items, "unseenCount": len(items)})
}

// Similar — GET /admin/categories/{id}/similar
func (h *AdminCategoryHandler) Similar(w http.ResponseWriter, r *http.Request) {
	s, err := h.Svc.Similar(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"similar": s})
}

// MarkSeen — POST /admin/categories/seen  {"ids":[...]}  (empty = all)
func (h *AdminCategoryHandler) MarkSeen(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IDs []string `json:"ids"`
	}
	if err := httpx.DecodeJSON(w, r, &body, httpx.MaxBodyBytes); err != nil {
		h.fail(w, r, err)
		return
	}
	if err := h.Svc.MarkSeen(r.Context(), body.IDs); err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// Rename — PUT /admin/categories/{id}  {"name":"Headphones"}
func (h *AdminCategoryHandler) Rename(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := httpx.DecodeJSON(w, r, &body, httpx.MaxBodyBytes); err != nil {
		h.fail(w, r, err)
		return
	}
	n, err := h.Svc.Rename(r.Context(), chi.URLParam(r, "id"), body.Name)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"id": n.ID, "name": n.Name, "mainId": n.ParentID})
}

// Merge — POST /admin/categories/{id}/merge  {"intoId":"fashion.hoodie"}
func (h *AdminCategoryHandler) Merge(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IntoID string `json:"intoId"`
	}
	if err := httpx.DecodeJSON(w, r, &body, httpx.MaxBodyBytes); err != nil {
		h.fail(w, r, err)
		return
	}
	moved, err := h.Svc.Merge(r.Context(), chi.URLParam(r, "id"), body.IntoID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"status": "ok", "productsMoved": moved})
}

// Move — POST /admin/categories/{id}/move  {"mainId":"electronics"}
func (h *AdminCategoryHandler) Move(w http.ResponseWriter, r *http.Request) {
	var body struct {
		MainID string `json:"mainId"`
	}
	if err := httpx.DecodeJSON(w, r, &body, httpx.MaxBodyBytes); err != nil {
		h.fail(w, r, err)
		return
	}
	n, err := h.Svc.Move(r.Context(), chi.URLParam(r, "id"), body.MainID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"id": n.ID, "mainId": n.ParentID})
}

// Delete — DELETE /admin/categories/{id}?moveTo=<categoryId>
// moveTo is required when the category still has products.
func (h *AdminCategoryHandler) Delete(w http.ResponseWriter, r *http.Request) {
	moved, err := h.Svc.Delete(r.Context(), chi.URLParam(r, "id"), r.URL.Query().Get("moveTo"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"status": "ok", "productsMoved": moved})
}

// SetImage — PUT /admin/categories/{id}/image  {"imageUrl":"https://..."}
// Works for main types and subcategories. An empty imageUrl removes the picture.
func (h *AdminCategoryHandler) SetImage(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ImageURL string `json:"imageUrl"`
	}
	if err := httpx.DecodeJSON(w, r, &body, httpx.MaxBodyBytes); err != nil {
		h.fail(w, r, err)
		return
	}
	n, err := h.Svc.SetImage(r.Context(), chi.URLParam(r, "id"), body.ImageURL)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"id": n.ID, "imageUrl": n.ImageURL})
}
