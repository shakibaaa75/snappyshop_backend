package service

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"shopbd/backend/internal/fraud"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/repository"
)

// OrderHistory is told about every order whose status just changed.
type OrderHistory interface {
	Record(ctx context.Context, o models.Order)
}

// SetOrderHistory turns on fraud-history recording for status changes.
func (s *OrderService) SetOrderHistory(h OrderHistory) { s.history = h }

// recordHistory runs in the background on a fresh context so a slow database
// never delays the seller's click and a cancelled request never drops it.
func (s *OrderService) recordHistory(o models.Order) {
	if s.history == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		s.history.Record(ctx, o)
	}()
}

// FraudHistory keeps the platform-wide outcome counters per customer phone.
type FraudHistory struct {
	settings repository.FraudSettingsRepository
	repo     repository.FraudHistoryRepository
	log      *slog.Logger
	now      func() time.Time
}

func NewFraudHistory(settings repository.FraudSettingsRepository, repo repository.FraudHistoryRepository, log *slog.Logger) *FraudHistory {
	if log == nil {
		log = slog.Default()
	}
	return &FraudHistory{settings: settings, repo: repo, log: log, now: func() time.Time { return time.Now().UTC() }}
}

// outcomeFor maps an order status to what it means for the customer's record.
// pending is not tracked: the seller has not accepted the order yet.
func outcomeFor(st models.OrderStatus) (models.FraudOutcome, bool) {
	switch st {
	case models.OrderConfirmed, models.OrderShipped:
		return models.FraudOpen, true
	case models.OrderDelivered:
		return models.FraudDelivered, true
	case models.OrderCancelled:
		return models.FraudCancelled, true
	}
	return "", false
}

// Record moves the order to its new outcome. Safe to call twice for the same
// status (nothing changes) and for out-of-order calls (the older one loses).
func (h *FraudHistory) Record(ctx context.Context, o models.Order) {
	if err := h.record(ctx, o); err != nil {
		h.log.Error("fraud history update failed", "order", o.ID, "err", err)
	}
}

func (h *FraudHistory) record(ctx context.Context, o models.Order) error {
	s, err := h.settings.Get(ctx)
	if errors.Is(err, repository.ErrNotFound) {
		return nil // never configured = off
	}
	if err != nil {
		return err
	}
	if !s.Enabled {
		return nil
	}
	phone, ok := fraud.NormalizePhone(o.CustomerPhone)
	if !ok {
		return nil
	}
	at := o.UpdatedAt
	if at.IsZero() {
		at = h.now()
	}

	outcome, tracked := outcomeFor(o.Status)
	if !tracked {
		// e.g. a confirmed order put back to pending: take it out again.
		prev, err := h.repo.RemoveEvent(ctx, o.ID, at)
		if err != nil || prev == nil {
			return err
		}
		var d repository.FraudDelta
		d.Add(prev.Outcome, -1)
		return h.repo.AdjustCustomer(ctx, prev.Phone, d, at)
	}

	prev, applied, err := h.repo.SwapEvent(ctx, &models.FraudOrderEvent{
		OrderID: o.ID, Phone: phone, SellerID: o.SellerID, Outcome: outcome, UpdatedAt: at,
	})
	if err != nil || !applied {
		return err
	}
	if prev != nil && prev.Outcome == outcome {
		return nil // already counted
	}
	var d repository.FraudDelta
	d.Add(outcome, +1)
	if prev != nil {
		d.Add(prev.Outcome, -1)
	}
	return h.repo.AdjustCustomer(ctx, phone, d, at)
}

// RunRetention deletes records with no activity for RetentionDays, until ctx
// ends. It keeps running while the feature is off, so switching it off never
// leaves old data behind forever. RetentionDays <= 0 means keep everything.
func (h *FraudHistory) RunRetention(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		h.retentionOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (h *FraudHistory) retentionOnce(ctx context.Context) {
	s, err := h.settings.Get(ctx)
	if err != nil || s.RetentionDays <= 0 {
		return
	}
	cutoff := h.now().AddDate(0, 0, -s.RetentionDays)
	n, err := h.repo.DeleteInactiveBefore(ctx, cutoff)
	if err != nil && ctx.Err() == nil {
		h.log.Error("fraud retention cleanup failed", "err", err)
		return
	}
	if n > 0 {
		h.log.Info("fraud retention cleanup", "removed", n)
	}
}
