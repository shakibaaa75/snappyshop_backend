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

// LookHandler serves Try the Look: the seller's editor API and the public page.
type LookHandler struct {
	Svc *service.LookService
	Log *slog.Logger
}

type lookBody struct {
	Title         string `json:"title"`
	Gender        string `json:"gender"`
	ModelImageURL string `json:"modelImageUrl"`
}

func (b lookBody) toInput() service.LookInput {
	return service.LookInput{Title: b.Title, Gender: b.Gender, ModelImageURL: b.ModelImageURL}
}

type lookProductBody struct {
	ProductID string `json:"productId"`
}

type lookNewProductBody struct {
	Product productBody `json:"product"`
}

type lookOrderBody struct {
	ProductIDs []string `json:"productIds"`
}

func (h *LookHandler) seller(w http.ResponseWriter, r *http.Request) (string, bool) {
	c, ok := middleware.ClaimsFrom(r.Context())
	if !ok || c == nil {
		httpx.WriteError(w, r, apperr.Unauthenticated("Authentication required"), h.Log)
		return "", false
	}
	return c.UserID, true
}

func (h *LookHandler) fail(w http.ResponseWriter, r *http.Request, err error) {
	httpx.WriteError(w, r, err, h.Log)
}

func (h *LookHandler) respond(w http.ResponseWriter, r *http.Request, status int, v *service.LookView, err error) {
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, status, map[string]any{"look": v})
}

// List - GET /api/v1/seller/looks
func (h *LookHandler) List(w http.ResponseWriter, r *http.Request) {
	id, ok := h.seller(w, r)
	if !ok {
		return
	}
	rows, err := h.Svc.List(r.Context(), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"looks": rows})
}

// Get - GET /api/v1/seller/looks/{id}
func (h *LookHandler) Get(w http.ResponseWriter, r *http.Request) {
	id, ok := h.seller(w, r)
	if !ok {
		return
	}
	v, err := h.Svc.Get(r.Context(), id, chi.URLParam(r, "id"))
	h.respond(w, r, http.StatusOK, v, err)
}

// Create - POST /api/v1/seller/looks
func (h *LookHandler) Create(w http.ResponseWriter, r *http.Request) {
	id, ok := h.seller(w, r)
	if !ok {
		return
	}
	var body lookBody
	if err := httpx.DecodeJSON(w, r, &body, httpx.MaxBodyBytes); err != nil {
		h.fail(w, r, err)
		return
	}
	v, err := h.Svc.Create(r.Context(), id, body.toInput())
	h.respond(w, r, http.StatusCreated, v, err)
}

// Update - PUT /api/v1/seller/looks/{id}
func (h *LookHandler) Update(w http.ResponseWriter, r *http.Request) {
	id, ok := h.seller(w, r)
	if !ok {
		return
	}
	var body lookBody
	if err := httpx.DecodeJSON(w, r, &body, httpx.MaxBodyBytes); err != nil {
		h.fail(w, r, err)
		return
	}
	v, err := h.Svc.Update(r.Context(), id, chi.URLParam(r, "id"), body.toInput())
	h.respond(w, r, http.StatusOK, v, err)
}

// Delete - DELETE /api/v1/seller/looks/{id}
func (h *LookHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id, ok := h.seller(w, r)
	if !ok {
		return
	}
	if err := h.Svc.Delete(r.Context(), id, chi.URLParam(r, "id")); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Publish - POST /api/v1/seller/looks/{id}/publish
func (h *LookHandler) Publish(w http.ResponseWriter, r *http.Request) {
	id, ok := h.seller(w, r)
	if !ok {
		return
	}
	v, err := h.Svc.Publish(r.Context(), id, chi.URLParam(r, "id"))
	h.respond(w, r, http.StatusOK, v, err)
}

// Unpublish - POST /api/v1/seller/looks/{id}/unpublish
func (h *LookHandler) Unpublish(w http.ResponseWriter, r *http.Request) {
	id, ok := h.seller(w, r)
	if !ok {
		return
	}
	v, err := h.Svc.Unpublish(r.Context(), id, chi.URLParam(r, "id"))
	h.respond(w, r, http.StatusOK, v, err)
}

// AddExisting - POST /api/v1/seller/looks/{id}/items   {"productId":"..."}
// Connects a product that is already published in the seller's store.
func (h *LookHandler) AddExisting(w http.ResponseWriter, r *http.Request) {
	id, ok := h.seller(w, r)
	if !ok {
		return
	}
	var body lookProductBody
	if err := httpx.DecodeJSON(w, r, &body, 1<<12); err != nil {
		h.fail(w, r, err)
		return
	}
	v, err := h.Svc.AddExisting(r.Context(), id, chi.URLParam(r, "id"), body.ProductID)
	h.respond(w, r, http.StatusOK, v, err)
}

// AddNew - POST /api/v1/seller/looks/{id}/items/new   {"product":{...}}
// The body is the same product form the Products page sends. The product is
// created as a normal published product and connected to the look.
func (h *LookHandler) AddNew(w http.ResponseWriter, r *http.Request) {
	id, ok := h.seller(w, r)
	if !ok {
		return
	}
	var body lookNewProductBody
	if err := httpx.DecodeJSON(w, r, &body, httpx.MaxBodyBytes); err != nil {
		h.fail(w, r, err)
		return
	}
	v, err := h.Svc.CreateAndAttach(r.Context(), id, chi.URLParam(r, "id"), body.Product.toInput())
	h.respond(w, r, http.StatusCreated, v, err)
}

// Remove - DELETE /api/v1/seller/looks/{id}/items/{productId}
// Takes the product out of the look only; the product itself is untouched.
func (h *LookHandler) Remove(w http.ResponseWriter, r *http.Request) {
	id, ok := h.seller(w, r)
	if !ok {
		return
	}
	v, err := h.Svc.RemoveItem(r.Context(), id, chi.URLParam(r, "id"), chi.URLParam(r, "productId"))
	h.respond(w, r, http.StatusOK, v, err)
}

// Replace - PUT /api/v1/seller/looks/{id}/items/{productId}   {"productId":"<new>"}
func (h *LookHandler) Replace(w http.ResponseWriter, r *http.Request) {
	id, ok := h.seller(w, r)
	if !ok {
		return
	}
	var body lookProductBody
	if err := httpx.DecodeJSON(w, r, &body, 1<<12); err != nil {
		h.fail(w, r, err)
		return
	}
	v, err := h.Svc.ReplaceItem(r.Context(), id, chi.URLParam(r, "id"), chi.URLParam(r, "productId"), body.ProductID)
	h.respond(w, r, http.StatusOK, v, err)
}

// Reorder - PUT /api/v1/seller/looks/{id}/items/order   {"productIds":["...","..."]}
func (h *LookHandler) Reorder(w http.ResponseWriter, r *http.Request) {
	id, ok := h.seller(w, r)
	if !ok {
		return
	}
	var body lookOrderBody
	if err := httpx.DecodeJSON(w, r, &body, httpx.MaxBodyBytes); err != nil {
		h.fail(w, r, err)
		return
	}
	v, err := h.Svc.Reorder(r.Context(), id, chi.URLParam(r, "id"), body.ProductIDs)
	h.respond(w, r, http.StatusOK, v, err)
}

// Public - GET /api/v1/looks/{id}  (no login)
func (h *LookHandler) Public(w http.ResponseWriter, r *http.Request) {
	v, err := h.Svc.Public(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"look": v})
}
