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

// FraudSellerHandler serves the seller-side fraud check.
type FraudSellerHandler struct {
	Svc *service.FraudCheckService
	Log *slog.Logger
}

func (h *FraudSellerHandler) fail(w http.ResponseWriter, r *http.Request, err error) {
	httpx.WriteError(w, r, err, h.Log)
}

// Status — GET /api/v1/seller/fraud/status  ({"enabled": bool})
func (h *FraudSellerHandler) Status(w http.ResponseWriter, r *http.Request) {
	on, err := h.Svc.Enabled(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"enabled": on})
}

type fraudCheckDTO struct {
	Phone    string `json:"phone"`
	Internal *struct {
		Delivered int `json:"delivered"`
		Cancelled int `json:"cancelled"`
		Failed    int `json:"failed"`
		Open      int `json:"open"`
		Finished  int `json:"finished"`
	} `json:"internal"`
	External *struct {
		Provider  string `json:"provider"`
		Total     int    `json:"total"`
		Success   int    `json:"success"`
		Cancel    int    `json:"cancel"`
		Rating    string `json:"rating,omitempty"`
		RiskLevel string `json:"riskLevel,omitempty"`
		CheckedAt string `json:"checkedAt"`
	} `json:"external"`
	ExternalStatus string `json:"externalStatus"`
	CheckedAt      string `json:"checkedAt,omitempty"`
	Risk           struct {
		Level       string   `json:"level"`
		Sample      int      `json:"sample"`
		SuccessRate *float64 `json:"successRate"`
		Reasons     []string `json:"reasons"`
	} `json:"risk"`
}

// Check — POST /api/v1/seller/orders/{id}/fraud-check
func (h *FraudSellerHandler) Check(w http.ResponseWriter, r *http.Request) {
	claims, ok := middleware.ClaimsFrom(r.Context())
	if !ok {
		h.fail(w, r, apperr.Unauthenticated("Authentication required"))
		return
	}
	res, err := h.Svc.CheckOrder(r.Context(), claims.UserID, chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"result": toFraudDTO(res)})
}

// Saved — GET /api/v1/seller/orders/{id}/fraud-check
// The last result for this order, or {"result": null} if it was never checked.
func (h *FraudSellerHandler) Saved(w http.ResponseWriter, r *http.Request) {
	claims, ok := middleware.ClaimsFrom(r.Context())
	if !ok {
		h.fail(w, r, apperr.Unauthenticated("Authentication required"))
		return
	}
	res, err := h.Svc.Saved(r.Context(), claims.UserID, chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if res == nil {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"result": nil})
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"result": toFraudDTO(res)})
}

func toFraudDTO(res *service.FraudCheckResult) fraudCheckDTO {
	var out fraudCheckDTO
	out.Phone = res.Phone
	if in := res.Internal; in != nil {
		out.Internal = &struct {
			Delivered int `json:"delivered"`
			Cancelled int `json:"cancelled"`
			Failed    int `json:"failed"`
			Open      int `json:"open"`
			Finished  int `json:"finished"`
		}{in.Delivered, in.Cancelled, in.Failed, in.Open, in.Delivered + in.Cancelled + in.Failed}
	}
	if e := res.External; e != nil {
		out.External = &struct {
			Provider  string `json:"provider"`
			Total     int    `json:"total"`
			Success   int    `json:"success"`
			Cancel    int    `json:"cancel"`
			Rating    string `json:"rating,omitempty"`
			RiskLevel string `json:"riskLevel,omitempty"`
			CheckedAt string `json:"checkedAt"`
		}{e.Provider, e.Total, e.Success, e.Cancel, e.Rating, e.RiskLevel, e.CheckedAt.UTC().Format(time.RFC3339)}
	}
	out.ExternalStatus = res.ExternalStatus
	if !res.CheckedAt.IsZero() {
		out.CheckedAt = res.CheckedAt.UTC().Format(time.RFC3339)
	}
	out.Risk.Level, out.Risk.Sample = res.Assessment.Level, res.Assessment.Sample
	out.Risk.SuccessRate, out.Risk.Reasons = res.Assessment.SuccessRate, res.Assessment.Reasons
	if out.Risk.Reasons == nil {
		out.Risk.Reasons = []string{} // JSON [] not null: the UI maps over it
	}
	return out
}
