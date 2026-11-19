package mongorepo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/mongo"

	"shopbd/backend/internal/database"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/repository"
)

// Needs TEST_MONGODB_URI / TEST_MONGODB_DB like the other integration tests.
// Works on a replica set (real transactions) and on a standalone mongod
// (compensating fallback); run it against a replica set before shipping.

const invSeller = "seller-inv"

func seedProduct(t *testing.T, ctx context.Context, db *mongo.Database, id string, stock int, combos ...models.ProductCombination) {
	t.Helper()
	p := &models.Product{
		ID: id, SellerID: invSeller, Name: "Product " + id, Slug: id,
		Price: 100, Stock: stock, IsActive: true, Combinations: combos,
	}
	if len(combos) > 0 {
		p.Stock = 0
		for _, c := range combos {
			p.Stock += c.Stock
		}
	}
	if err := NewProducts(db).Create(ctx, p); err != nil {
		t.Fatalf("seed product: %v", err)
	}
}

func newOrder(n string, paid bool, lines ...models.OrderLine) *models.Order {
	now := time.Now().UTC()
	return &models.Order{
		ID: models.NewID(), SellerID: invSeller, Number: n, Status: models.OrderPending,
		PaymentMethod: models.PayBkash, Paid: paid, Lines: lines, CreatedAt: now, UpdatedAt: now,
	}
}

func stockOf(t *testing.T, ctx context.Context, db *mongo.Database, id string) *models.Product {
	t.Helper()
	p, err := NewProducts(db).FindByID(ctx, invSeller, id)
	if err != nil {
		t.Fatalf("find product: %v", err)
	}
	return p
}

func TestOrderStockLifecycle(t *testing.T) {
	db, ctx := testDB(t)
	orders := NewOrders(db)
	seedProduct(t, ctx, db, "hoodie", 5)

	// Placing an order reduces stock.
	o1 := newOrder("N1", false, models.OrderLine{ProductID: "hoodie", Qty: 3})
	if err := orders.Create(ctx, o1); err != nil {
		t.Fatal(err)
	}
	if got := stockOf(t, ctx, db, "hoodie").Stock; got != 2 {
		t.Fatalf("after order of 3, stock = %d, want 2", got)
	}

	// Asking for more than is left fails and changes nothing (no partial reserve).
	seedProduct(t, ctx, db, "cap", 10)
	o2 := newOrder("N2", false,
		models.OrderLine{ProductID: "cap", Qty: 4},
		models.OrderLine{ProductID: "hoodie", Qty: 3}, // only 2 left
	)
	err := orders.Create(ctx, o2)
	var ise *repository.InsufficientStockError
	if !errors.As(err, &ise) || ise.Available != 2 {
		t.Fatalf("want InsufficientStockError with 2 available, got %v", err)
	}
	if got := stockOf(t, ctx, db, "cap").Stock; got != 10 {
		t.Fatalf("failed order must roll back earlier lines: cap stock = %d, want 10", got)
	}
	if _, err := orders.FindByID(ctx, invSeller, o2.ID); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("failed order must not be stored, got %v", err)
	}

	// Cancelling restores stock - exactly once.
	if err := orders.UpdateStatus(ctx, invSeller, o1.ID, models.OrderCancelled, "", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if got := stockOf(t, ctx, db, "hoodie").Stock; got != 5 {
		t.Fatalf("after cancel, stock = %d, want 5", got)
	}
	if err := orders.UpdateStatus(ctx, invSeller, o1.ID, models.OrderCancelled, "again", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if got := stockOf(t, ctx, db, "hoodie").Stock; got != 5 {
		t.Fatalf("second cancel must not restock again: stock = %d, want 5", got)
	}

	// Re-opening a cancelled order takes the stock again; refused if gone.
	if err := orders.UpdateStatus(ctx, invSeller, o1.ID, models.OrderPending, "", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if got := stockOf(t, ctx, db, "hoodie").Stock; got != 2 {
		t.Fatalf("after reopen, stock = %d, want 2", got)
	}
}

func TestOrderStockVariants(t *testing.T) {
	db, ctx := testDB(t)
	orders := NewOrders(db)
	seedProduct(t, ctx, db, "tee", 0,
		models.ProductCombination{ID: "m", Stock: 2},
		models.ProductCombination{ID: "l", Stock: 4},
	)

	o := newOrder("V1", false, models.OrderLine{ProductID: "tee", VariantID: "m", Qty: 2})
	if err := orders.Create(ctx, o); err != nil {
		t.Fatal(err)
	}
	p := stockOf(t, ctx, db, "tee")
	if p.Stock != 4 || p.Combinations[0].Stock != 0 || p.Combinations[1].Stock != 4 {
		t.Fatalf("variant M should be 0, L untouched, total 4; got total=%d %+v", p.Stock, p.Combinations)
	}

	// Variant M is sold out even though the product total is not.
	err := orders.Create(ctx, newOrder("V2", false, models.OrderLine{ProductID: "tee", VariantID: "m", Qty: 1}))
	var ise *repository.InsufficientStockError
	if !errors.As(err, &ise) || ise.Available != 0 {
		t.Fatalf("want sold-out error for variant M, got %v", err)
	}

	if err := orders.UpdateStatus(ctx, invSeller, o.ID, models.OrderCancelled, "", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	p = stockOf(t, ctx, db, "tee")
	if p.Stock != 6 || p.Combinations[0].Stock != 2 {
		t.Fatalf("cancel should restore variant M to 2 and total to 6; got total=%d %+v", p.Stock, p.Combinations)
	}
}

// The core oversell guarantee: many buyers race for the last units.
func TestNoOversellUnderConcurrency(t *testing.T) {
	db, ctx := testDB(t)
	orders := NewOrders(db)
	const stock, buyers = 5, 30
	seedProduct(t, ctx, db, "rare", stock)

	var ok, soldOut, other atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < buyers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := orders.Create(ctx, newOrder(fmt.Sprintf("C%d", i), false, models.OrderLine{ProductID: "rare", Qty: 1}))
			var ise *repository.InsufficientStockError
			switch {
			case err == nil:
				ok.Add(1)
			case errors.As(err, &ise):
				soldOut.Add(1)
			default:
				t.Errorf("unexpected error: %v", err)
				other.Add(1)
			}
		}(i)
	}
	wg.Wait()

	if ok.Load() != stock {
		t.Fatalf("exactly %d orders must succeed, got %d (sold out: %d, other: %d)", stock, ok.Load(), soldOut.Load(), other.Load())
	}
	if got := stockOf(t, ctx, db, "rare").Stock; got != 0 {
		t.Fatalf("stock must end at 0 (never negative), got %d", got)
	}
}

func TestUnpaidOrdersExpireAndRestock(t *testing.T) {
	db, ctx := testDB(t)
	orders := NewOrders(db)
	seedProduct(t, ctx, db, "mug", 10)

	past := time.Now().UTC().Add(-time.Minute)
	future := time.Now().UTC().Add(time.Hour)

	expired := newOrder("E1", false, models.OrderLine{ProductID: "mug", Qty: 2})
	expired.ReservationExpiresAt = &past
	paid := newOrder("E2", false, models.OrderLine{ProductID: "mug", Qty: 3})
	paid.ReservationExpiresAt = &past
	waiting := newOrder("E3", false, models.OrderLine{ProductID: "mug", Qty: 1})
	waiting.ReservationExpiresAt = &future
	cod := newOrder("E4", false, models.OrderLine{ProductID: "mug", Qty: 1})
	cod.PaymentMethod = models.PayCOD // no expiry set
	for _, o := range []*models.Order{expired, paid, waiting, cod} {
		if err := orders.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	if got := stockOf(t, ctx, db, "mug").Stock; got != 3 {
		t.Fatalf("setup: stock = %d, want 3", got)
	}
	// Seller marks E2 paid before the deadline: it must survive.
	if err := orders.MarkPaid(ctx, invSeller, paid.ID, true); err != nil {
		t.Fatal(err)
	}

	got, err := orders.ExpireUnpaid(ctx, time.Now().UTC(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != expired.ID {
		t.Fatalf("only E1 should expire, got %d orders", len(got))
	}
	if s := stockOf(t, ctx, db, "mug").Stock; s != 5 {
		t.Fatalf("expired order's 2 units must return: stock = %d, want 5", s)
	}
	o, _ := orders.FindByID(ctx, invSeller, expired.ID)
	if o.Status != models.OrderCancelled || o.StockReserved {
		t.Fatalf("expired order should be cancelled and unreserved, got %s reserved=%v", o.Status, o.StockReserved)
	}

	// Running the sweeper again must not restock a second time.
	got, err = orders.ExpireUnpaid(ctx, time.Now().UTC(), 100)
	if err != nil || len(got) != 0 {
		t.Fatalf("second sweep should do nothing, got %d, %v", len(got), err)
	}
	if s := stockOf(t, ctx, db, "mug").Stock; s != 5 {
		t.Fatalf("stock changed on second sweep: %d, want 5", s)
	}
}

/* ---------------- order IDs / numbers ---------------- */

func TestOrderSequenceNeverRepeatsUnderConcurrency(t *testing.T) {
	db, ctx := testDB(t)
	orders := NewOrders(db)

	const workers = 60
	got := make(chan int64, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, err := orders.NextOrderSequence(ctx, "order-number:20261115")
			if err != nil {
				t.Errorf("sequence: %v", err)
				return
			}
			got <- n
		}()
	}
	wg.Wait()
	close(got)

	seen := map[int64]bool{}
	for n := range got {
		if seen[n] {
			t.Fatalf("sequence value %d was issued twice", n)
		}
		seen[n] = true
	}
	for n := int64(1); n <= workers; n++ {
		if !seen[n] {
			t.Fatalf("sequence has a gap at %d (want exactly 1..%d)", n, workers)
		}
	}

	// A different day is a different counter and starts from 1.
	if n, err := orders.NextOrderSequence(ctx, "order-number:20261116"); err != nil || n != 1 {
		t.Fatalf("new day should start at 1, got %d, %v", n, err)
	}
}

// A collision must be reported as ErrDuplicateOrder AND must not leave stock
// reserved, otherwise the service's retry would double-reserve.
func TestDuplicateOrderNumberRollsBackStock(t *testing.T) {
	db, ctx := testDB(t)
	if err := database.EnsureIndexes(ctx, db, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("indexes: %v", err)
	}
	orders := NewOrders(db)
	seedProduct(t, ctx, db, "poster", 10)

	first := newOrder("SNAPPY-20261115-000001", false, models.OrderLine{ProductID: "poster", Qty: 2})
	if err := orders.Create(ctx, first); err != nil {
		t.Fatal(err)
	}

	// Same seller + same number, different ID.
	clash := newOrder("SNAPPY-20261115-000001", false, models.OrderLine{ProductID: "poster", Qty: 3})
	if err := orders.Create(ctx, clash); !errors.Is(err, repository.ErrDuplicateOrder) {
		t.Fatalf("want ErrDuplicateOrder for a repeated number, got %v", err)
	}
	if got := stockOf(t, ctx, db, "poster").Stock; got != 8 {
		t.Fatalf("the clashing order must not keep its stock: stock = %d, want 8", got)
	}

	// Same ID, different number.
	sameID := newOrder("SNAPPY-20261115-000002", false, models.OrderLine{ProductID: "poster", Qty: 1})
	sameID.ID = first.ID
	if err := orders.Create(ctx, sameID); !errors.Is(err, repository.ErrDuplicateOrder) {
		t.Fatalf("want ErrDuplicateOrder for a repeated ID, got %v", err)
	}
	if got := stockOf(t, ctx, db, "poster").Stock; got != 8 {
		t.Fatalf("stock = %d, want 8", got)
	}

	// With a fresh ID and number (what the service's retry does) it goes through.
	retry := newOrder("SNAPPY-20261115-000003", false, models.OrderLine{ProductID: "poster", Qty: 3})
	if err := orders.Create(ctx, retry); err != nil {
		t.Fatal(err)
	}
	if got := stockOf(t, ctx, db, "poster").Stock; got != 5 {
		t.Fatalf("stock = %d, want 5", got)
	}
}
