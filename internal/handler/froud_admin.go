package handler

import (
	"log/slog"
	"net/http"
	"time"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/httpx"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/service"
)

// FraudAdminHandler serves the super admin's fraud settings.
type FraudAdminHandler struct {
	Svc *service.FraudSettingsService
	Log *slog.Logger
}

func (h *FraudAdminHandler) fail(w http.ResponseWriter, r *http.Request, err error) {
	httpx.WriteError(w, r, err, h.Log)
}

// FraudSettingsDTO never contains the API key, only whether one is saved.
type FraudSettingsDTO struct {
	Enabled             bool   `json:"enabled"`
	ProviderEnabled     bool   `json:"providerEnabled"`
	ProviderID          string `json:"providerId"`
	BaseURL             string `json:"baseUrl"`
	HasAPIKey           bool   `json:"hasApiKey"`
	APIKeyHint          string `json:"apiKeyHint,omitempty"`
	KeyStorageAvailable bool   `json:"keyStorageAvailable"`
	MinOrders           int    `json:"minOrders"`
	CacheDays           int    `json:"cacheDays"`
	RetentionDays       int    `json:"retentionDays"`
	StoreProviderData   bool   `json:"storeProviderData"`
	UpdatedAt           string `json:"updatedAt,omitempty"`
}

func (h *FraudAdminHandler) dto(s models.FraudSettings) FraudSettingsDTO {
	out := FraudSettingsDTO{
		Enabled: s.Enabled, ProviderEnabled: s.ProviderEnabled, ProviderID: s.ProviderID,
		BaseURL: s.BaseURL, HasAPIKey: s.APIKeySealed != "", APIKeyHint: s.APIKeyHint,
		KeyStorageAvailable: h.Svc.KeyStorageAvailable(),
		MinOrders:           s.MinOrders, CacheDays: s.CacheDays, RetentionDays: s.RetentionDays,
		StoreProviderData: s.StoreProviderData,
	}
	if !s.UpdatedAt.IsZero() {
		out.UpdatedAt = s.UpdatedAt.UTC().Format(time.RFC3339)
	}
	return out
}

// Get — GET /api/v1/admin/fraud/settings
func (h *FraudAdminHandler) Get(w http.ResponseWriter, r *http.Request) {
	s, err := h.Svc.Load(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"settings": h.dto(s)})
}

type fraudSettingsBody struct {
	Enabled           *bool   `json:"enabled"`
	ProviderEnabled   *bool   `json:"providerEnabled"`
	BaseURL           *string `json:"baseUrl"`
	APIKey            *string `json:"apiKey"`
	ClearAPIKey       bool    `json:"clearApiKey"`
	MinOrders         *int    `json:"minOrders"`
	CacheDays         *int    `json:"cacheDays"`
	RetentionDays     *int    `json:"retentionDays"`
	StoreProviderData *bool   `json:"storeProviderData"`
}

// Update — PUT /api/v1/admin/fraud/settings (partial update)
func (h *FraudAdminHandler) Update(w http.ResponseWriter, r *http.Request) {
	adminID, ok := adminClaims(r)
	if !ok {
		h.fail(w, r, apperr.Unauthenticated("Authentication required"))
		return
	}
	var b fraudSettingsBody
	if err := httpx.DecodeJSON(w, r, &b, httpx.MaxBodyBytes); err != nil {
		h.fail(w, r, err)
		return
	}
	s, err := h.Svc.Update(r.Context(), adminID, clientIP(r), service.FraudSettingsInput{
		Enabled: b.Enabled, ProviderEnabled: b.ProviderEnabled, BaseURL: b.BaseURL,
		APIKey: b.APIKey, ClearAPIKey: b.ClearAPIKey, MinOrders: b.MinOrders,
		CacheDays: b.CacheDays, RetentionDays: b.RetentionDays, StoreProviderData: b.StoreProviderData,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"settings": h.dto(*s)})
}

// Test — POST /api/v1/admin/fraud/test
func (h *FraudAdminHandler) Test(w http.ResponseWriter, r *http.Request) {
	res, err := h.Svc.Test(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": res.OK, "message": res.Message})
}

func backfillJSON(b service.BackfillStatus) map[string]any {
	out := map[string]any{"running": b.Running, "processed": b.Processed}
	if b.StartedAt != nil {
		out["startedAt"] = b.StartedAt.UTC().Format(time.RFC3339)
	}
	if b.FinishedAt != nil {
		out["finishedAt"] = b.FinishedAt.UTC().Format(time.RFC3339)
	}
	if b.Error != "" {
		out["error"] = b.Error
	}
	return out
}

// BackfillStart — POST /api/v1/admin/fraud/backfill (202, runs in background)
func (h *FraudAdminHandler) BackfillStart(w http.ResponseWriter, r *http.Request) {
	adminID, ok := adminClaims(r)
	if !ok {
		h.fail(w, r, apperr.Unauthenticated("Authentication required"))
		return
	}
	if err := h.Svc.StartBackfill(r.Context(), adminID, clientIP(r)); err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, map[string]any{"backfill": backfillJSON(h.Svc.Backfill())})
}

// BackfillStatus — GET /api/v1/admin/fraud/backfill
func (h *FraudAdminHandler) BackfillStatus(w http.ResponseWriter, r *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"backfill": backfillJSON(h.Svc.Backfill())})
}
