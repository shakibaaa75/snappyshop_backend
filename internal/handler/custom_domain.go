package handler

import (
	"log/slog"
	"net/http"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/httpx"
	"shopbd/backend/internal/middleware"
	"shopbd/backend/internal/service"
)

// CustomDomainHandler serves a seller's custom-domain settings (authenticated)
// and the two public lookups the platform's own infrastructure needs: which
// store a domain belongs to, and whether a certificate may be issued for it.
type CustomDomainHandler struct {
	Svc *service.CustomDomainService
	Log *slog.Logger
}

type customDomainBody struct {
	Domain string `json:"domain"`
}

func (h *CustomDomainHandler) sellerID(w http.ResponseWriter, r *http.Request) (string, bool) {
	claims, ok := middleware.ClaimsFrom(r.Context())
	if !ok {
		httpx.WriteError(w, r, apperr.Unauthenticated("Authentication required"), h.Log)
		return "", false
	}
	return claims.UserID, true
}

// Get — GET /api/v1/seller/custom-domain
// The seller's domain, its status and the DNS records still to add.
func (h *CustomDomainHandler) Get(w http.ResponseWriter, r *http.Request) {
	id, ok := h.sellerID(w, r)
	if !ok {
		return
	}
	out, err := h.Svc.Get(r.Context(), id)
	if err != nil {
		httpx.WriteError(w, r, err, h.Log)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// Set — PUT /api/v1/seller/custom-domain   {"domain": "shop.yourbrand.com"}
// The seller ID always comes from the login token, never from the request.
func (h *CustomDomainHandler) Set(w http.ResponseWriter, r *http.Request) {
	id, ok := h.sellerID(w, r)
	if !ok {
		return
	}
	var body customDomainBody
	if err := httpx.DecodeJSON(w, r, &body, 1<<10); err != nil {
		httpx.WriteError(w, r, err, h.Log)
		return
	}
	out, err := h.Svc.Set(r.Context(), id, body.Domain)
	if err != nil {
		httpx.WriteError(w, r, err, h.Log)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// Verify — POST /api/v1/seller/custom-domain/verify
// Checks the DNS records now and returns the updated state.
func (h *CustomDomainHandler) Verify(w http.ResponseWriter, r *http.Request) {
	id, ok := h.sellerID(w, r)
	if !ok {
		return
	}
	out, err := h.Svc.Verify(r.Context(), id)
	if err != nil {
		httpx.WriteError(w, r, err, h.Log)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// Remove — DELETE /api/v1/seller/custom-domain
func (h *CustomDomainHandler) Remove(w http.ResponseWriter, r *http.Request) {
	id, ok := h.sellerID(w, r)
	if !ok {
		return
	}
	if err := h.Svc.Remove(r.Context(), id); err != nil {
		httpx.WriteError(w, r, err, h.Log)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Resolve — GET /api/v1/domain/resolve?host=shop.brand.com (no auth)
// Which store a custom domain serves: {"slug": "...", "domain": "..."}.
// The frontend calls this to turn the Host of a request into a store. A
// domain that is unknown, not verified, or whose plan lapsed is a 404.
func (h *CustomDomainHandler) Resolve(w http.ResponseWriter, r *http.Request) {
	out, err := h.Svc.ResolveHost(r.Context(), r.URL.Query().Get("host"))
	if err != nil {
		httpx.WriteError(w, r, err, h.Log)
		return
	}
	// Short browser/CDN caching is fine: it is public data and the service
	// already caches for the same time.
	w.Header().Set("Cache-Control", "public, max-age=15")
	httpx.WriteJSON(w, http.StatusOK, out)
}

// Allowed — GET /api/v1/domain/allowed?domain=shop.brand.com&token=...
// The "ask" endpoint for on-demand TLS (for example Caddy's on_demand_tls).
// 200 means a certificate may be issued for that domain; anything else means
// it may not. Only connected, verified domains are allowed, so nobody can make
// the server request certificates for names it does not serve.
func (h *CustomDomainHandler) Allowed(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if !h.Svc.TLSAllowed(r.Context(), q.Get("domain"), q.Get("token")) {
		httpx.WriteError(w, r, apperr.NotFound(), h.Log)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
