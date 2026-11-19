package service

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/plans"
	"shopbd/backend/internal/repository"
)

// PlanCatalogService lets an admin change plan prices, limits, feature
// switches, discounts and extra feature lines. The edited catalog is stored in
// MongoDB and loaded into the plans package, which every plan check and every
// price quote already reads, so a saved change applies on the next request.
//
// Existing subscribers keep the period they paid for. They get the new
// features and limits of their plan straight away (entitlements are read live);
// a new price only applies to purchases made after the change.
type PlanCatalogService struct {
	repo repository.PlanCatalogRepository
	logs repository.AdminActivityLogRepository // optional
	log  *slog.Logger
	now  func() time.Time

	mu      sync.Mutex // serialises Update / Reset / reload
	updated time.Time  // UpdatedAt of the document currently applied
	by      string
}

func NewPlanCatalogService(
	repo repository.PlanCatalogRepository,
	logs repository.AdminActivityLogRepository,
	log *slog.Logger,
) *PlanCatalogService {
	if log == nil {
		log = slog.Default()
	}
	return &PlanCatalogService{repo: repo, logs: logs, log: log, now: time.Now}
}

// Load applies the stored catalog, if there is one. A stored catalog that no
// longer validates is ignored (the built-in plans stay) and reported.
func (s *PlanCatalogService) Load(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reloadLocked(ctx)
}

func (s *PlanCatalogService) reloadLocked(ctx context.Context) error {
	doc, err := s.repo.Get(ctx)
	if errors.Is(err, repository.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if doc.UpdatedAt.Equal(s.updated) && !s.updated.IsZero() {
		return nil // already applied
	}
	if err := plans.Apply(doc.Config); err != nil {
		s.log.Error("stored plan catalog is invalid; keeping the current plans", "error", err.Error())
		return err
	}
	s.updated, s.by = doc.UpdatedAt, doc.UpdatedBy
	s.log.Info("plan catalog loaded", "updated_at", doc.UpdatedAt)
	return nil
}

// Run re-reads the stored catalog every interval until ctx ends, so a change
// saved through one server instance reaches the others without a restart.
func (s *PlanCatalogService) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			s.mu.Lock()
			err := s.reloadLocked(cctx)
			s.mu.Unlock()
			cancel()
			if err != nil {
				s.log.Warn("plan catalog refresh failed", "error", err.Error())
			}
		}
	}
}

/* ------------------------------------------------------------------ */
/* Views                                                               */
/* ------------------------------------------------------------------ */

// DurationView is one billing period as the pricing page shows it.
type DurationView struct {
	ID          string `json:"id"` // monthly | half_yearly | yearly
	Months      int    `json:"months"`
	DiscountPct int    `json:"discountPct"`
	Label       string `json:"label"`
}

// PlanCatalogView is the catalog sent to the app. The pricing page and the
// admin editor both use it.
type PlanCatalogView struct {
	plans.Config
	Durations []DurationView `json:"durations"`
	UpdatedAt *time.Time     `json:"updatedAt,omitempty"`
	UpdatedBy string         `json:"updatedBy,omitempty"` // admin id; only in the admin view
}

func periodLabel(months int) string {
	if months == 1 {
		return "1 month"
	}
	return strconv.Itoa(months) + " months"
}

func (s *PlanCatalogService) view() PlanCatalogView {
	cfg := plans.Snapshot()
	v := PlanCatalogView{Config: cfg}
	for _, id := range []string{plans.Monthly, plans.HalfYearly, plans.Yearly} {
		m := plans.Months(id)
		v.Durations = append(v.Durations, DurationView{
			ID: id, Months: m, DiscountPct: plans.PeriodDiscountPct(id), Label: periodLabel(m),
		})
	}
	s.mu.Lock()
	if !s.updated.IsZero() {
		t := s.updated
		v.UpdatedAt = &t
		v.UpdatedBy = s.by
	}
	s.mu.Unlock()
	return v
}

// PublicView is what any signed-in seller (or visitor) may see.
func (s *PlanCatalogService) PublicView() PlanCatalogView {
	v := s.view()
	v.UpdatedBy = ""
	return v
}

// AdminView is the catalog for the admin editor.
func (s *PlanCatalogService) AdminView() PlanCatalogView { return s.view() }

/* ------------------------------------------------------------------ */
/* Changes                                                             */
/* ------------------------------------------------------------------ */

// Update validates, stores and applies a new catalog. Nothing changes if the
// input is invalid or the save fails.
func (s *PlanCatalogService) Update(ctx context.Context, adminID string, cfg plans.Config, ip string) (PlanCatalogView, error) {
	return s.save(ctx, adminID, cfg, "plans.update", ip)
}

// Reset stores and applies the built-in plans and discounts again.
func (s *PlanCatalogService) Reset(ctx context.Context, adminID, ip string) (PlanCatalogView, error) {
	return s.save(ctx, adminID, plans.DefaultConfig(), "plans.reset", ip)
}

func (s *PlanCatalogService) save(ctx context.Context, adminID string, cfg plans.Config, action, ip string) (PlanCatalogView, error) {
	clean, err := plans.Normalize(cfg)
	if err != nil {
		var ve *plans.ValidationError
		if errors.As(err, &ve) {
			return PlanCatalogView{}, apperr.BadRequest(ve.Msg)
		}
		return PlanCatalogView{}, err
	}

	s.mu.Lock()
	prev := plans.Snapshot()
	now := s.now().UTC()
	doc := &models.PlanCatalogDoc{Config: clean, UpdatedAt: now, UpdatedBy: adminID}
	if err := s.repo.Save(ctx, doc); err != nil {
		s.mu.Unlock()
		return PlanCatalogView{}, err
	}
	if err := plans.Apply(clean); err != nil { // cannot fail after Normalize; kept as a guard
		s.mu.Unlock()
		return PlanCatalogView{}, err
	}
	s.updated, s.by = now, adminID
	s.mu.Unlock()

	if s.logs != nil {
		_ = s.logs.Create(ctx, &models.AdminActivityLog{
			ID: models.NewID(), AdminID: adminID, Action: action,
			TargetType: "plans", TargetID: models.PlanCatalogID,
			PreviousValue: prev, NewValue: clean, IP: ip, CreatedAt: now,
		})
	}
	return s.view(), nil
}
