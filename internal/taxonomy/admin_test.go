package taxonomy

import (
	"context"
	"errors"
	"testing"
)

type fakeProduct struct{ main, sub, name string }

type fakeProducts struct{ items []*fakeProduct }

func (f *fakeProducts) CountByCategory(context.Context) (map[string]int, error) {
	m := map[string]int{}
	for _, p := range f.items {
		m[p.sub]++
	}
	return m, nil
}
func (f *fakeProducts) CountForCategory(_ context.Context, id string) (int64, error) {
	var n int64
	for _, p := range f.items {
		if p.sub == id {
			n++
		}
	}
	return n, nil
}
func (f *fakeProducts) Reassign(_ context.Context, from string, to Node) (int64, error) {
	var n int64
	for _, p := range f.items {
		if p.sub == from {
			p.main, p.sub, p.name = to.ParentID, to.ID, to.Name
			n++
		}
	}
	return n, nil
}
func (f *fakeProducts) SetName(_ context.Context, id, name string) error {
	for _, p := range f.items {
		if p.sub == id {
			p.name = name
		}
	}
	return nil
}
func (f *fakeProducts) SetMain(_ context.Context, id, main string) error {
	for _, p := range f.items {
		if p.sub == id {
			p.main = main
		}
	}
	return nil
}

func newAdmin(t *testing.T) (*Admin, *fakeStore, *fakeProducts) {
	t.Helper()
	st := newFake()
	c, err := NewCatalog(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	fp := &fakeProducts{}
	return &Admin{Cat: c, Products: fp}, st, fp
}

func TestAdminRenameKeepsProductsAndOldSpelling(t *testing.T) {
	ctx := context.Background()
	a, _, fp := newAdmin(t)
	// A seller creates a misspelled category and lists a product in it.
	r, _ := a.Cat.Resolve(ctx, Input{MainID: "fashion", CustomCategory: "Jaket Pro"}, Actor{SellerID: "s1"})
	fp.items = append(fp.items, &fakeProduct{"fashion", r.CategoryID, r.CategoryName})

	n, err := a.Rename(ctx, r.CategoryID, "Jacket Pro")
	if err != nil || n.Name != "Jacket Pro" {
		t.Fatalf("rename: %+v %v", n, err)
	}
	if fp.items[0].name != "Jacket Pro" {
		t.Errorf("product name not updated: %q", fp.items[0].name)
	}
	// The misspelling still resolves to the same category: no re-creation.
	r2, _ := a.Cat.Resolve(ctx, Input{MainID: "fashion", CustomCategory: "Jaket Pro"}, Actor{})
	if r2.Created || r2.CategoryID != r.CategoryID || r2.CategoryName != "Jacket Pro" {
		t.Errorf("old spelling should map to renamed category: %+v", r2)
	}
	if _, err := a.Rename(ctx, "fashion.hoodie", "Shirt"); err == nil {
		t.Error("renaming onto an existing name must fail")
	} else {
		var taken *NameTakenError
		if !errors.As(err, &taken) || taken.Existing.ID != "fashion.shirt" {
			t.Errorf("want NameTakenError(fashion.shirt), got %v", err)
		}
	}
	if _, err := a.Rename(ctx, "fashion", "Clothes"); !errors.Is(err, ErrMainTypeLocked) {
		t.Errorf("main types must be locked, got %v", err)
	}
}

func TestAdminMergeMovesProductsAndRemovesDuplicate(t *testing.T) {
	ctx := context.Background()
	a, st, fp := newAdmin(t)
	r, _ := a.Cat.Resolve(ctx, Input{MainID: "fashion", CustomCategory: "Hoody Wear"}, Actor{})
	fp.items = append(fp.items,
		&fakeProduct{"fashion", r.CategoryID, r.CategoryName},
		&fakeProduct{"fashion", r.CategoryID, r.CategoryName})

	moved, err := a.Merge(ctx, r.CategoryID, "fashion.hoodie")
	if err != nil || moved != 2 {
		t.Fatalf("merge: moved=%d err=%v", moved, err)
	}
	for _, p := range fp.items {
		if p.sub != "fashion.hoodie" || p.name != "Hoodie" {
			t.Errorf("product not moved: %+v", p)
		}
	}
	if _, ok := st.nodes[r.CategoryID]; ok {
		t.Error("duplicate was not deleted")
	}
	if _, ok := a.Cat.Index.Node(r.CategoryID); ok {
		t.Error("duplicate still in live index")
	}
	// Typing the merged name again goes to the target.
	r2, _ := a.Cat.Resolve(ctx, Input{MainID: "fashion", CustomCategory: "Hoody Wear"}, Actor{})
	if r2.Created || r2.CategoryID != "fashion.hoodie" {
		t.Errorf("merged name should alias the target: %+v", r2)
	}
	if _, err := a.Merge(ctx, "fashion.shirt", "electronics.laptop"); !errors.Is(err, ErrDifferentMain) {
		t.Errorf("cross-main merge: %v", err)
	}
	if _, err := a.Merge(ctx, "fashion.shirt", "fashion.shirt"); !errors.Is(err, ErrSameCategory) {
		t.Errorf("self merge: %v", err)
	}
}

func TestAdminMove(t *testing.T) {
	ctx := context.Background()
	a, _, fp := newAdmin(t)
	r, _ := a.Cat.Resolve(ctx, Input{MainID: "fashion", CustomCategory: "Phone Stand"}, Actor{})
	fp.items = append(fp.items, &fakeProduct{"fashion", r.CategoryID, r.CategoryName})

	n, err := a.Move(ctx, r.CategoryID, "electronics")
	if err != nil || n.ParentID != "electronics" {
		t.Fatalf("move: %+v %v", n, err)
	}
	if fp.items[0].main != "electronics" || fp.items[0].sub != r.CategoryID {
		t.Errorf("product must follow: %+v", fp.items[0])
	}
	if len(a.Cat.Index.Children("electronics")) == 0 {
		t.Error("index not updated")
	}
	// Name clash in the target Main Type is refused.
	if _, err := a.Move(ctx, "fashion.watch", "electronics"); err == nil {
		// "Watch" vs electronics "Smart Watch" differ, so this one is allowed;
		// use a real clash instead:
		r3, _ := a.Cat.Resolve(ctx, Input{MainID: "fashion", CustomCategory: "Laptop"}, Actor{})
		var taken *NameTakenError
		if _, err := a.Move(ctx, r3.CategoryID, "electronics"); !errors.As(err, &taken) {
			t.Errorf("want NameTakenError, got %v", err)
		}
	}
	if _, err := a.Move(ctx, r.CategoryID, "nope"); !errors.Is(err, ErrUnknownMain) {
		t.Errorf("unknown main: %v", err)
	}
}

func TestAdminDeleteNeverLosesProducts(t *testing.T) {
	ctx := context.Background()
	a, st, fp := newAdmin(t)
	fp.items = append(fp.items, &fakeProduct{"fashion", "fashion.belt", "Belt"})

	_, err := a.Delete(ctx, "fashion.belt", "")
	var has *HasProductsError
	if !errors.As(err, &has) || has.Count != 1 {
		t.Fatalf("want HasProductsError, got %v", err)
	}
	if _, ok := st.nodes["fashion.belt"]; !ok {
		t.Fatal("category deleted despite having products")
	}
	moved, err := a.Delete(ctx, "fashion.belt", "fashion.wallet")
	if err != nil || moved != 1 || fp.items[0].sub != "fashion.wallet" {
		t.Fatalf("delete with moveTo: moved=%d err=%v item=%+v", moved, err, fp.items[0])
	}
	if _, err := a.Delete(ctx, "fashion.saree", ""); err != nil { // empty: deletes freely
		t.Errorf("empty category delete: %v", err)
	}
}

func TestAdminNewCategoriesAndSeen(t *testing.T) {
	ctx := context.Background()
	a, _, _ := newAdmin(t)
	a.Cat.Resolve(ctx, Input{MainID: "fashion", CustomCategory: "Pet Wear"}, Actor{SellerID: "s1", StoreNameFn: func(context.Context) string { return "Rahim Store" }})

	items, err := a.Unseen(ctx)
	if err != nil || len(items) != 1 || items[0].CreatedByStoreName != "Rahim Store" {
		t.Fatalf("unseen: %+v %v", items, err)
	}
	if _, unseen, _ := a.List(ctx); unseen != 1 {
		t.Errorf("badge count = %d, want 1", unseen)
	}
	mains, _, _ := a.List(ctx)
	if len(mains) != 10 {
		t.Errorf("want 10 main types, got %d", len(mains))
	}
	if err := a.MarkSeen(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if items, _ := a.Unseen(ctx); len(items) != 0 {
		t.Errorf("still unseen after MarkSeen: %d", len(items))
	}
}

func TestAdminSimilarFindsLookalikes(t *testing.T) {
	ctx := context.Background()
	a, _, _ := newAdmin(t)
	r, _ := a.Cat.Resolve(ctx, Input{MainID: "fashion", CustomCategory: "Polo Shirts Wear"}, Actor{})
	sim, err := a.Similar(ctx, r.CategoryID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range sim {
		if s.ID == "fashion.polo-shirt" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected Polo Shirt among suggestions: %+v", sim)
	}
}
