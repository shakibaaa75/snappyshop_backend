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
)

// ---- fakes: embed the interface, override only what the service calls ----

type fakeUsers struct {
	repository.UserRepository
	u *models.User
}

func (f fakeUsers) FindByID(_ context.Context, id string) (*models.User, error) {
	if f.u == nil || f.u.ID != id {
		return nil, repository.ErrNotFound
	}
	return f.u, nil
}

type fakeOrders struct{ list []*models.Order }

func (f *fakeOrders) ListByEmail(_ context.Context, email string, limit int, before time.Time) ([]*models.Order, error) {
	var out []*models.Order
	for _, o := range f.list {
		if !strings.EqualFold(o.CustomerEmail, email) {
			continue
		}
		if !before.IsZero() && !o.CreatedAt.Before(before) {
			continue
		}
		out = append(out, o)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (f *fakeOrders) FindByEmailAndID(_ context.Context, email, id string) (*models.Order, error) {
	for _, o := range f.list {
		if o.ID == id && strings.EqualFold(o.CustomerEmail, email) {
			return o, nil
		}
	}
	return nil, repository.ErrNotFound
}

type fakeShipments struct {
	repository.ShipmentRepository
	byOrder map[string]*models.Shipment
}

func (f fakeShipments) FindByOrder(_ context.Context, _, orderID string) (*models.Shipment, error) {
	if s, ok := f.byOrder[orderID]; ok {
		return s, nil
	}
	return nil, repository.ErrNotFound
}

type fakeStores struct {
	repository.StoreDesignRepository
}

func (fakeStores) GetBySeller(_ context.Context, id string) (*models.StoreDesign, error) {
	return &models.StoreDesign{SellerID: id, Slug: "shop-" + id, StoreName: "Shop " + id}, nil
}

type fakeIssues struct{ list []*models.OrderIssue }

func (f *fakeIssues) Create(_ context.Context, i *models.OrderIssue) error {
	for _, x := range f.list {
		if x.OrderID == i.OrderID && x.Status == models.IssueOpen {
			return repository.ErrIssueOpen
		}
	}
	f.list = append([]*models.OrderIssue{i}, f.list...)
	return nil
}

func (f *fakeIssues) ListByBuyer(_ context.Context, buyerID string, _ int) ([]*models.OrderIssue, error) {
	var out []*models.OrderIssue
	for _, i := range f.list {
		if i.BuyerID == buyerID {
			out = append(out, i)
		}
	}
	return out, nil
}

func (f *fakeIssues) Withdraw(_ context.Context, buyerID, id string, at time.Time) error {
	for _, i := range f.list {
		if i.ID == id && i.BuyerID == buyerID && i.Status == models.IssueOpen {
			i.Status = models.IssueWithdrawn
			i.UpdatedAt = at
			return nil
		}
	}
	return repository.ErrNotFound
}

type fakeNotifs struct {
	repository.NotificationRepository
	got []*models.Notification
}

func (f *fakeNotifs) Create(_ context.Context, n *models.Notification) error {
	f.got = append(f.got, n)
	return nil
}

// ---- helpers ----

var t0 = time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)

func order(id, email string, st models.OrderStatus, createdOffset time.Duration) *models.Order {
	return &models.Order{
		ID: id, SellerID: "s1", Number: "SB-" + id, CustomerEmail: email, Status: st,
		CreatedAt: t0.Add(createdOffset), UpdatedAt: t0.Add(createdOffset),
		Total: 1000, Lines: []models.OrderLine{{Name: "Shirt", Qty: 2}, {Name: "Cap", Qty: 1}},
		Events: []models.OrderStatusEvent{{Status: models.OrderPending, At: t0.Add(createdOffset)}},
	}
}

type env struct {
	svc    *BuyerOrderService
	orders *fakeOrders
	issues *fakeIssues
	notifs *fakeNotifs
}

func newEnv(verified bool, orders ...*models.Order) env {
	u := &models.User{ID: "u1", Email: "Rahman@Example.com", EmailVerified: verified}
	fo := &fakeOrders{list: orders}
	fi := &fakeIssues{}
	fn := &fakeNotifs{}
	svc := NewBuyerOrderService(fakeUsers{u: u}, fo, fakeShipments{byOrder: map[string]*models.Shipment{}}, fakeStores{}, fi, fn, nil)
	return env{svc, fo, fi, fn}
}

func isCode(err error, code string) bool {
	var ae *apperr.Error
	return errors.As(err, &ae) && ae.Code == code
}

// ---- tests ----

func TestOrdersOnlyForVerifiedEmailAndCaseInsensitive(t *testing.T) {
	ctx := context.Background()
	mine := order("a", "rahman@example.com", models.OrderPending, 0)
	other := order("b", "someone@else.com", models.OrderPending, 0)

	e := newEnv(true, mine, other)
	list, err := e.svc.Orders(ctx, "u1", 0, time.Time{})
	if err != nil || !list.EmailVerified || len(list.Orders) != 1 || list.Orders[0].ID != "a" {
		t.Fatalf("want only my order: %+v %v", list, err)
	}
	if got := list.Orders[0]; got.ItemCount != 3 || got.Seller.Slug != "shop-s1" || len(got.Preview) != 2 {
		t.Fatalf("bad summary: %+v", got)
	}

	// Unverified email: nothing is shown, and a flag tells the UI why.
	e = newEnv(false, mine)
	list, err = e.svc.Orders(ctx, "u1", 0, time.Time{})
	if err != nil || list.EmailVerified || len(list.Orders) != 0 {
		t.Fatalf("unverified email must see nothing: %+v %v", list, err)
	}
	if _, err := e.svc.Order(ctx, "u1", "a"); !isCode(err, apperr.NotFound().Code) {
		t.Fatalf("detail must be not found when unverified, got %v", err)
	}
}

func TestOrderDetailIsolation(t *testing.T) {
	e := newEnv(true, order("b", "someone@else.com", models.OrderDelivered, 0))
	if _, err := e.svc.Order(context.Background(), "u1", "b"); err == nil {
		t.Fatal("must not read another person's order")
	}
	if _, err := e.svc.ReportIssue(context.Background(), "u1", "b", models.IssueOther, "this is not my order really"); err == nil {
		t.Fatal("must not report on another person's order")
	}
}

func TestPaging(t *testing.T) {
	var os []*models.Order
	for i := 0; i < 5; i++ {
		os = append(os, order(string(rune('a'+i)), "rahman@example.com", models.OrderPending, -time.Duration(i)*time.Hour))
	}
	e := newEnv(true, os...)
	p1, _ := e.svc.Orders(context.Background(), "u1", 2, time.Time{})
	if len(p1.Orders) != 2 || p1.NextBefore == nil {
		t.Fatalf("page 1: %+v", p1)
	}
	p2, _ := e.svc.Orders(context.Background(), "u1", 2, *p1.NextBefore)
	if len(p2.Orders) != 2 || p2.Orders[0].ID != "c" {
		t.Fatalf("page 2: %+v", p2)
	}
	p3, _ := e.svc.Orders(context.Background(), "u1", 2, *p2.NextBefore)
	if len(p3.Orders) != 1 || p3.NextBefore != nil {
		t.Fatalf("page 3: %+v", p3)
	}
}

func TestTimeline(t *testing.T) {
	o := order("a", "x@y.com", models.OrderShipped, 0)
	o.Events = append(o.Events,
		models.OrderStatusEvent{Status: models.OrderConfirmed, At: t0.Add(time.Hour)},
		models.OrderStatusEvent{Status: models.OrderShipped, At: t0.Add(2 * time.Hour)},
	)
	steps := BuildTimeline(o, nil)
	want := []struct {
		key  string
		done bool
	}{{"placed", true}, {"confirmed", true}, {"shipped", true}, {"delivered", false}}
	if len(steps) != len(want) {
		t.Fatalf("steps: %+v", steps)
	}
	for i, w := range want {
		if steps[i].Key != w.key || steps[i].Done != w.done {
			t.Errorf("step %d: %+v want %+v", i, steps[i], w)
		}
	}
	if steps[3].At != nil {
		t.Error("undone step must have no time")
	}

	// Cancelled after confirmation: placed, confirmed, cancelled only.
	c := order("c", "x@y.com", models.OrderCancelled, 0)
	c.Events = append(c.Events,
		models.OrderStatusEvent{Status: models.OrderConfirmed, At: t0.Add(time.Hour)},
		models.OrderStatusEvent{Status: models.OrderCancelled, At: t0.Add(3 * time.Hour)},
	)
	cs := BuildTimeline(c, nil)
	if len(cs) != 3 || cs[2].Key != "cancelled" || cs[2].At == nil {
		t.Fatalf("cancelled timeline: %+v", cs)
	}
}

func TestTrackingOnlyWhenBookedAndSafeURL(t *testing.T) {
	ctx := context.Background()
	o := order("a", "rahman@example.com", models.OrderShipped, 0)
	e := newEnv(true, o)
	e.svc.shipments = fakeShipments{byOrder: map[string]*models.Shipment{
		"a": {State: models.ShipmentBooked, Provider: "steadfast", TrackingCode: "ABC123", TrackingURL: "javascript:alert(1)", UpdatedAt: t0.Add(time.Hour)},
	}}
	d, err := e.svc.Order(ctx, "u1", "a")
	if err != nil || d.Tracking == nil || d.Tracking.Code != "ABC123" || d.Tracking.URL != "" {
		t.Fatalf("unsafe tracking url must be dropped: %+v %v", d.Tracking, err)
	}

	e.svc.shipments = fakeShipments{byOrder: map[string]*models.Shipment{
		"a": {State: models.ShipmentPending, Provider: "steadfast"},
	}}
	d, _ = e.svc.Order(ctx, "u1", "a")
	if d.Tracking != nil {
		t.Fatal("a pending shipment must not be shown as tracking")
	}
}

func TestReportIssueRules(t *testing.T) {
	ctx := context.Background()
	msg := "The parcel never arrived at my door."

	e := newEnv(true,
		order("sh", "rahman@example.com", models.OrderShipped, 0),
		order("dl", "rahman@example.com", models.OrderDelivered, 0),
		order("pe", "rahman@example.com", models.OrderPending, 0),
	)

	// Not received: fine once shipped, not while pending.
	if _, err := e.svc.ReportIssue(ctx, "u1", "sh", models.IssueNotReceived, msg); err != nil {
		t.Fatalf("shipped + not_received should work: %v", err)
	}
	if len(e.notifs.got) != 1 || e.notifs.got[0].RecipientIDs[0] != "s1" ||
		e.notifs.got[0].LinkURL != "/seller/shop-s1/orders/sh" {
		t.Fatalf("seller must be notified: %+v", e.notifs.got)
	}
	if _, err := e.svc.ReportIssue(ctx, "u1", "pe", models.IssueNotReceived, msg); err == nil {
		t.Fatal("pending order cannot be 'not received'")
	}
	// A damaged item only makes sense after delivery.
	if _, err := e.svc.ReportIssue(ctx, "u1", "sh", models.IssueDamaged, msg); err == nil {
		t.Fatal("damaged needs a delivered order")
	}
	if _, err := e.svc.ReportIssue(ctx, "u1", "dl", models.IssueDamaged, msg); err != nil {
		t.Fatalf("delivered + damaged should work: %v", err)
	}

	// One open issue per order.
	if _, err := e.svc.ReportIssue(ctx, "u1", "dl", models.IssueWrongItem, msg); !isCode(err, "issue_open") {
		t.Fatalf("second open issue must be a conflict, got %v", err)
	}

	// Validation.
	if _, err := e.svc.ReportIssue(ctx, "u1", "pe", "nonsense", "short"); err == nil {
		t.Fatal("bad type and short message must fail")
	}
	if _, err := e.svc.ReportIssue(ctx, "u1", "pe", models.IssueOther, strings.Repeat("x", issueMaxMessage+1)); err == nil {
		t.Fatal("too long message must fail")
	}

	// Detail shows the open issue and hides the report form options.
	d, _ := e.svc.Order(ctx, "u1", "dl")
	if !d.HasOpenIssue || len(d.Issues) != 1 || len(d.ReportTypes) != 0 {
		t.Fatalf("detail must show open issue and no new report types: %+v", d)
	}

	// Withdraw: only my own, only while open; then I can report again.
	id := d.Issues[0].ID
	if err := e.svc.WithdrawIssue(ctx, "someone-else", id); err == nil {
		t.Fatal("cannot withdraw another buyer's issue")
	}
	if err := e.svc.WithdrawIssue(ctx, "u1", id); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.WithdrawIssue(ctx, "u1", id); err == nil {
		t.Fatal("cannot withdraw twice")
	}
	d, _ = e.svc.Order(ctx, "u1", "dl")
	if d.HasOpenIssue || len(d.ReportTypes) == 0 {
		t.Fatalf("after withdrawing, reporting is possible again: %+v", d)
	}
}

func TestAllowedIssueTypes(t *testing.T) {
	cancelledPaid := order("x", "a@b.c", models.OrderCancelled, 0)
	cancelledPaid.Paid = true
	got := AllowedIssueTypes(cancelledPaid)
	if len(got) != 2 || got[0] != models.IssueRefund || got[1] != models.IssueOther {
		t.Fatalf("paid + cancelled: %v", got)
	}
	cancelledUnpaid := order("y", "a@b.c", models.OrderCancelled, 0)
	if got := AllowedIssueTypes(cancelledUnpaid); len(got) != 1 || got[0] != models.IssueOther {
		t.Fatalf("unpaid + cancelled: %v", got)
	}
}
