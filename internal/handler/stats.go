package handler

import (
	"log/slog"
	"net/http"
	"time"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/httpx"
	"shopbd/backend/internal/middleware"
	"shopbd/backend/internal/service"
)

type StatsHandler struct {
	Svc *service.OrderService
	Log *slog.Logger
}

type StatsDTO struct {
	OrderCount     int     `json:"orderCount"`
	PaidOrderCount int     `json:"paidOrderCount"`
	Revenue        float64 `json:"revenue"`
	PendingCount   int     `json:"pendingCount"`
	ConfirmedCount int     `json:"confirmedCount"`
	ShippedCount   int     `json:"shippedCount"`
	DeliveredCount int     `json:"deliveredCount"`
	CancelledCount int     `json:"cancelledCount"`
	AvgOrderValue  float64 `json:"avgOrderValue"`
	UnitsSold      int     `json:"unitsSold"`
}

// Get — GET /api/v1/seller/stats
func (h *StatsHandler) Get(w http.ResponseWriter, r *http.Request) {
	claims, ok := middleware.ClaimsFrom(r.Context())
	if !ok {
		httpx.WriteError(w, r, apperr.Unauthenticated("Authentication required"), h.Log)
		return
	}

	// Default: since Unix epoch (all time). Client can pass ?since=RFC3339.
	since := time.Unix(0, 0).UTC()
	if v := r.URL.Query().Get("since"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			since = t
		}
	}

	stats, err := h.Svc.Stats(r.Context(), claims.UserID, since)
	if err != nil {
		httpx.WriteError(w, r, err, h.Log)
		return
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"stats": StatsDTO{
			OrderCount:     stats.OrderCount,
			PaidOrderCount: stats.PaidOrderCount,
			Revenue:        stats.Revenue,
			PendingCount:   stats.PendingCount,
			ConfirmedCount: stats.ConfirmedCount,
			ShippedCount:   stats.ShippedCount,
			DeliveredCount: stats.DeliveredCount,
			CancelledCount: stats.CancelledCount,
			AvgOrderValue:  stats.AvgOrderValue,
			UnitsSold:      stats.UnitsSold,
		},
	})
}
