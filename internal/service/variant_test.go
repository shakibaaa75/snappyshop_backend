package service

import (
	"context"
	"math"
	"strings"
	"testing"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/repository"
)

// varProducts is a product store that, unlike invProducts, honours the seller
// and product IDs it is asked for, so ownership rules can be tested.
type varProducts struct {
	repository.ProductRepository
	items map[string]models.Product // "sellerID/productID"
}

func (f varProducts) FindByID(_ context.Context, sellerID, id string) (*models.Product, error) {
	p, ok := f.items[sellerID+"/"+id]
	if !ok {
		return nil, repository.ErrNotFound
	}
	cp := p
	return &cp, nil
}

// tee is a product with two options and four variants, each with its own price.
// Its base price (100) is deliberately different from every variant price so a
// fallback to the base price cannot go unnoticed.
func tee(id string) models.Product {
	opts := []models.ProductOption{
		{ID: "size", Name: "Size", Values: []models.ProductOptionValue{{ID: "s", Label: "S"}, {ID: "m", Label: "M"}}},
		{ID: "color", Name: "Color", Values: []models.ProductOptionValue{{ID: "red", Label: "Red"}, {ID: "blue", Label: "Blue"}}},
	}
	combo := func(vid, size, color string, price float64, stock int) models.ProductCombination {
		return models.ProductCombination{ID: vid, ValueIDs: map[string]string{"size": size, "color": color}, Price: price, Stock: stock}
	}
	return models.Product{
		ID: id, SellerID: "sellerA", Name: "Tee", Price: 100, Stock: 100, IsActive: true,
		Options: opts,
		Combinations: []models.ProductCombination{
			combo(id+"-s-red", "s", "red", 150, 5),
			combo(id+"-m-red", "m", "red", 175, 1),
			combo(id+"-s-blue", "s", "blue", 160, 0),
			combo(id+"-m-blue", "m", "blue", 170, 8),
		},
	}
}

func newVarService(orders *invOrders, ps ...models.Product) *OrderService {
	items := map[string]models.Product{}
	for _, p := range ps {
		items[p.SellerID+"/"+p.ID] = p
	}
	return NewOrderService(orders, varProducts{items: items}, invDesigns{})
}

func varOrder(lines ...OrderLineInput) CreateOrderInput { return validOrder("cod", lines...) }

func TestVariantRequiredWhenProductHasVariants(t *testing.T) {
	orders := &invOrders{}
	svc := newVarService(orders, tee("p1"))
	_, err := svc.Create(context.Background(), "sellerA", varOrder(OrderLineInput{ProductID: "p1", Qty: 1}))
	if !isCode(err, apperr.CodeValidationFailed) || !strings.Contains(fieldMsg(err, "lines"), "choose an option") {
		t.Fatalf("want a 'choose an option' validation error, got %v", err)
	}
	if orders.calls != 0 {
		t.Fatal("no order may be created, and no stock reserved")
	}
}

func TestVariantLineUsesTheVariantPriceNotTheBasePrice(t *testing.T) {
	orders := &invOrders{}
	svc := newVarService(orders, tee("p1"))
	o, err := svc.Create(context.Background(), "sellerA", varOrder(OrderLineInput{ProductID: "p1", VariantID: "p1-s-red", Qty: 2}))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	l := o.Lines[0]
	if l.Price != 150 || l.Subtotal != 300 {
		t.Fatalf("price must be the variant's 150 (subtotal 300), got %v / %v", l.Price, l.Subtotal)
	}
	if l.VariantID != "p1-s-red" || l.Variant != "S / Red" {
		t.Fatalf("variant must be recorded on the line, got %q / %q", l.VariantID, l.Variant)
	}
	if o.Subtotal != 300 {
		t.Fatalf("order subtotal should be 300, got %v", o.Subtotal)
	}
}

func TestVariantWithoutAPriceIsRefusedNeverSoldAtTheBasePrice(t *testing.T) {
	for name, price := range map[string]float64{"zero": 0, "negative": -5, "NaN": math.NaN(), "infinite": math.Inf(1)} {
		t.Run(name, func(t *testing.T) {
			p := tee("p1")
			p.Combinations[0].Price = price
			orders := &invOrders{}
			svc := newVarService(orders, p)
			_, err := svc.Create(context.Background(), "sellerA", varOrder(OrderLineInput{ProductID: "p1", VariantID: "p1-s-red", Qty: 1}))
			if !isCode(err, apperr.CodeValidationFailed) || !strings.Contains(fieldMsg(err, "lines"), "can't be ordered") {
				t.Fatalf("want a refusal, got %v", err)
			}
			if orders.calls != 0 {
				t.Fatalf("an order was created for an unpriced variant (price %v)", price)
			}
		})
	}
}

func TestVariantOfAnotherProductIsRefused(t *testing.T) {
	orders := &invOrders{}
	svc := newVarService(orders, tee("p1"), tee("p2"))
	// p2's variant, offered on a line for p1.
	_, err := svc.Create(context.Background(), "sellerA", varOrder(OrderLineInput{ProductID: "p1", VariantID: "p2-s-red", Qty: 1}))
	if !strings.Contains(fieldMsg(err, "lines"), "no longer available") {
		t.Fatalf("want 'no longer available', got %v", err)
	}
	if orders.calls != 0 {
		t.Fatal("no order may be created")
	}
}

func TestProductOfAnotherSellerIsNotFound(t *testing.T) {
	orders := &invOrders{}
	svc := newVarService(orders, tee("p1")) // belongs to sellerA
	_, err := svc.Create(context.Background(), "sellerB", varOrder(OrderLineInput{ProductID: "p1", VariantID: "p1-s-red", Qty: 1}))
	if !strings.Contains(fieldMsg(err, "lines"), "no longer available") || orders.calls != 0 {
		t.Fatalf("another seller's product must not be orderable through this store: %v", err)
	}
}

func TestUnknownVariantIsRefused(t *testing.T) {
	svc := newVarService(&invOrders{}, tee("p1"))
	for _, id := range []string{"nope", " ", "P1-S-RED", "p1-s-red "} {
		_, err := svc.Create(context.Background(), "sellerA", varOrder(OrderLineInput{ProductID: "p1", VariantID: id, Qty: 1}))
		if !strings.Contains(fieldMsg(err, "lines"), "no longer available") {
			t.Fatalf("variant %q should be refused, got %v", id, err)
		}
	}
}

func TestVariantSentForAProductWithoutVariantsIsRefused(t *testing.T) {
	plain := models.Product{ID: "plain", SellerID: "sellerA", Name: "Mug", Price: 50, Stock: 10, IsActive: true}
	orders := &invOrders{}
	svc := newVarService(orders, plain)

	if _, err := svc.Create(context.Background(), "sellerA", varOrder(OrderLineInput{ProductID: "plain", VariantID: "x", Qty: 1})); err == nil {
		t.Fatal("a variant ID on a product that has none must be refused")
	}
	o, err := svc.Create(context.Background(), "sellerA", varOrder(OrderLineInput{ProductID: "plain", Qty: 2}))
	if err != nil || o.Lines[0].Price != 50 || o.Lines[0].VariantID != "" {
		t.Fatalf("a plain product still sells at its own price: %v %+v", err, o)
	}
}

func TestVariantStockIsPerVariant(t *testing.T) {
	svc := newVarService(&invOrders{}, tee("p1")) // the product total is 100
	cases := []struct {
		variant string
		qty     int
		want    string
	}{
		{"p1-m-red", 2, "only has 1 left"}, // stock 1
		{"p1-s-blue", 1, "out of stock"},   // stock 0
	}
	for _, c := range cases {
		_, err := svc.Create(context.Background(), "sellerA", varOrder(OrderLineInput{ProductID: "p1", VariantID: c.variant, Qty: c.qty}))
		if got := fieldMsg(err, "lines"); !strings.Contains(got, c.want) {
			t.Fatalf("%s x%d: want %q, got %q", c.variant, c.qty, c.want, got)
		}
	}
	if _, err := svc.Create(context.Background(), "sellerA", varOrder(OrderLineInput{ProductID: "p1", VariantID: "p1-m-red", Qty: 1})); err != nil {
		t.Fatalf("the last unit of a variant should be sellable: %v", err)
	}
}

func TestDamagedVariantsAreRefusedNotGuessed(t *testing.T) {
	cases := map[string]func(p *models.Product){
		"value no longer exists":  func(p *models.Product) { p.Combinations[0].ValueIDs["color"] = "deleted" },
		"missing an option":       func(p *models.Product) { delete(p.Combinations[0].ValueIDs, "color") },
		"names an unknown option": func(p *models.Product) { p.Combinations[0].ValueIDs["fabric"] = "silk" },
		"product lost its options": func(p *models.Product) {
			p.Options = nil
		},
		"duplicate variant id": func(p *models.Product) { p.Combinations[1].ID = p.Combinations[0].ID },
	}
	for name, damage := range cases {
		t.Run(name, func(t *testing.T) {
			p := tee("p1")
			damage(&p)
			orders := &invOrders{}
			svc := newVarService(orders, p)
			_, err := svc.Create(context.Background(), "sellerA", varOrder(OrderLineInput{ProductID: "p1", VariantID: "p1-s-red", Qty: 1}))
			if !isCode(err, apperr.CodeValidationFailed) || orders.calls != 0 {
				t.Fatalf("a damaged variant must not be sold: err=%v calls=%d", err, orders.calls)
			}
		})
	}
}

func TestInactiveProductWithVariantsIsNotSold(t *testing.T) {
	p := tee("p1")
	p.IsActive = false
	orders := &invOrders{}
	_, err := newVarService(orders, p).Create(context.Background(), "sellerA", varOrder(OrderLineInput{ProductID: "p1", VariantID: "p1-s-red", Qty: 1}))
	if err == nil || orders.calls != 0 {
		t.Fatalf("an inactive product must not be sold: %v", err)
	}
}

/* ---- what sellers may save ---- */

func validVariantInput() ProductInput {
	t := tee("x")
	in := ProductInput{Price: 100}
	for _, o := range t.Options {
		oi := OptionInput{ID: o.ID, Name: o.Name}
		for _, v := range o.Values {
			oi.Values = append(oi.Values, OptionValueInput{ID: v.ID, Label: v.Label})
		}
		in.Options = append(in.Options, oi)
	}
	for _, c := range t.Combinations {
		in.Combinations = append(in.Combinations, CombinationInput{ID: c.ID, ValueIDs: c.ValueIDs, Stock: c.Stock, Price: c.Price})
	}
	return in
}

func TestValidateCombinationsAcceptsAWellFormedProduct(t *testing.T) {
	if msg := validateCombinations(validVariantInput()); msg != "" {
		t.Fatalf("well-formed variants rejected: %s", msg)
	}
	if msg := validateCombinations(ProductInput{Price: 10}); msg != "" {
		t.Fatalf("a product without variants needs nothing: %s", msg)
	}
}

func TestValidateCombinationsRejectsBrokenVariants(t *testing.T) {
	cases := map[string]func(in *ProductInput){
		"no options at all":      func(in *ProductInput) { in.Options = nil },
		"duplicate variant id":   func(in *ProductInput) { in.Combinations[1].ID = in.Combinations[0].ID },
		"missing an option":      func(in *ProductInput) { delete(in.Combinations[0].ValueIDs, "color") },
		"unknown value":          func(in *ProductInput) { in.Combinations[0].ValueIDs["color"] = "ghost" },
		"unknown option":         func(in *ProductInput) { in.Combinations[0].ValueIDs["fabric"] = "silk" },
		"same options twice":     func(in *ProductInput) { in.Combinations[1].ValueIDs = in.Combinations[0].ValueIDs },
		"negative price":         func(in *ProductInput) { in.Combinations[0].Price = -1 },
		"absurd price":           func(in *ProductInput) { in.Combinations[0].Price = 1e12 },
		"NaN price":              func(in *ProductInput) { in.Combinations[0].Price = math.NaN() },
		"option without an id":   func(in *ProductInput) { in.Options[0].ID = "" },
		"duplicate option id":    func(in *ProductInput) { in.Options[1].ID = in.Options[0].ID },
		"duplicate value id":     func(in *ProductInput) { in.Options[0].Values[1].ID = in.Options[0].Values[0].ID },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			in := validVariantInput()
			// Fresh maps, so one case cannot leak into the next.
			for i := range in.Combinations {
				cp := map[string]string{}
				for k, v := range in.Combinations[i].ValueIDs {
					cp[k] = v
				}
				in.Combinations[i].ValueIDs = cp
			}
			mutate(&in)
			if validateCombinations(in) == "" {
				t.Fatal("accepted")
			}
		})
	}
}

func TestBuildCombinationsStoresAnExplicitPrice(t *testing.T) {
	in := []CombinationInput{
		{ID: "a", Price: 0},   // blank: takes the product's price now
		{ID: "b", Price: 250}, // its own price stays
	}
	got := buildCombinations(in, 100)
	if got[0].Price != 100 || got[1].Price != 250 {
		t.Fatalf("want 100 and 250, got %v and %v", got[0].Price, got[1].Price)
	}
	for _, c := range got {
		if !(c.Price > 0) {
			t.Fatal("every stored variant must have a price")
		}
	}
}

// A variant saved blank is therefore sellable straight away, at the price the
// product had when it was saved.
func TestBlankPriceVariantBecomesSellableAtTheSavedPrice(t *testing.T) {
	in := validVariantInput() // variant IDs are "x-..."
	in.Combinations[0].Price = 0
	p := tee("x")
	p.Combinations = buildCombinations(in.Combinations, 100)

	sel, problem := resolveVariant(&p, "x-s-red")
	if problem != "" || sel.Price != 100 {
		t.Fatalf("want a sellable variant at 100, got %+v %q", sel, problem)
	}
}
