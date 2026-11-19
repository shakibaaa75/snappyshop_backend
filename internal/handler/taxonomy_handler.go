package handler

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/httpx"
	"shopbd/backend/internal/taxonomy"
)

// TaxonomyHandler serves the platform-wide category tree. All routes are public.
type TaxonomyHandler struct {
	Index *taxonomy.Index
	Log   *slog.Logger
}

// Home — GET /api/v1/categories  (main categories only, for the homepage)
func (h *TaxonomyHandler) Home(w http.ResponseWriter, r *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"categories": h.Index.Home()})
}

// Tree — GET /api/v1/categories/tree  (mains + children, for the seller form)
func (h *TaxonomyHandler) Tree(w http.ResponseWriter, r *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"categories": h.Index.Tree()})
}

// Children — GET /api/v1/categories/{id}/children  (shown when a buyer clicks a main category)
func (h *TaxonomyHandler) Children(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	n, ok := h.Index.Node(id)
	if !ok || !taxonomy.IsMain(n) {
		httpx.WriteError(w, r, apperr.NotFound(), h.Log)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"category":      taxonomy.Brief{ID: n.ID, Name: n.Name},
		"subcategories": h.Index.Subcategories(n.ID),
	})
}

// Suggest — GET /api/v1/categories/suggest?q=hod
//
//	(no extra params)  searches main categories
//	&parent=fashion    searches the subcategories of one main category
//	&level=sub         searches subcategories of every main category
//
// Matches names AND aliases, and tolerates small typos ("hodie" -> Hoodie).
func (h *TaxonomyHandler) Suggest(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	parent := r.URL.Query().Get("parent")
	level := r.URL.Query().Get("level")

	var filter taxonomy.NodeFilter = taxonomy.IsMain
	switch {
	case parent != "":
		filter = func(n taxonomy.Node) bool { return n.ParentID == parent }
	case level == "sub":
		filter = taxonomy.IsSub
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"suggestions": h.Index.Suggest(q, filter, 8)})
}
