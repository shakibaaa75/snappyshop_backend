package service

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/fraud"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/repository"
	"shopbd/backend/internal/secretbox"
)

var (
	// ErrFraudDisabled: the admin has the feature switched off.
	ErrFraudDisabled = errors.New("fraud check is switched off")
	// ErrFraudProviderOff: no usable third-party provider is configured.
	ErrFraudProviderOff = errors.New("fraud provider is not configured")
)

var fraudKeyAAD = []byte("fraud_settings:provider_api_key")

type adminLogWriter interface {
	Create(ctx context.Context, l *models.AdminActivityLog) error
}

// FraudOrderSource streams finished orders for the backfill.
type FraudOrderSource interface {
	ForEachTracked(ctx context.Context, fn func(models.Order) error) error
}

// BackfillStatus describes the one-off history import.
type BackfillStatus struct {
	Running    bool
	Processed  int
	StartedAt  *time.Time
	FinishedAt *time.Time
	Error      string
}

// FraudSettingsService is the admin side: switches, provider key, retention.
type FraudSettingsService struct {
	repo       repository.FraudSettingsRepository
	logs       adminLogWriter
	box        *secretbox.Box // nil = the provider key cannot be stored
	history    *FraudHistory
	orders     FraudOrderSource
	allowLocal bool            // accept http://localhost provider URLs (dev/test only)
	bg         context.Context // app lifetime, for the backfill goroutine
	log        *slog.Logger
	now        func() time.Time

	mu sync.Mutex
	bf BackfillStatus
}

func NewFraudSettingsService(
	repo repository.FraudSettingsRepository, logs adminLogWriter, box *secretbox.Box,
	history *FraudHistory, orders FraudOrderSource, allowLocal bool,
	bg context.Context, log *slog.Logger,
) *FraudSettingsService {
	if log == nil {
		log = slog.Default()
	}
	return &FraudSettingsService{
		repo: repo, logs: logs, box: box, history: history, orders: orders,
		allowLocal: allowLocal, bg: bg, log: log,
		now: func() time.Time { return time.Now().UTC() },
	}
}

// Load returns the saved settings, or the all-off defaults.
func (s *FraudSettingsService) Load(ctx context.Context) (models.FraudSettings, error) {
	cur, err := s.repo.Get(ctx)
	if errors.Is(err, repository.ErrNotFound) {
		return models.DefaultFraudSettings(), nil
	}
	if err != nil {
		return models.FraudSettings{}, err
	}
	return *cur, nil
}

// KeyStorageAvailable reports whether a provider key can be saved.
func (s *FraudSettingsService) KeyStorageAvailable() bool { return s.box != nil }

// FraudSettingsInput is a partial update: nil fields stay as they are.
type FraudSettingsInput struct {
	Enabled           *bool
	ProviderEnabled   *bool
	BaseURL           *string
	APIKey            *string // empty/blank = keep the saved key
	ClearAPIKey       bool
	MinOrders         *int
	CacheDays         *int
	RetentionDays     *int
	StoreProviderData *bool
}

func (s *FraudSettingsService) Update(ctx context.Context, adminID, ip string, in FraudSettingsInput) (*models.FraudSettings, error) {
	cur, err := s.Load(ctx)
	if err != nil {
		return nil, err
	}
	next := cur
	bad := map[string]string{}

	if in.Enabled != nil {
		next.Enabled = *in.Enabled
	}
	if in.ProviderEnabled != nil {
		next.ProviderEnabled = *in.ProviderEnabled
	}
	if in.StoreProviderData != nil {
		next.StoreProviderData = *in.StoreProviderData
	}
	if in.MinOrders != nil {
		if *in.MinOrders < 1 || *in.MinOrders > 50 {
			bad["minOrders"] = "Must be between 1 and 50"
		}
		next.MinOrders = *in.MinOrders
	}
	if in.CacheDays != nil {
		if *in.CacheDays < 0 || *in.CacheDays > 90 {
			bad["cacheDays"] = "Must be between 0 and 90"
		}
		next.CacheDays = *in.CacheDays
	}
	if in.RetentionDays != nil {
		if d := *in.RetentionDays; d != 0 && (d < 30 || d > 1825) {
			bad["retentionDays"] = "Use 0 (keep forever) or 30 to 1825 days"
		}
		next.RetentionDays = *in.RetentionDays
	}
	if in.BaseURL != nil {
		u := strings.TrimSpace(*in.BaseURL)
		if msg := s.checkBaseURL(u); msg != "" {
			bad["baseUrl"] = msg
		}
		next.BaseURL = strings.TrimRight(u, "/")
	}

	switch {
	case in.ClearAPIKey:
		next.APIKeySealed, next.APIKeyHint = "", ""
	case in.APIKey != nil && strings.TrimSpace(*in.APIKey) != "":
		key := strings.TrimSpace(*in.APIKey)
		switch {
		case len(key) < 16 || len(key) > 256 || strings.IndexFunc(key, unicode.IsSpace) >= 0 || strings.IndexFunc(key, unicode.IsControl) >= 0:
			bad["apiKey"] = "That does not look like a valid API key"
		case s.box == nil:
			bad["apiKey"] = "Secure key storage is not configured on the server (COURIER_ENCRYPTION_KEY)"
		default:
			sealed, err := s.box.Seal([]byte(key), fraudKeyAAD)
			if err != nil {
				return nil, err
			}
			next.APIKeySealed, next.APIKeyHint = sealed, key[len(key)-4:]
		}
	}
	if next.APIKeySealed == "" {
		// No key, nothing to call the provider with.
		if in.ProviderEnabled != nil && *in.ProviderEnabled {
			bad["providerEnabled"] = "Add an API key first"
		}
		next.ProviderEnabled = false
	}
	if len(bad) > 0 {
		return nil, apperr.Validation(bad)
	}

	next.ID = models.FraudSettingsID
	next.ProviderID = fraud.FraudBDID
	next.UpdatedAt = s.now()
	next.UpdatedBy = adminID
	if err := s.repo.Upsert(ctx, &next); err != nil {
		return nil, err
	}
	_ = s.logs.Create(ctx, &models.AdminActivityLog{
		ID: models.NewID(), AdminID: adminID, Action: "fraud.settings.update",
		TargetType: "fraud_settings", TargetID: models.FraudSettingsID,
		PreviousValue: fraudAuditView(cur), NewValue: fraudAuditView(next),
		IP: ip, CreatedAt: next.UpdatedAt,
	})
	return &next, nil
}

// fraudAuditView is what the activity log keeps: never the key itself.
func fraudAuditView(s models.FraudSettings) map[string]any {
	return map[string]any{
		"enabled": s.Enabled, "providerEnabled": s.ProviderEnabled, "baseUrl": s.BaseURL,
		"hasApiKey": s.APIKeySealed != "", "apiKeyHint": s.APIKeyHint,
		"minOrders": s.MinOrders, "cacheDays": s.CacheDays,
		"retentionDays": s.RetentionDays, "storeProviderData": s.StoreProviderData,
	}
}

// checkBaseURL: empty = the provider's real address. Otherwise only the
// provider's own domain (or localhost in dev), so the server can never be
// pointed at an arbitrary or internal address.
func (s *FraudSettingsService) checkBaseURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "Enter a plain address such as https://fraudbd.com"
	}
	host := strings.ToLower(u.Hostname())
	switch {
	case u.Scheme == "https" && (host == "fraudbd.com" || strings.HasSuffix(host, ".fraudbd.com")):
		return ""
	case s.allowLocal && u.Scheme == "http" && (host == "localhost" || host == "127.0.0.1"):
		return ""
	}
	return "Only https://fraudbd.com addresses are allowed"
}

// Provider builds the live client for the fraud check. It returns
// ErrFraudDisabled or ErrFraudProviderOff when there is nothing to call.
func (s *FraudSettingsService) Provider(ctx context.Context) (fraud.Provider, models.FraudSettings, error) {
	cfg, err := s.Load(ctx)
	if err != nil {
		return nil, cfg, err
	}
	if !cfg.Enabled {
		return nil, cfg, ErrFraudDisabled
	}
	if !cfg.ProviderEnabled || cfg.APIKeySealed == "" || s.box == nil {
		return nil, cfg, ErrFraudProviderOff
	}
	key, err := s.box.Open(cfg.APIKeySealed, fraudKeyAAD)
	if err != nil {
		s.log.Error("fraud provider key cannot be decrypted", "err", err)
		return nil, cfg, ErrFraudProviderOff
	}
	return fraud.NewFraudBD(cfg.BaseURL, string(key), nil), cfg, nil
}

// TestResult is the outcome of "Test connection".
type TestResult struct {
	OK      bool
	Message string
}

// Test makes ONE real lookup (it uses one request of the provider's quota).
func (s *FraudSettingsService) Test(ctx context.Context) (TestResult, error) {
	cfg, err := s.Load(ctx)
	if err != nil {
		return TestResult{}, err
	}
	if cfg.APIKeySealed == "" {
		return TestResult{Message: "Save an API key first."}, nil
	}
	if s.box == nil {
		return TestResult{Message: "Secure key storage is not configured on the server."}, nil
	}
	key, err := s.box.Open(cfg.APIKeySealed, fraudKeyAAD)
	if err != nil {
		return TestResult{Message: "The saved key cannot be read. Save it again."}, nil
	}
	_, err = fraud.NewFraudBD(cfg.BaseURL, string(key), nil).Check(ctx, "01700000000")
	switch {
	case err == nil:
		return TestResult{OK: true, Message: "Connected. The provider accepted the key."}, nil
	case errors.Is(err, fraud.ErrRejected):
		return TestResult{Message: "The provider rejected this key, or the account has no active plan."}, nil
	case errors.Is(err, fraud.ErrQuota):
		return TestResult{OK: true, Message: "The key works, but the provider's usage limit is reached."}, nil
	default:
		return TestResult{Message: "Could not reach the provider. Try again in a moment."}, nil
	}
}

/* ---------------------------- history backfill ---------------------------- */

// autoCancelNote starts the note the payment-window sweeper writes. Those
// cancellations were not the customer refusing a parcel, so they are skipped.
const autoCancelNote = "Payment was not received in time"

// StartBackfill imports existing orders into the fraud history, in the
// background. It is safe to run again: orders are counted once.
func (s *FraudSettingsService) StartBackfill(ctx context.Context, adminID, ip string) error {
	cfg, err := s.Load(ctx)
	if err != nil {
		return err
	}
	if !cfg.Enabled {
		return apperr.Validation(map[string]string{"enabled": "Switch the fraud check on before importing history"})
	}
	s.mu.Lock()
	if s.bf.Running {
		s.mu.Unlock()
		return apperr.Conflict("BACKFILL_RUNNING", "An import is already running", "")
	}
	started := s.now()
	s.bf = BackfillStatus{Running: true, StartedAt: &started}
	s.mu.Unlock()

	_ = s.logs.Create(ctx, &models.AdminActivityLog{
		ID: models.NewID(), AdminID: adminID, Action: "fraud.backfill.start",
		TargetType: "fraud_settings", TargetID: models.FraudSettingsID, IP: ip, CreatedAt: started,
	})

	go func() {
		err := s.orders.ForEachTracked(s.bg, func(o models.Order) error {
			if o.Status == models.OrderCancelled && len(o.Events) > 0 &&
				strings.HasPrefix(o.Events[len(o.Events)-1].Note, autoCancelNote) {
				return nil
			}
			s.history.Record(s.bg, o)
			s.mu.Lock()
			s.bf.Processed++
			s.mu.Unlock()
			return s.bg.Err()
		})
		done := s.now()
		s.mu.Lock()
		s.bf.Running, s.bf.FinishedAt = false, &done
		if err != nil {
			s.bf.Error = "The import stopped early. You can run it again."
			s.log.Error("fraud backfill failed", "err", err)
		}
		s.mu.Unlock()
	}()
	return nil
}

func (s *FraudSettingsService) Backfill() BackfillStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bf
}
