package taxonomy

import (
	"context"
	"errors"
	"testing"
)

func TestPlanUsesOfficialNameForTyposAndVariants(t *testing.T) {
	ix := NewIndex(Seed(), nil)
	cases := []struct{ main, typed, want string }{
		{"fashion", "T-shirt", "fashion.t-shirt"},
		{"fashion", "tshirts", "fashion.t-shirt"},
		{"fashion", "Hodie", "fashion.hoodie"},
		{"fashion", "cotton hoodie for men", "fashion.hoodie"},
		{"electronics", "Headfone", "electronics.headphones"}, // spec example
		{"electronics", "Moblie Phone", "electronics.smartphone"},
		{"electronics", "Smart wach", "electronics.smart-watch"},
	}
	for _, c := range cases {
		p, err := Plan(ix, Input{MainID: c.main, CustomCategory: c.typed})
		if err != nil {
			t.Fatalf("%q: %v", c.typed, err)
		}
		if p.Create || p.Result.CategoryID != c.want {
			t.Errorf("%q under %s: got create=%v id=%q, want %q", c.typed, c.main, p.Create, p.Result.CategoryID, c.want)
		}
		if n, _ := ix.Node(c.want); p.Result.CategoryName != n.Name {
			t.Errorf("%q: stored name %q, want official %q", c.typed, p.Result.CategoryName, n.Name)
		}
	}
}

func TestPlanCreatesWhenNoCloseMatch(t *testing.T) {
	ix := NewIndex(Seed(), nil)
	cases := []struct{ main, typed, name string }{
		{"fashion", "pet wear", "Pet Wear"},
		{"groceries", "Dog Food", "Dog Food"},
		{"fashion", "Watch Strap", "Watch Strap"}, // must not be forced into "Watch"
		{"electronics", "Hodie", "Hodie"},         // typo of a Fashion category stays in the chosen Main Type
	}
	for _, c := range cases {
		p, err := Plan(ix, Input{MainID: c.main, CustomCategory: c.typed})
		if err != nil || !p.Create || p.NewName != c.name || p.Result.MainID != c.main {
			t.Errorf("%q under %s: %+v err=%v", c.typed, c.main, p, err)
		}
	}
}

func TestPlanErrors(t *testing.T) {
	ix := NewIndex(Seed(), nil)
	cases := []struct {
		in   Input
		want error
	}{
		{Input{CustomCategory: "Shoes"}, ErrMainRequired},
		{Input{MainID: "other", CustomCategory: "Shoes"}, ErrUnknownMain},
		{Input{MainID: "fashion.hoodie", CustomCategory: "Shoes"}, ErrUnknownMain}, // a category is not a Main Type
		{Input{MainID: "fashion"}, ErrCategoryRequired},
		{Input{MainID: "electronics", CategoryID: "fashion.hoodie"}, ErrCategoryNotInMain},
		{Input{MainID: "fashion", CategoryID: "fashion.gone"}, ErrCategoryNotFound},
		{Input{MainID: "fashion", CustomCategory: "x"}, ErrInvalidName},
		{Input{MainID: "fashion", CustomCategory: "12345"}, ErrInvalidName},
		{Input{MainID: "fashion", CustomCategory: "!!!"}, ErrInvalidName},
	}
	for _, c := range cases {
		if _, err := Plan(ix, c.in); !errors.Is(err, c.want) {
			t.Errorf("%+v: got %v, want %v", c.in, err, c.want)
		}
	}
}

func TestPlanPickedCategoryAndLegacyClients(t *testing.T) {
	ix := NewIndex(Seed(), nil)
	p, err := Plan(ix, Input{MainID: "fashion", CategoryID: "fashion.hoodie"})
	if err != nil || p.Result.CategoryID != "fashion.hoodie" || p.Result.Method != MethodSelected {
		t.Errorf("picked: %+v %v", p, err)
	}
	// Old clients send only the category ID: the Main Type comes from it.
	p, err = Plan(ix, Input{CategoryID: "fashion.hoodie"})
	if err != nil || p.Result.MainID != "fashion" {
		t.Errorf("legacy: %+v %v", p, err)
	}
}

func TestTidyName(t *testing.T) {
	cases := map[string]string{
		"mens t-shirt":  "Mens T-Shirt",
		"  pet   wear ": "Pet Wear",
		"SHOES":         "Shoes",
		"iPhone Cases":  "iPhone Cases", // mixed case is left as typed
	}
	for in, want := range cases {
		if got := TidyName(in); got != want {
			t.Errorf("TidyName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResolveCreatesLiveCategoryAndNeverDuplicates(t *testing.T) {
	ctx := context.Background()
	st := newFake()
	c, _ := NewCatalog(ctx, st)
	lookups := 0
	who := Actor{
		SellerID: "seller-1", ProductID: "prod-1", Via: "seller_product_form",
		StoreNameFn: func(context.Context) string { lookups++; return "Rahim Store" },
	}

	r, err := c.Resolve(ctx, Input{MainID: "fashion", CustomCategory: "pet wear"}, who)
	if err != nil || !r.Created || r.CategoryID != "fashion.pet-wear" || r.CategoryName != "Pet Wear" {
		t.Fatalf("create: %+v %v", r, err)
	}
	n := st.nodes["fashion.pet-wear"]
	if !n.IsActive || !n.ShowOnHome || n.AdminSeen || n.Source != SourceSeller ||
		n.CreatedBySellerID != "seller-1" || n.CreatedByStoreName != "Rahim Store" ||
		n.CreatedVia != "seller_product_form" || n.CreatedFromProduct != "prod-1" ||
		n.RawInput != "pet wear" || n.CreatedAt.IsZero() {
		t.Errorf("provenance/notification fields wrong: %+v", n)
	}
	if _, ok := c.Index.Node("fashion.pet-wear"); !ok {
		t.Error("new category is not live in the shared index")
	}

	// Same name, and a typo of it, reuse the category: no duplicates.
	for _, typed := range []string{"Pet Wear", "petwear", "Pet Were"} {
		r, err := c.Resolve(ctx, Input{MainID: "fashion", CustomCategory: typed}, who)
		if err != nil || r.Created || r.CategoryID != "fashion.pet-wear" {
			t.Errorf("%q should reuse the category: %+v %v", typed, r, err)
		}
	}
	if lookups != 1 {
		t.Errorf("store name looked up %d times, want 1 (only on creation)", lookups)
	}

	// The same name under a different Main Type is a different category.
	r, err = c.Resolve(ctx, Input{MainID: "electronics", CustomCategory: "Pet Wear"}, who)
	if err != nil || !r.Created || r.CategoryID != "electronics.pet-wear" {
		t.Errorf("other main type: %+v %v", r, err)
	}
}

func TestResolveSurvivesIDClash(t *testing.T) {
	ctx := context.Background()
	st := newFake()
	c, _ := NewCatalog(ctx, st)
	// An inactive node already owns the ID, so it is invisible to the index.
	st.nodes["fashion.pet-wear"] = Node{ID: "fashion.pet-wear", ParentID: "fashion", Name: "Old", IsActive: false}

	r, err := c.Resolve(ctx, Input{MainID: "fashion", CustomCategory: "Pet Wear"}, Actor{})
	if err != nil || !r.Created || r.CategoryID != "fashion.pet-wear-2" {
		t.Fatalf("clash: %+v %v", r, err)
	}
}
