package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/repository"
	"shopbd/backend/internal/secretbox"
)

type memFraudSettings struct{ s *models.FraudSettings }

func (m *memFraudSettings) Get(context.Context) (*models.FraudSettings, error) {
	if m.s == nil {
		return nil, repository.ErrNotFound
	}
	c := *m.s
	return &c, nil
}
func (m *memFraudSettings) Upsert(_ context.Context, s *models.FraudSettings) error {
	c := *s
	m.s = &c
	return nil
}

type memLogs struct{ items []*models.AdminActivityLog }

func (m *memLogs) Create(_ context.Context, l *models.AdminActivityLog) error {
	m.items = append(m.items, l)
	return nil
}

func testBox(t *testing.T) *secretbox.Box {
	t.Helper()
	key := strings.Repeat("k", 32)
	b, err := secretbox.New("k1", map[string][]byte{"k1": []byte(key)}, []byte(strings.Repeat("f", 32)))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func newSettingsSvc(t *testing.T, box *secretbox.Box) (*FraudSettingsService, *memFraudSettings, *memLogs) {
	repo, logs := &memFraudSettings{}, &memLogs{}
	return NewFraudSettingsService(repo, logs, box, nil, nil, false, context.Background(), nil), repo, logs
}

func ptr[T any](v T) *T { return &v }

func isValidation(err error, field string) bool {
	var ae *apperr.Error
	return errors.As(err, &ae) && ae.Fields[field] != ""
}

func TestFraudSettingsDefaultsAreOff(t *testing.T) {
	svc, _, _ := newSettingsSvc(t, testBox(t))
	s, err := svc.Load(context.Background())
	if err != nil || s.Enabled || s.ProviderEnabled {
		t.Fatalf("defaults %+v %v", s, err)
	}
	if _, _, err := svc.Provider(context.Background()); !errors.Is(err, ErrFraudDisabled) {
		t.Fatalf("want ErrFraudDisabled, got %v", err)
	}
}

func TestFraudSettingsKeyIsSealedAndNeverLogged(t *testing.T) {
	svc, repo, logs := newSettingsSvc(t, testBox(t))
	ctx := context.Background()
	const key = "abcd1234efgh5678"
	if _, err := svc.Update(ctx, "adm", "1.2.3.4", FraudSettingsInput{Enabled: ptr(true), APIKey: ptr(key), ProviderEnabled: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	if repo.s.APIKeySealed == "" || strings.Contains(repo.s.APIKeySealed, key) || repo.s.APIKeyHint != "5678" {
		t.Fatalf("bad stored key %+v", repo.s)
	}
	for _, l := range logs.items {
		if strings.Contains(strings.ToLower(l.Action+toString(l.NewValue)+toString(l.PreviousValue)), key) {
			t.Fatal("API key leaked into the activity log")
		}
	}
	p, _, err := svc.Provider(ctx)
	if err != nil || p == nil {
		t.Fatalf("provider: %v", err)
	}
	// Blank key on a later update keeps the saved one.
	if _, err := svc.Update(ctx, "adm", "", FraudSettingsInput{APIKey: ptr("  "), CacheDays: ptr(3)}); err != nil || repo.s.APIKeyHint != "5678" {
		t.Fatalf("key lost: %v %+v", err, repo.s)
	}
	// Clearing the key also turns the provider off.
	if _, err := svc.Update(ctx, "adm", "", FraudSettingsInput{ClearAPIKey: true}); err != nil || repo.s.ProviderEnabled || repo.s.APIKeySealed != "" {
		t.Fatalf("clear failed: %v %+v", err, repo.s)
	}
}

func toString(v any) string {
	m, _ := v.(map[string]any)
	var b strings.Builder
	for k, x := range m {
		b.WriteString(k)
		if s, ok := x.(string); ok {
			b.WriteString(s)
		}
	}
	return b.String()
}

func TestFraudSettingsValidation(t *testing.T) {
	svc, _, _ := newSettingsSvc(t, testBox(t))
	ctx := context.Background()
	cases := map[string]FraudSettingsInput{
		"minOrders":       {MinOrders: ptr(0)},
		"cacheDays":       {CacheDays: ptr(91)},
		"retentionDays":   {RetentionDays: ptr(10)},
		"baseUrl":         {BaseURL: ptr("https://evil.example.com")},
		"apiKey":          {APIKey: ptr("short")},
		"providerEnabled": {ProviderEnabled: ptr(true)}, // no key saved
	}
	for field, in := range cases {
		if _, err := svc.Update(ctx, "adm", "", in); !isValidation(err, field) {
			t.Errorf("%s: want validation error, got %v", field, err)
		}
	}
	if _, err := svc.Update(ctx, "adm", "", FraudSettingsInput{BaseURL: ptr("https://fraudbd.com/"), RetentionDays: ptr(0)}); err != nil {
		t.Errorf("valid input rejected: %v", err)
	}
	if _, err := svc.Update(ctx, "adm", "", FraudSettingsInput{BaseURL: ptr("http://127.0.0.1:9000")}); !isValidation(err, "baseUrl") {
		t.Error("localhost must be rejected when allowLocal is false")
	}
}

func TestFraudSettingsNoBoxCannotStoreKey(t *testing.T) {
	svc, _, _ := newSettingsSvc(t, nil)
	if _, err := svc.Update(context.Background(), "adm", "", FraudSettingsInput{APIKey: ptr("abcd1234efgh5678")}); !isValidation(err, "apiKey") {
		t.Fatalf("want apiKey error, got %v", err)
	}
}

type fakeOrderSource []models.Order

func (f fakeOrderSource) ForEachTracked(_ context.Context, fn func(models.Order) error) error {
	for _, o := range f {
		if err := fn(o); err != nil {
			return err
		}
	}
	return nil
}

func TestFraudBackfillSkipsAutoCancelledAndIsRepeatable(t *testing.T) {
	repo := newFakeFraudRepo()
	enabled := &memFraudSettings{s: &models.FraudSettings{Enabled: true}}
	hist := NewFraudHistory(enabled, repo, nil)
	at := time.Now()
	auto := order("auto", models.OrderCancelled, at)
	auto.Events = []models.OrderStatusEvent{{Status: models.OrderCancelled, Note: autoCancelNote + " - order cancelled automatically"}}
	src := fakeOrderSource{order("a", models.OrderDelivered, at), order("b", models.OrderCancelled, at), auto}
	svc := NewFraudSettingsService(enabled, &memLogs{}, nil, hist, src, false, context.Background(), nil)

	for i := 0; i < 2; i++ { // run twice: counts must not double
		if err := svc.StartBackfill(context.Background(), "adm", ""); err != nil {
			t.Fatal(err)
		}
		for j := 0; j < 200 && svc.Backfill().Running; j++ {
			time.Sleep(10 * time.Millisecond)
		}
	}
	c := repo.cust["01712345678"]
	if c == nil || c.Delivered != 1 || c.Cancelled != 1 {
		t.Fatalf("counts %+v", c)
	}
}

func TestFraudBackfillNeedsFeatureOn(t *testing.T) {
	svc, _, _ := newSettingsSvc(t, nil)
	if err := svc.StartBackfill(context.Background(), "adm", ""); !isValidation(err, "enabled") {
		t.Fatalf("want enabled error, got %v", err)
	}
}
