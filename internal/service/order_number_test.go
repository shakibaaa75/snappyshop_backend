package service

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"shopbd/backend/internal/models"
	"shopbd/backend/internal/repository"
)

var (
	uuidV7Re      = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	orderNumberRe = regexp.MustCompile(`^SNAPPY-\d{8}-\d{6,}$`)
)

func okProduct() models.Product {
	return models.Product{ID: "p1", Name: "Tee", Price: 100, Stock: 10, IsActive: true}
}

var oneLine = OrderLineInput{ProductID: "p1", Qty: 1}

func TestFormatOrderNumber(t *testing.T) {
	// 20:00 UTC on 15 Nov is already 02:00 on 16 Nov in Bangladesh (UTC+6).
	at := time.Date(2026, 11, 15, 20, 0, 0, 0, time.UTC)
	if got, want := formatOrderNumber("SNAPPY", at, 123), "SNAPPY-20261116-000123"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	// Seven-digit sequences are never truncated.
	if got, want := formatOrderNumber("SNAPPY", at, 1234567), "SNAPPY-20261116-1234567"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestCreate_AssignsUUIDv7AndSnappyNumber(t *testing.T) {
	o := &invOrders{}
	svc := newInvService(okProduct(), o)
	order, err := svc.Create(context.Background(), "s1", validOrder("cod", oneLine))
	if err != nil {
		t.Fatal(err)
	}
	if !uuidV7Re.MatchString(order.ID) {
		t.Fatalf("order ID should be a UUIDv7, got %q", order.ID)
	}
	if !orderNumberRe.MatchString(order.Number) {
		t.Fatalf("order number should look like SNAPPY-YYYYMMDD-NNNNNN, got %q", order.Number)
	}
	if !strings.HasSuffix(order.Number, "-000001") {
		t.Fatalf("first order of the day should be sequence 1, got %q", order.Number)
	}
	if len(o.seqKeys) != 1 || !strings.HasPrefix(o.seqKeys[0], "order-number:") {
		t.Fatalf("expected one daily counter lookup, got %v", o.seqKeys)
	}
}

func TestCreate_SequentialOrdersGetIncreasingNumbers(t *testing.T) {
	o := &invOrders{}
	svc := newInvService(okProduct(), o)
	a, _ := svc.Create(context.Background(), "s1", validOrder("cod", oneLine))
	b, _ := svc.Create(context.Background(), "s1", validOrder("cod", oneLine))
	if a.Number == b.Number || a.ID == b.ID {
		t.Fatalf("orders must differ: %q/%q %q/%q", a.ID, a.Number, b.ID, b.Number)
	}
	if !strings.HasSuffix(a.Number, "-000001") || !strings.HasSuffix(b.Number, "-000002") {
		t.Fatalf("want sequence 1 then 2, got %q then %q", a.Number, b.Number)
	}
}

func TestSetOrderNumberPrefix(t *testing.T) {
	svc := newInvService(okProduct(), &invOrders{})
	svc.SetOrderNumberPrefix("  my-shop 24! ")
	if svc.numberPrefix != "MYSHOP24" {
		t.Fatalf("prefix should be cleaned to MYSHOP24, got %q", svc.numberPrefix)
	}
	svc.SetOrderNumberPrefix("!!!") // nothing usable: keep the current one
	if svc.numberPrefix != "MYSHOP24" {
		t.Fatalf("an unusable prefix must be ignored, got %q", svc.numberPrefix)
	}
}

func TestCreate_RetriesWithFreshIDAndNumberOnCollision(t *testing.T) {
	o := &invOrders{createErrs: []error{repository.ErrDuplicateOrder, repository.ErrDuplicateOrder}}
	svc := newInvService(okProduct(), o)

	order, err := svc.Create(context.Background(), "s1", validOrder("bkash", oneLine))
	if err != nil {
		t.Fatalf("two collisions then success should succeed, got %v", err)
	}
	if o.calls != 3 {
		t.Fatalf("want 3 Create attempts, got %d", o.calls)
	}
	ids, nums := map[string]bool{}, map[string]bool{}
	for _, tr := range o.tried {
		ids[tr[0]], nums[tr[1]] = true, true
	}
	if len(ids) != 3 || len(nums) != 3 {
		t.Fatalf("every attempt needs its own ID and number, got ids=%v numbers=%v", ids, nums)
	}
	if order.ID != o.tried[2][0] || order.Number != o.tried[2][1] {
		t.Fatalf("returned order must carry the successful attempt's ID/number")
	}
}

func TestCreate_GivesUpAfterMaxAttempts(t *testing.T) {
	errs := make([]error, 50)
	for i := range errs {
		errs[i] = repository.ErrDuplicateOrder
	}
	o := &invOrders{createErrs: errs}
	svc := newInvService(okProduct(), o)

	_, err := svc.Create(context.Background(), "s1", validOrder("cod", oneLine))
	if err == nil {
		t.Fatal("persistent collisions must end in an error, not an infinite loop")
	}
	if o.calls != maxOrderAttempts {
		t.Fatalf("want exactly %d attempts, got %d", maxOrderAttempts, o.calls)
	}
}

func TestCreate_OtherErrorsAreNotRetried(t *testing.T) {
	o := &invOrders{createErr: &repository.InsufficientStockError{ProductID: "p1", Name: "Tee", Available: 0}}
	svc := newInvService(okProduct(), o)
	if _, err := svc.Create(context.Background(), "s1", validOrder("cod", oneLine)); err == nil {
		t.Fatal("want an error")
	}
	if o.calls != 1 {
		t.Fatalf("an out-of-stock error must not be retried, got %d attempts", o.calls)
	}
}
