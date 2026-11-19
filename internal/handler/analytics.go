package handler

import (
	"log/slog"
	"net"
	"net/http"
	"strings"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/httpx"
	"shopbd/backend/internal/middleware"
	"shopbd/backend/internal/service"
)

type AnalyticsHandler struct {
	Svc *service.AnalyticsService
	Log *slog.Logger
	// TrustProxy must match cfg.TrustProxy. When true (behind Nginx), the
	// client IP is read from X-Real-IP / X-Forwarded-For instead of RemoteAddr.
	TrustProxy bool
}

type trackBody struct {
	StoreSlug string `json:"storeSlug"`
	Kind      string `json:"kind"`
	ProductID string `json:"productId"`
	Term      string `json:"term"`
	Source    string `json:"source"`
	VisitorID string `json:"visitorId"`
}

// clientIP returns the caller's IP. It only trusts proxy headers when
// TrustProxy is on, otherwise anyone could spoof them.
func (h *AnalyticsHandler) clientIP(r *http.Request) string {
	if h.TrustProxy {
		if ip := strings.TrimSpace(r.Header.Get("X-Real-IP")); ip != "" {
			return ip
		}
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			// left-most entry is the original client
			if first := strings.TrimSpace(strings.Split(xff, ",")[0]); first != "" {
				return first
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// Track — POST /api/v1/analytics/events (public)
func (h *AnalyticsHandler) Track(w http.ResponseWriter, r *http.Request) {
	var body trackBody
	// Lenient decode: the analytics payload is owned by the frontend, and an
	// unknown field must not silently drop the event. The service layer
	// validates every field it actually consumes.
	if err := httpx.DecodeJSONLenient(w, r, &body, 1<<12); err != nil {
		if h.Log != nil {
			h.Log.Warn("analytics track: decode failed", "error", err.Error(), "path", r.URL.Path)
		}
		httpx.WriteError(w, r, err, h.Log)
		return
	}
	err := h.Svc.Track(r.Context(), service.TrackEventInput{
		StoreSlug: strings.TrimSpace(body.StoreSlug),
		Kind:      strings.TrimSpace(body.Kind),
		ProductID: strings.TrimSpace(body.ProductID),
		Term:      strings.TrimSpace(body.Term),
		Source:    strings.TrimSpace(body.Source),
		VisitorID: strings.TrimSpace(body.VisitorID),
		ClientIP:  h.clientIP(r),
		UserAgent: r.UserAgent(),
	})
	if err != nil {
		if h.Log != nil {
			h.Log.Warn("analytics track: service rejected event",
				"error", err.Error(),
				"slug", body.StoreSlug,
				"kind", body.Kind,
				"productId", body.ProductID,
			)
		}
		httpx.WriteError(w, r, err, h.Log)
		return
	}
	httpx.WriteJSON(w, http.StatusNoContent, nil)
}

// Overview — GET /api/v1/seller/analytics/overview
func (h *AnalyticsHandler) Overview(w http.ResponseWriter, r *http.Request) {
	claims, ok := middleware.ClaimsFrom(r.Context())
	if !ok {
		httpx.WriteError(w, r, apperr.Unauthenticated("Authentication required"), h.Log)
		return
	}

	overview, err := h.Svc.Overview(r.Context(), claims.UserID)
	if err != nil {
		httpx.WriteError(w, r, err, h.Log)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, overview)
}
