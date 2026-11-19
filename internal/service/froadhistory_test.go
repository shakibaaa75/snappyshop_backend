package service

import (
	"context"
	"testing"
	"time"

	"shopbd/backend/internal/models"
	"shopbd/backend/internal/repository"
)

type fakeFraudSettings struct{ s *models.FraudSettings }

func (f fakeFraudSettings) Get(context.Context) (*models.FraudSettings, error) {
	if f.s == nil {
		return nil, repository.ErrNotFound
	}
	return f.s, nil
}
func (f fakeFraudSettings) Upsert(context.Context, *models.FraudSettings) error { return nil }

type fakeFraudRepo struct {
	events map[string]models.FraudOrderEvent
	cust   map[string]*models.FraudCustomer
}

func newFakeFraudRepo() *fakeFraudRepo {
	return &fakeFraudRepo{events: map[string]models.FraudOrderEvent{}, cust: map[string]*models.FraudCustomer{}}
}

func (f *fakeFraudRepo) SwapEvent(_ context.Context, ev *models.FraudOrderEvent) (*models.FraudOrderEvent, bool, error) {
	old, ok := f.events[ev.OrderID]
	if ok && old.UpdatedAt.After(ev.UpdatedAt) {
		return nil, false, nil
	}
	f.events[ev.OrderID] = *ev
	if !ok {
		return nil, true, nil
	}
	return &old, true, nil
}
func (f *fakeFraudRepo) RemoveEvent(_ context.Context, id string, at time.Time) (*models.FraudOrderEvent, error) {
	old, ok := f.events[id]
	if !ok || old.UpdatedAt.After(at) {
		return nil, nil
	}
	delete(f.events, id)
	return &old, nil
}
func (f *fakeFraudRepo) AdjustCustomer(_ context.Context, phone string, d repository.FraudDelta, at time.Time) error {
	c := f.cust[phone]
	if c == nil {
		c = &models.FraudCustomer{Phone: phone}
		f.cust[phone] = c
	}
	c.Delivered += d.Delivered
	c.Cancelled += d.Cancelled
	c.Failed += d.Failed
	c.Open += d.Open
	return nil
}
func (f *fakeFraudRepo) GetCustomer(context.Context, string) (*models.FraudCustomer, error) {
	return nil, repository.ErrNotFound
}
func (f *fakeFraudRepo) SaveExternal(context.Context, string, *models.FraudExternal, time.Time) error {
	return nil
}
func (f *fakeFraudRepo) DeleteInactiveBefore(context.Context, time.Time) (int64, error) {
	return 0, nil
}

func fraudOrder(id string, st models.OrderStatus, at time.Time) models.Order {
	return models.Order{ID: id, SellerID: "s1", CustomerPhone: "+8801712345678", Status: st, UpdatedAt: at}
}

func TestFraudHistoryMovesOrderBetweenCounters(t *testing.T) {
	repo := newFakeFraudRepo()
	h := NewFraudHistory(fakeFraudSettings{&models.FraudSettings{Enabled: true}}, repo, nil)
	ctx := context.Background()
	t0 := time.Now()
	const p = "01712345678"

	h.Record(ctx, fraudOrder("o1", models.OrderPending, t0)) // not tracked
	if repo.cust[p] != nil {
		t.Fatal("pending must not be recorded")
	}
	h.Record(ctx, fraudOrder("o1", models.OrderConfirmed, t0.Add(1*time.Second)))
	h.Record(ctx, fraudOrder("o1", models.OrderConfirmed, t0.Add(1*time.Second))) // replay
	if c := repo.cust[p]; c.Open != 1 {
		t.Fatalf("open=%d want 1", c.Open)
	}
	h.Record(ctx, fraudOrder("o1", models.OrderDelivered, t0.Add(2*time.Second)))
	if c := repo.cust[p]; c.Open != 0 || c.Delivered != 1 {
		t.Fatalf("after deliver %+v", c)
	}
	// A stale (older) update arriving late must be ignored.
	h.Record(ctx, fraudOrder("o1", models.OrderCancelled, t0.Add(1500*time.Millisecond)))
	if c := repo.cust[p]; c.Delivered != 1 || c.Cancelled != 0 {
		t.Fatalf("stale update applied %+v", c)
	}
	// Re-opened to pending: the order leaves the record entirely.
	h.Record(ctx, fraudOrder("o1", models.OrderPending, t0.Add(3*time.Second)))
	if c := repo.cust[p]; c.Delivered != 0 || c.Open != 0 {
		t.Fatalf("after pending %+v", c)
	}
}

func TestFraudHistoryOffByDefault(t *testing.T) {
	repo := newFakeFraudRepo()
	for _, s := range []*models.FraudSettings{nil, {Enabled: false}} {
		NewFraudHistory(fakeFraudSettings{s}, repo, nil).Record(context.Background(), fraudOrder("o2", models.OrderDelivered, time.Now()))
	}
	if len(repo.events) != 0 || len(repo.cust) != 0 {
		t.Fatal("recorded while disabled")
	}
}

func TestFraudHistoryIgnoresBadPhone(t *testing.T) {
	repo := newFakeFraudRepo()
	o := fraudOrder("o3", models.OrderDelivered, time.Now())
	o.CustomerPhone = "12345"
	NewFraudHistory(fakeFraudSettings{&models.FraudSettings{Enabled: true}}, repo, nil).Record(context.Background(), o)
	if len(repo.cust) != 0 {
		t.Fatal("recorded invalid phone")
	}
}
