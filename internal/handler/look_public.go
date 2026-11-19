package handler

import (
	"net/http"
	"strconv"

	"shopbd/backend/internal/httpx"
)

// PublicList - GET /api/v1/looks?store=<slug>&gender=male|female&limit=12  (no login)
// Published looks that buyers can see. Used by the marketplace home, a store's
// home and the all-looks page.
func (h *LookHandler) PublicList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	rows, err := h.Svc.PublicList(r.Context(), q.Get("store"), q.Get("gender"), limit)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=30")
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"looks": rows})
}
