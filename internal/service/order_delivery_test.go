package service

import (
	"context"
	"strings"
	"testing"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/repository"
)

/* Fakes ------------------------------------------------------------- */

// dlvDesigns is a store-design repo that knows one published store
// (slug "shop" -> sellerA) and, optionally, a "free delivery over X" amount.
type dlvDesigns struct {
	invDesigns
	threshold float64
}

func (d dlvDesigns) GetBySeller(context.Context, string) (*models.StoreDesign, error) {
	if d.threshold <= 0 {
		return &models.StoreDesign{Draft: map[string]any{}}, nil
	}
	return &models.StoreDesign{Draft: map[string]any{"freeDeliveryThreshold": d.threshold}}, nil
}

func (dlvDesigns) GetPublishedBySlug(_ context.Context, slug string) (*models.StoreDesign, error) {
	if slug == "shop" {
		return &models.StoreDesign{SellerID: "sellerA"}, nil
	}
	return nil, repository.ErrNotFound
}

// memSettings holds delivery settings per seller.
type memSettings struct {
	repository.DeliverySettingsRepository
	m   map[string]*models.DeliverySettings
	err error
}

func (f memSettings) Get(_ context.Context, sellerID string) (*models.DeliverySettings, error) {
	if f.err != nil {
		return nil, f.err
	}
	if s, ok := f.m[sellerID]; ok {
		return s, nil
	}
	return nil, repository.ErrNotFound
}

func item(id string, price float64, stock int) models.Product {
	return models.Product{ID: id, SellerID: "sellerA", Name: "Item " + id, Price: price, Stock: stock, IsActive: true}
}

type dlvEnv struct {
	svc    *OrderService
	orders *invOrders
}

// newDlvEnv builds an order service. With settings == nil no quoter is
// installed (the old flat-fee rule); otherwise orders are priced from them.
func newDlvEnv(threshold float64, settings map[string]*models.DeliverySettings, ps ...models.Product) dlvEnv {
	items := map[string]models.Product{}
	for _, p := range ps {
		items[p.SellerID+"/"+p.ID] = p
	}
	products := varProducts{items: items}
	designs := dlvDesigns{threshold: threshold}
	orders := &invOrders{}
	svc := NewOrderService(orders, products, designs)
	if settings != nil {
		svc.SetDeliveryQuoter(NewDeliveryService(memSettings{m: settings}, products, designs))
	}
	return dlvEnv{svc: svc, orders: orders}
}

func charges(in, out float64) models.DeliveryCharges {
	return models.DeliveryCharges{InsideDhaka: in, OutsideDhaka: out}
}

func settingsFor(global models.DeliveryCharges, overrides map[string]models.DeliveryCharges) map[string]*models.DeliverySettings {
	return map[string]*models.DeliverySettings{"sellerA": {SellerID: "sellerA", Global: global, Products: overrides}}
}

func orderIn(city string, lines ...OrderLineInput) CreateOrderInput {
	in := validOrder("cod", lines...)
	in.ShippingCity = city
	return in
}

func line(id string, qty int) OrderLineInput { return OrderLineInput{ProductID: id, Qty: qty} }

/* Create: zone, charge, breakdown, total -------------------------------- */

func TestCreate_StoresZoneChargeAndBreakdown(t *testing.T) {
	cases := []struct {
		name, city string
		wantZone   models.DeliveryZone
		wantCharge float64
	}{
		{"Dhaka is inside", "Dhaka", models.ZoneInsideDhaka, 60},
		{"Dhaka District is inside", "Dhaka District", models.ZoneInsideDhaka, 60},
		{"Gazipur is outside", "Gazipur", models.ZoneOutsideDhaka, 120},
		{"Narayanganj is outside", "Narayanganj", models.ZoneOutsideDhaka, 120},
		{"Madaripur is outside", "Madaripur", models.ZoneOutsideDhaka, 120},
		{"Sylhet is outside", "Sylhet", models.ZoneOutsideDhaka, 120},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := newDlvEnv(0, settingsFor(charges(60, 120), nil), item("p1", 500, 10))
			o, err := env.svc.Create(context.Background(), "sellerA", orderIn(c.city, line("p1", 2)))
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			if o.DeliveryZone != c.wantZone || o.Delivery != c.wantCharge || o.DeliveryBase != c.wantCharge || o.DeliveryWaived {
				t.Fatalf("zone=%q delivery=%v base=%v waived=%v; want %q %v %v false",
					o.DeliveryZone, o.Delivery, o.DeliveryBase, o.DeliveryWaived, c.wantZone, c.wantCharge, c.wantCharge)
			}
			if o.Subtotal != 1000 || o.Total != 1000+c.wantCharge || o.Discount != 0 {
				t.Fatalf("subtotal=%v total=%v discount=%v", o.Subtotal, o.Total, o.Discount)
			}
			if env.orders.created.DeliveryZone != c.wantZone {
				t.Fatal("the stored order must carry the zone")
			}
		})
	}
}

func TestCreate_OneChargePerOrder_HighestProductWins(t *testing.T) {
	// p2 has its own (higher) charge; p3 is free to deliver. Three products
	// must cost ONE delivery charge: the highest, not the sum.
	set := settingsFor(charges(60, 120), map[string]models.DeliveryCharges{
		"p2": charges(90, 200),
		"p3": charges(0, 0),
	})
	env := newDlvEnv(0, set, item("p1", 100, 9), item("p2", 100, 9), item("p3", 100, 9))
	o, err := env.svc.Create(context.Background(), "sellerA", orderIn("Gazipur", line("p1", 1), line("p2", 1), line("p3", 5)))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if o.Delivery != 200 {
		t.Fatalf("delivery = %v, want 200 (highest of 120, 200, 0)", o.Delivery)
	}
	inside, _ := newDlvEnv(0, set, item("p1", 100, 9), item("p2", 100, 9), item("p3", 100, 9)).
		svc.Create(context.Background(), "sellerA", orderIn("Dhaka", line("p1", 1), line("p3", 1)))
	if inside.Delivery != 60 {
		t.Fatalf("inside Dhaka, p1+p3: delivery = %v, want 60", inside.Delivery)
	}
}

func TestCreate_ZeroOverrideIsFreeDeliveryForThatProduct(t *testing.T) {
	set := settingsFor(charges(60, 120), map[string]models.DeliveryCharges{"p1": charges(0, 0)})
	env := newDlvEnv(0, set, item("p1", 100, 9))
	o, err := env.svc.Create(context.Background(), "sellerA", orderIn("Sylhet", line("p1", 1)))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if o.Delivery != 0 || o.Total != 100 {
		t.Fatalf("delivery=%v total=%v, want 0 and 100", o.Delivery, o.Total)
	}
}

func TestCreate_FreeDeliveryOverXWaivesAndKeepsTheBase(t *testing.T) {
	set := settingsFor(charges(60, 120), nil)

	// Subtotal 1000 reaches the 1000 threshold: waived, but the order still
	// records what the charge would have been.
	env := newDlvEnv(1000, set, item("p1", 500, 9))
	o, err := env.svc.Create(context.Background(), "sellerA", orderIn("Gazipur", line("p1", 2)))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if o.Delivery != 0 || o.DeliveryBase != 120 || !o.DeliveryWaived || o.Total != 1000 {
		t.Fatalf("delivery=%v base=%v waived=%v total=%v; want 0 120 true 1000", o.Delivery, o.DeliveryBase, o.DeliveryWaived, o.Total)
	}

	// One taka short of the threshold: charged.
	env = newDlvEnv(1000, set, item("p1", 999, 9))
	o, err = env.svc.Create(context.Background(), "sellerA", orderIn("Gazipur", line("p1", 1)))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if o.Delivery != 120 || o.DeliveryWaived || o.Total != 1119 {
		t.Fatalf("delivery=%v waived=%v total=%v; want 120 false 1119", o.Delivery, o.DeliveryWaived, o.Total)
	}
}

func TestCreate_NoWaiverNoteWhenNothingToWaive(t *testing.T) {
	// A free product over the threshold was never charged, so nothing was waived.
	set := settingsFor(charges(0, 0), nil)
	env := newDlvEnv(100, set, item("p1", 500, 9))
	o, err := env.svc.Create(context.Background(), "sellerA", orderIn("Dhaka", line("p1", 1)))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if o.DeliveryWaived || o.DeliveryBase != 0 || o.Delivery != 0 {
		t.Fatalf("waived=%v base=%v delivery=%v, want false 0 0", o.DeliveryWaived, o.DeliveryBase, o.Delivery)
	}
}

/* Existing stores keep today's behaviour ------------------------------- */

func TestCreate_SellerWhoNeverSavedKeepsTheOldRule(t *testing.T) {
	// Quoter installed, but sellerA has no saved settings -> legacy.
	noSettings := map[string]*models.DeliverySettings{}
	for _, city := range []string{"Dhaka", "Gazipur", "Sylhet"} {
		// threshold 500, subtotal 300: the old flat 60, in every district.
		env := newDlvEnv(500, noSettings, item("p1", 300, 9))
		o, err := env.svc.Create(context.Background(), "sellerA", orderIn(city, line("p1", 1)))
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if o.Delivery != 60 || o.Total != 360 {
			t.Fatalf("%s: delivery=%v total=%v, want 60 and 360", city, o.Delivery, o.Total)
		}
		// at or above the threshold: free.
		env = newDlvEnv(500, noSettings, item("p1", 500, 9))
		o, _ = env.svc.Create(context.Background(), "sellerA", orderIn(city, line("p1", 1)))
		if o.Delivery != 0 {
			t.Fatalf("%s: at the threshold delivery=%v, want 0", city, o.Delivery)
		}
	}
	// no threshold at all: free, as before.
	env := newDlvEnv(0, noSettings, item("p1", 300, 9))
	o, _ := env.svc.Create(context.Background(), "sellerA", orderIn("Dhaka", line("p1", 1)))
	if o.Delivery != 0 {
		t.Fatalf("no threshold: delivery=%v, want 0", o.Delivery)
	}
}

func TestCreate_WithoutAQuoterBehavesLikeBefore(t *testing.T) {
	// nil settings => no quoter installed: the pre-Delivery-Settings code path
	// (this is what every existing test and any deployment without the
	// feature gets).
	env := newDlvEnv(500, nil, item("p1", 300, 9))
	o, err := env.svc.Create(context.Background(), "sellerA", orderIn("Dhaka", line("p1", 1)))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if o.Delivery != 60 || o.Total != 360 {
		t.Fatalf("delivery=%v total=%v, want 60 and 360", o.Delivery, o.Total)
	}
	if o.DeliveryZone != models.ZoneInsideDhaka {
		t.Fatalf("zone = %q, want inside_dhaka (the zone is stored on this path too)", o.DeliveryZone)
	}
	env = newDlvEnv(500, nil, item("p1", 300, 9))
	o, err = env.svc.Create(context.Background(), "sellerA", orderIn("Gazipur", line("p1", 1)))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if o.DeliveryZone != models.ZoneOutsideDhaka || o.Delivery != 60 {
		t.Fatalf("Gazipur: zone=%q delivery=%v, want outside_dhaka and 60", o.DeliveryZone, o.Delivery)
	}
}

func TestCreate_SettingsLookupFailureFailsTheOrderInsteadOfUndercharging(t *testing.T) {
	products := varProducts{items: map[string]models.Product{"sellerA/p1": item("p1", 100, 9)}}
	designs := dlvDesigns{}
	orders := &invOrders{}
	svc := NewOrderService(orders, products, designs)
	svc.SetDeliveryQuoter(NewDeliveryService(memSettings{err: context.DeadlineExceeded}, products, designs))

	_, err := svc.Create(context.Background(), "sellerA", orderIn("Dhaka", line("p1", 1)))
	if err == nil {
		t.Fatal("an unreadable settings store must not silently mean free delivery")
	}
	if orders.calls != 0 {
		t.Fatal("no order may be stored (and no stock reserved) when delivery cannot be priced")
	}
}

/* Quote ---------------------------------------------------------------- */

func TestQuote_MatchesWhatCreateCharges(t *testing.T) {
	set := settingsFor(charges(60, 120), map[string]models.DeliveryCharges{"p2": charges(80, 250)})
	ps := []models.Product{item("p1", 400, 9), item("p2", 150, 9)}
	for _, tc := range []struct {
		city string
		thr  float64
	}{{"Dhaka", 0}, {"Gazipur", 0}, {"Gazipur", 700}, {"Dhaka", 700}, {"Rangpur", 5000}} {
		lines := []OrderLineInput{line("p1", 1), line("p2", 2)}

		q, err := newDlvEnv(tc.thr, set, ps...).svc.QuoteBySlug(context.Background(), "shop", QuoteInput{ShippingCity: tc.city, Lines: lines})
		if err != nil {
			t.Fatalf("Quote(%s): %v", tc.city, err)
		}
		o, err := newDlvEnv(tc.thr, set, ps...).svc.Create(context.Background(), "sellerA", orderIn(tc.city, lines...))
		if err != nil {
			t.Fatalf("Create(%s): %v", tc.city, err)
		}
		if q.Zone != o.DeliveryZone || q.Subtotal != o.Subtotal || q.Delivery != o.Delivery ||
			q.DeliveryBase != o.DeliveryBase || q.FreeDeliveryApplied != o.DeliveryWaived ||
			q.Discount != o.Discount || q.Total != o.Total {
			t.Fatalf("%s thr=%v: quote %+v disagrees with order zone=%q sub=%v del=%v base=%v waived=%v total=%v",
				tc.city, tc.thr, q, o.DeliveryZone, o.Subtotal, o.Delivery, o.DeliveryBase, o.DeliveryWaived, o.Total)
		}
	}
}

func TestQuote_DoesNotPlaceAnOrderOrCheckStock(t *testing.T) {
	env := newDlvEnv(0, settingsFor(charges(60, 120), nil), item("p1", 100, 1))
	// 5 requested, 1 in stock: a quote still prices it (the cart is editable).
	q, err := env.svc.QuoteBySlug(context.Background(), "shop", QuoteInput{ShippingCity: "Dhaka", Lines: []OrderLineInput{line("p1", 5)}})
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	if q.Subtotal != 500 || q.Delivery != 60 || q.Total != 560 {
		t.Fatalf("quote = %+v", q)
	}
	if env.orders.calls != 0 || len(env.orders.seqKeys) != 0 {
		t.Fatal("a quote must not create an order or use an order number")
	}
	// ...but placing that order is refused for stock.
	_, err = env.svc.Create(context.Background(), "sellerA", orderIn("Dhaka", line("p1", 5)))
	if !isCode(err, apperr.CodeValidationFailed) || !strings.Contains(fieldMsg(err, "lines"), "only has 1 left") {
		t.Fatalf("Create should refuse on stock, got %v", err)
	}
}

func TestQuote_Validation(t *testing.T) {
	env := newDlvEnv(0, settingsFor(charges(60, 120), nil), item("p1", 100, 9))
	ctx := context.Background()

	if _, err := env.svc.QuoteBySlug(ctx, "nope", QuoteInput{ShippingCity: "Dhaka", Lines: []OrderLineInput{line("p1", 1)}}); !isCode(err, apperr.CodeNotFound) {
		t.Fatalf("unknown store: want not found, got %v", err)
	}
	if _, err := env.svc.QuoteBySlug(ctx, "shop", QuoteInput{ShippingCity: "Dhaka"}); fieldMsg(err, "lines") == "" {
		t.Fatalf("empty cart: want a lines error, got %v", err)
	}
	if _, err := env.svc.QuoteBySlug(ctx, "shop", QuoteInput{ShippingCity: "Dhaka", Lines: []OrderLineInput{line("p1", 0)}}); fieldMsg(err, "lines") == "" {
		t.Fatalf("zero quantity: want a lines error, got %v", err)
	}
	if _, err := env.svc.QuoteBySlug(ctx, "shop", QuoteInput{ShippingCity: " ", Lines: []OrderLineInput{line("p1", 1)}}); fieldMsg(err, "shippingCity") == "" {
		t.Fatalf("no district: want a shippingCity error, got %v", err)
	}
	// A product that is not this seller's cannot be priced through this store.
	if _, err := env.svc.QuoteBySlug(ctx, "shop", QuoteInput{ShippingCity: "Dhaka", Lines: []OrderLineInput{line("someone-elses", 1)}}); !isCode(err, apperr.CodeValidationFailed) {
		t.Fatalf("foreign product: want validation error, got %v", err)
	}
}

/* Totals ---------------------------------------------------------------- */

func TestOrderTotal(t *testing.T) {
	cases := []struct{ sub, del, disc, want float64 }{
		{1000, 120, 0, 1120},
		{1000, 0, 0, 1000},
		{1000, 120, 100, 1020},
		{100, 60, 500, 0}, // a discount can never make the total negative
		{99.999, 60.004, 0, 160},
	}
	for _, c := range cases {
		if got := orderTotal(c.sub, c.del, c.disc); got != c.want {
			t.Errorf("orderTotal(%v, %v, %v) = %v, want %v", c.sub, c.del, c.disc, got, c.want)
		}
	}
}

/* Emails ---------------------------------------------------------------- */

func emailOrder() models.Order {
	return models.Order{
		Number: "SNAPPY-20261117-000001", CustomerName: "Rahim", CustomerEmail: "r@example.com",
		CustomerPhone: "01712345678", ShippingAddress: "House 4", ShippingCity: "Gazipur",
		PaymentMethod: models.PayCOD, Status: models.OrderConfirmed,
		Lines:    []models.OrderLine{{Name: "Tee", Qty: 1, Price: 500, Subtotal: 500}},
		Subtotal: 500, Delivery: 120, Total: 620,
		DeliveryZone: models.ZoneOutsideDhaka, DeliveryBase: 120,
	}
}

func TestOrderMoneyLines(t *testing.T) {
	o := emailOrder()
	del, label, disc := orderMoneyLines(o)
	if del != formatTaka(120) || label != "Delivery (Outside Dhaka)" || disc != "" {
		t.Fatalf("got %q %q %q", del, label, disc)
	}

	o.DeliveryZone = models.ZoneInsideDhaka
	o.Delivery, o.DeliveryWaived = 0, true
	del, label, _ = orderMoneyLines(o)
	if del != "Free" || label != "Delivery (Inside Dhaka)" {
		t.Fatalf("waived: got %q %q", del, label)
	}

	// An order from before Delivery Settings has no zone: the plain label.
	old := emailOrder()
	old.DeliveryZone, old.DeliveryBase = "", 0
	if _, label, _ := orderMoneyLines(old); label != "Delivery" {
		t.Fatalf("old order label = %q, want plain Delivery", label)
	}

	o = emailOrder()
	o.Discount = 50
	if _, _, disc := orderMoneyLines(o); disc != formatTaka(50) {
		t.Fatalf("discount text = %q", disc)
	}
}

func TestCustomerAndSellerEmailsShowZoneAndDiscount(t *testing.T) {
	o := emailOrder()
	o.Discount = 50
	o.Total = 570

	cust, ok := buildOrderStatusEmail(o)
	if !ok {
		t.Fatal("customer email not built")
	}
	sell, ok := buildSellerOrderEmail("Shakib", o, "https://example.com/o/1")
	if !ok {
		t.Fatal("seller email not built")
	}
	for name, e := range map[string]orderEmail{"customer": cust, "seller": sell} {
		for _, part := range []string{e.HTML, e.Text} {
			if !strings.Contains(part, "Delivery (Outside Dhaka)") {
				t.Errorf("%s email is missing the zone label", name)
			}
			if !strings.Contains(part, "Discount") {
				t.Errorf("%s email is missing the discount row", name)
			}
		}
	}

	// No discount, no zone (an old order): neither shows up.
	old := emailOrder()
	old.DeliveryZone = ""
	cust, _ = buildOrderStatusEmail(old)
	sell, _ = buildSellerOrderEmail("Shakib", old, "")
	for name, e := range map[string]orderEmail{"customer": cust, "seller": sell} {
		if strings.Contains(e.HTML, "Discount") || strings.Contains(e.Text, "Discount") {
			t.Errorf("%s email shows a discount row when there is none", name)
		}
		if strings.Contains(e.HTML, "Outside Dhaka") || strings.Contains(e.Text, "Outside Dhaka") {
			t.Errorf("%s email names a zone for an order that has none", name)
		}
	}
}
