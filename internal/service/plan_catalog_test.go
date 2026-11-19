package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/plans"
	"shopbd/backend/internal/repository"
)

type memCatalog struct {
	doc  *models.PlanCatalogDoc
	fail bool
}

func (m *memCatalog) Get(context.Context) (*models.PlanCatalogDoc, error) {
	if m.doc == nil {
		return nil, repository.ErrNotFound
	}
	c := *m.doc
	return &c, nil
}
func (m *memCatalog) Save(_ context.Context, d *models.PlanCatalogDoc) error {
	if m.fail {
		return errors.New("db down")
	}
	c := *d
	m.doc = &c
	return nil
}

type memLogs struct{ n int }

func (l *memLogs) Create(context.Context, *models.AdminActivityLog) error { l.n++; return nil }
func (l *memLogs) List(context.Context, repository.AdminLogFilter) ([]*models.AdminActivityLog, int64, error) {
	return nil, 0, nil
}

func TestPlanCatalogService(t *testing.T) {
	t.Cleanup(plans.Reset)
	plans.Reset()
	repo, logs := &memCatalog{}, &memLogs{}
	s := NewPlanCatalogService(repo, logs, nil)
	ctx := context.Background()

	if err := s.Load(ctx); err != nil {
		t.Fatal(err) // no document yet: defaults stay
	}
	v := s.PublicView()
	if len(v.Plans) != 3 || len(v.Durations) != 3 || v.Durations[2].Label != "12 months" || v.UpdatedAt != nil {
		t.Fatalf("default view wrong: %+v", v)
	}

	cfg := plans.DefaultConfig()
	cfg.Plans[1].MonthlyPrice = 349
	cfg.Plans[1].Extras = []plans.Extra{{Label: "Priority support", Included: true}}
	v, err := s.Update(ctx, "admin1", cfg, "1.2.3.4")
	if err != nil {
		t.Fatal(err)
	}
	if v.Plans[1].MonthlyPrice != 349 || v.UpdatedBy != "admin1" || v.UpdatedAt == nil || logs.n != 1 {
		t.Fatalf("update not applied: %+v logs=%d", v, logs.n)
	}
	if pv := s.PublicView(); pv.UpdatedBy != "" {
		t.Error("public view leaks the admin id")
	}
	if q, _ := plans.QuoteFor(plans.Pro, plans.Monthly, false); q.Amount != 349 {
		t.Errorf("quote still old: %+v", q)
	}

	// invalid -> 400 and nothing changes / nothing stored
	bad := plans.DefaultConfig()
	bad.Plans[1].MonthlyPrice = 0
	_, err = s.Update(ctx, "admin1", bad, "")
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Status != 400 {
		t.Fatalf("want 400, got %v", err)
	}
	if p, _ := plans.Get(plans.Pro); p.Price[plans.Monthly] != 349 {
		t.Error("invalid update changed live prices")
	}

	// save failure -> live catalog untouched
	repo.fail = true
	cfg.Plans[1].MonthlyPrice = 999
	if _, err := s.Update(ctx, "admin1", cfg, ""); err == nil {
		t.Fatal("expected save error")
	}
	if p, _ := plans.Get(plans.Pro); p.Price[plans.Monthly] != 349 {
		t.Error("failed save changed live prices")
	}
	repo.fail = false

	// another instance restarts: loads what was stored
	plans.Reset()
	s2 := NewPlanCatalogService(repo, nil, nil)
	if err := s2.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if p, _ := plans.Get(plans.Pro); p.Price[plans.Monthly] != 349 || len(p.Extras) != 1 {
		t.Errorf("stored catalog not loaded: %+v", p)
	}

	// refresh picks up a change made elsewhere
	other := plans.DefaultConfig()
	other.Plans[2].MonthlyPrice = 555
	repo.doc = &models.PlanCatalogDoc{ID: models.PlanCatalogID, Config: other, UpdatedAt: time.Now().Add(time.Minute), UpdatedBy: "x"}
	s2.mu.Lock()
	err = s2.reloadLocked(ctx)
	s2.mu.Unlock()
	if p, _ := plans.Get(plans.Business); err != nil || p.Price[plans.Monthly] != 555 {
		t.Errorf("refresh failed: %v %+v", err, p)
	}

	// reset
	if _, err := s.Reset(ctx, "admin1", ""); err != nil {
		t.Fatal(err)
	}
	if p, _ := plans.Get(plans.Pro); p.Price[plans.Monthly] != 299 {
		t.Error("reset did not restore defaults")
	}
}
