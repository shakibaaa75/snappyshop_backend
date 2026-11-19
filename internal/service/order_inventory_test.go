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

/* Fakes: only the methods OrderService touches on these paths. */

type invProducts struct {
	repository.ProductRepository
	p models.Product
}

func (f invProducts) FindByID(context.Context, string, string) (*models.Product, error) {
	cp := f.p
	return &cp, nil
}

type invDesigns struct {
	repository.StoreDesignRepository
}

func (invDesigns) GetBySeller(context.Context, string) (*models.StoreDesign, error) {
	return nil, repository.ErrNotFound
}

type invOrders struct {
	repository.OrderRepository
	created      *models.Order
	createErr    error
	updateErr    error
	statusBefore models.OrderStatus

	createErrs []error     // errors returned by the 1st, 2nd, ... Create call
	calls      int         // Create calls so far
	tried      [][2]string // {ID, Number} offered on each Create call
	seq        int64       // last value handed out by NextOrderSequence
	seqKeys    []string    // counter names requested
}

func (f *invOrders) NextOrderSequence(_ context.Context, counter string) (int64, error) {
	f.seq++
	f.seqKeys = append(f.seqKeys, counter)
	return f.seq, nil
}

func (f *invOrders) Create(_ context.Context, o *models.Order) error {
	f.calls++
	f.tried = append(f.tried, [2]string{o.ID, o.Number})
	if f.calls <= len(f.createErrs) && f.createErrs[f.calls-1] != nil {
		return f.createErrs[f.calls-1]
	}
	if f.createErr != nil {
		return f.createErr
	}
	cp := *o
	f.created = &cp
	return nil
}

func (f *invOrders) FindByID(_ context.Context, sellerID, id string) (*models.Order, error) {
	return &models.Order{ID: id, SellerID: sellerID, Status: f.statusBefore}, nil
}

func (f *invOrders) UpdateStatus(context.Context, string, string, models.OrderStatus, string, time.Time) error {
	return f.updateErr
}

func newInvService(p models.Product, o *invOrders) *OrderService {
	return NewOrderService(o, invProducts{p: p}, invDesigns{})
}

func validOrder(method string, lines ...OrderLineInput) CreateOrderInput {
	return CreateOrderInput{
		CustomerName: "Rahim Uddin", CustomerPhone: "01712345678",
		ShippingAddress: "House 4, Road 2", ShippingCity: "Dhaka",
		PaymentMethod: method, Lines: lines,
	}
}

func fieldMsg(err error, field string) string {
	var ae *apperr.Error
	if errors.As(err, &ae) {
		return ae.Fields[field]
	}
	return ""
}

func TestCreate_OutOfStockFromRepoBecomesValidationError(t *testing.T) {
	// The early check passes (stock looked fine), but another buyer took the
	// units before the atomic reservation: the repo reports it.
	orders := &invOrders{createErr: &repository.InsufficientStockError{ProductID: "p1", Name: "Blue Hoodie", Available: 2}}
	svc := newInvService(models.Product{ID: "p1", Name: "Blue Hoodie", Price: 100, Stock: 5, IsActive: true}, orders)

	_, err := svc.Create(context.Background(), "seller1", validOrder("cod", OrderLineInput{ProductID: "p1", Qty: 3}))
	if !isCode(err, apperr.CodeValidationFailed) {
		t.Fatalf("want validation error, got %v", err)
	}
	if got := fieldMsg(err, "lines"); !strings.Contains(got, "only has 2 left") {
		t.Fatalf("message should tell the buyer how many are left, got %q", got)
	}
}

func TestCreate_ZeroStockSaysOutOfStock(t *testing.T) {
	svc := newInvService(models.Product{ID: "p1", Name: "Blue Hoodie", Price: 100, Stock: 0, IsActive: true}, &invOrders{})
	_, err := svc.Create(context.Background(), "seller1", validOrder("cod", OrderLineInput{ProductID: "p1", Qty: 1}))
	if got := fieldMsg(err, "lines"); !strings.Contains(got, "out of stock") {
		t.Fatalf("want out of stock message, got %q (err %v)", got, err)
	}
}

func TestCreate_ProductWithVariantsRequiresAVariant(t *testing.T) {
	p := models.Product{ID: "p1", Name: "Tee", Price: 100, Stock: 10, IsActive: true,
		Combinations: []models.ProductCombination{{ID: "v1", Stock: 10}}}
	svc := newInvService(p, &invOrders{})
	_, err := svc.Create(context.Background(), "seller1", validOrder("cod", OrderLineInput{ProductID: "p1", Qty: 1}))
	if !isCode(err, apperr.CodeValidationFailed) {
		t.Fatalf("want validation error, got %v", err)
	}
}

func TestCreate_PaymentWindow(t *testing.T) {
	p := models.Product{ID: "p1", Name: "Tee", Price: 100, Stock: 10, IsActive: true}
	line := OrderLineInput{ProductID: "p1", Qty: 1}

	// Online payment: reservation expires after the window.
	o := &invOrders{}
	svc := newInvService(p, o)
	svc.SetPaymentWindow(30 * time.Minute)
	if _, err := svc.Create(context.Background(), "s", validOrder("bkash", line)); err != nil {
		t.Fatal(err)
	}
	if o.created.ReservationExpiresAt == nil {
		t.Fatal("bkash order must have a reservation expiry")
	}
	if d := time.Until(*o.created.ReservationExpiresAt); d < 29*time.Minute || d > 31*time.Minute {
		t.Fatalf("expiry should be ~30m away, got %v", d)
	}

	// Cash on delivery never expires.
	o = &invOrders{}
	svc = newInvService(p, o)
	if _, err := svc.Create(context.Background(), "s", validOrder("cod", line)); err != nil {
		t.Fatal(err)
	}
	if o.created.ReservationExpiresAt != nil {
		t.Fatal("COD order must not expire")
	}

	// Window 0 switches expiry off.
	o = &invOrders{}
	svc = newInvService(p, o)
	svc.SetPaymentWindow(0)
	if _, err := svc.Create(context.Background(), "s", validOrder("card", line)); err != nil {
		t.Fatal(err)
	}
	if o.created.ReservationExpiresAt != nil {
		t.Fatal("expiry disabled: order must not expire")
	}
}

func TestUpdateStatus_ReopenWithoutStockIsRejected(t *testing.T) {
	o := &invOrders{
		statusBefore: models.OrderCancelled,
		updateErr:    &repository.InsufficientStockError{ProductID: "p1", Name: "Tee", Available: 0},
	}
	svc := newInvService(models.Product{}, o)
	_, err := svc.UpdateStatus(context.Background(), "s", "o1", "pending", "")
	if !isCode(err, apperr.CodeValidationFailed) || !strings.Contains(fieldMsg(err, "status"), "out of stock") {
		t.Fatalf("want 'can't reopen / out of stock' validation error, got %v", err)
	}
}

func TestUpdateStatus_ConcurrentChangeIsConflict(t *testing.T) {
	o := &invOrders{statusBefore: models.OrderPending, updateErr: repository.ErrOrderChanged}
	svc := newInvService(models.Product{}, o)
	_, err := svc.UpdateStatus(context.Background(), "s", "o1", "cancelled", "")
	if !isCode(err, "ORDER_CHANGED") {
		t.Fatalf("want ORDER_CHANGED conflict, got %v", err)
	}
}
