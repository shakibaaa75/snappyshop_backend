package service

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/repository"
)

/* ---------------- fakes ---------------- */

type fakeLooks struct {
	mu   sync.Mutex
	rows map[string]*models.Look
}

func newFakeLooks() *fakeLooks { return &fakeLooks{rows: map[string]*models.Look{}} }

func (f *fakeLooks) get(seller, id string) (*models.Look, error) {
	l, ok := f.rows[id]
	if !ok || l.SellerID != seller {
		return nil, repository.ErrNotFound
	}
	return l, nil
}
func clone(l *models.Look) *models.Look {
	c := *l
	c.Items = append([]models.LookItem{}, l.Items...)
	return &c
}
func (f *fakeLooks) Create(_ context.Context, l *models.Look) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[l.ID] = clone(l)
	return nil
}
func (f *fakeLooks) FindByID(_ context.Context, s, id string) (*models.Look, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	l, err := f.get(s, id)
	if err != nil {
		return nil, err
	}
	return clone(l), nil
}
func (f *fakeLooks) FindPublishedByID(_ context.Context, id string) (*models.Look, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	l, ok := f.rows[id]
	if !ok || l.Status != models.LookPublished {
		return nil, repository.ErrNotFound
	}
	return clone(l), nil
}
func (f *fakeLooks) ListBySeller(_ context.Context, s string) ([]*models.Look, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*models.Look
	for _, l := range f.rows {
		if l.SellerID == s {
			out = append(out, clone(l))
		}
	}
	return out, nil
}
func (f *fakeLooks) CountBySeller(ctx context.Context, s string) (int, error) {
	l, _ := f.ListBySeller(ctx, s)
	return len(l), nil
}
func (f *fakeLooks) UpdateMeta(_ context.Context, s, id, title, gender, img string, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	l, err := f.get(s, id)
	if err != nil {
		return err
	}
	l.Title, l.Gender, l.ModelImageURL, l.UpdatedAt = title, gender, img, at
	return nil
}
func (f *fakeLooks) SetStatus(_ context.Context, s, id, st string, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	l, err := f.get(s, id)
	if err != nil {
		return err
	}
	l.Status, l.UpdatedAt = st, at
	return nil
}
func (f *fakeLooks) Delete(_ context.Context, s, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, err := f.get(s, id); err != nil {
		return err
	}
	delete(f.rows, id)
	return nil
}
func (f *fakeLooks) AddItem(_ context.Context, s, id, pid string, max int, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	l, err := f.get(s, id)
	if err != nil {
		return err
	}
	if l.HasProduct(pid) {
		return repository.ErrLookItemExists
	}
	if len(l.Items) >= max {
		return repository.ErrLookFull
	}
	l.Items = append(l.Items, models.LookItem{ProductID: pid})
	return nil
}
func (f *fakeLooks) RemoveItem(_ context.Context, s, id, pid string, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	l, err := f.get(s, id)
	if err != nil {
		return err
	}
	if !l.HasProduct(pid) {
		return repository.ErrLookItemMissing
	}
	var keep []models.LookItem
	for _, it := range l.Items {
		if it.ProductID != pid {
			keep = append(keep, it)
		}
	}
	l.Items = keep
	return nil
}
func (f *fakeLooks) ReplaceItem(_ context.Context, s, id, old, nw string, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	l, err := f.get(s, id)
	if err != nil {
		return err
	}
	if !l.HasProduct(old) {
		return repository.ErrLookItemMissing
	}
	if l.HasProduct(nw) {
		return repository.ErrLookItemExists
	}
	for i := range l.Items {
		if l.Items[i].ProductID == old {
			l.Items[i].ProductID = nw
		}
	}
	return nil
}
func (f *fakeLooks) SetOrder(_ context.Context, s, id string, ids []string, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	l, err := f.get(s, id)
	if err != nil {
		return err
	}
	if len(ids) != len(l.Items) {
		return repository.ErrLookItemsChanged
	}
	items := make([]models.LookItem, len(ids))
	for i, p := range ids {
		if !l.HasProduct(p) {
			return repository.ErrLookItemsChanged
		}
		items[i] = models.LookItem{ProductID: p}
	}
	l.Items = items
	return nil
}

type fakeCatalog struct {
	mu       sync.Mutex
	products map[string]*models.Product
	created  int
}

func newCatalog() *fakeCatalog { return &fakeCatalog{products: map[string]*models.Product{}} }
func (c *fakeCatalog) put(p *models.Product) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.products[p.ID] = p
}
func (c *fakeCatalog) FindByID(_ context.Context, seller, id string) (*models.Product, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if p, ok := c.products[id]; ok && p.SellerID == seller {
		cp := *p
		return &cp, nil
	}
	return nil, repository.ErrNotFound
}
func (c *fakeCatalog) FindByIDs(_ context.Context, seller string, ids []string) ([]*models.Product, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []*models.Product
	for _, id := range ids {
		if p, ok := c.products[id]; ok && p.SellerID == seller {
			cp := *p
			out = append(out, &cp)
		}
	}
	return out, nil
}
func (c *fakeCatalog) Create(_ context.Context, seller string, in ProductInput) (*models.Product, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.created++
	p := &models.Product{
		ID: fmt.Sprintf("new%d", c.created), SellerID: seller, Name: in.Name, Slug: "s",
		Price: in.Price, Stock: in.Stock, IsActive: in.IsActive,
	}
	c.products[p.ID] = p
	return p, nil
}

type fakeGate struct{ on map[string]bool }

func (g *fakeGate) HasAccess(_ context.Context, s, _ string) (bool, error) { return g.on[s], nil }
func (g *fakeGate) Require(_ context.Context, s, _ string) error {
	if !g.on[s] {
		return featureLocked("Try the Look")
	}
	return nil
}

type lookEnv struct {
	svc  *LookService
	cat  *fakeCatalog
	gate *fakeGate
}

func newLookEnv() *lookEnv {
	cat := newCatalog()
	gate := &fakeGate{on: map[string]bool{"s1": true}}
	svc := NewLookService(newFakeLooks(), cat, cat, gate)
	svc.SetPublic(
		func(context.Context, string) (models.Status, error) { return models.StatusActive, nil },
		func(context.Context, string) string { return "my-shop" },
	)
	return &lookEnv{svc: svc, cat: cat, gate: gate}
}

func (e *lookEnv) product(id, seller string, active bool, stock int) {
	e.cat.put(&models.Product{ID: id, SellerID: seller, Name: "P " + id, Slug: id, Price: 100, Stock: stock, IsActive: active})
}

func (e *lookEnv) newLook(t *testing.T) string {
	t.Helper()
	v, err := e.svc.Create(context.Background(), "s1", LookInput{
		Title: "Summer", Gender: "female", ModelImageURL: "https://img.example/m.jpg"})
	if err != nil {
		t.Fatal(err)
	}
	return v.ID
}

var bg = context.Background()

/* ---------------- tests ---------------- */

func TestLookCreateNeedsAccessAndValidInput(t *testing.T) {
	e := newLookEnv()
	in := LookInput{Title: "A", ModelImageURL: "https://x/y.jpg"}
	if _, err := e.svc.Create(bg, "nobody", in); code(err) != "FEATURE_LOCKED" {
		t.Fatalf("want FEATURE_LOCKED, got %v", err)
	}
	for name, bad := range map[string]LookInput{
		"no title":    {ModelImageURL: "https://x/y.jpg"},
		"no image":    {Title: "A"},
		"data uri":    {Title: "A", ModelImageURL: "data:image/png;base64,AAAA"},
		"js scheme":   {Title: "A", ModelImageURL: "javascript:alert(1)"},
		"bad gender":  {Title: "A", Gender: "robot", ModelImageURL: "https://x/y.jpg"},
		"long title":  {Title: strings.Repeat("x", 81), ModelImageURL: "https://x/y.jpg"},
	} {
		if _, err := e.svc.Create(bg, "s1", bad); code(err) != apperr.CodeValidationFailed {
			t.Fatalf("%s: want VALIDATION_FAILED, got %v", name, err)
		}
	}
	if _, err := e.svc.Create(bg, "s1", in); err != nil {
		t.Fatal(err)
	}
}

func TestAddExistingReferencesWithoutDuplicating(t *testing.T) {
	e := newLookEnv()
	e.product("p1", "s1", true, 5)
	id := e.newLook(t)

	v, err := e.svc.AddExisting(bg, "s1", id, "p1")
	if err != nil || len(v.Items) != 1 || v.Items[0].ProductID != "p1" {
		t.Fatalf("add failed: %+v %v", v, err)
	}
	if e.cat.created != 0 || len(e.cat.products) != 1 {
		t.Fatal("attaching an existing product must not create a product")
	}
	if _, err := e.svc.AddExisting(bg, "s1", id, "p1"); code(err) != "LOOK_ITEM_EXISTS" {
		t.Fatalf("want LOOK_ITEM_EXISTS, got %v", err)
	}
}

func TestAddExistingRejectsUnpublishedAndForeignProducts(t *testing.T) {
	e := newLookEnv()
	e.product("off", "s1", false, 5)
	e.product("theirs", "s2", true, 5)
	id := e.newLook(t)
	for _, pid := range []string{"off", "theirs", "ghost", ""} {
		if _, err := e.svc.AddExisting(bg, "s1", id, pid); code(err) != apperr.CodeValidationFailed {
			t.Fatalf("%q: want VALIDATION_FAILED, got %v", pid, err)
		}
	}
}

func TestLookIsCappedAtThirtyProducts(t *testing.T) {
	e := newLookEnv()
	id := e.newLook(t)
	for i := 0; i < models.MaxLookItems; i++ {
		pid := fmt.Sprintf("p%d", i)
		e.product(pid, "s1", true, 1)
		if _, err := e.svc.AddExisting(bg, "s1", id, pid); err != nil {
			t.Fatalf("product %d: %v", i, err)
		}
	}
	e.product("extra", "s1", true, 1)
	if _, err := e.svc.AddExisting(bg, "s1", id, "extra"); code(err) != "LOOK_FULL" {
		t.Fatalf("31st product: want LOOK_FULL, got %v", err)
	}
	// A full look must not create a stray product either.
	before := e.cat.created
	if _, err := e.svc.CreateAndAttach(bg, "s1", id, ProductInput{Name: "N", Price: 5, Stock: 1}); code(err) != "LOOK_FULL" {
		t.Fatalf("want LOOK_FULL, got %v", err)
	}
	if e.cat.created != before {
		t.Fatal("a full look must not create a product")
	}
}

func TestCreateAndAttachMakesPublishedProductAndLinksIt(t *testing.T) {
	e := newLookEnv()
	id := e.newLook(t)
	// The seller (or a tampered request) asks for an unpublished product:
	// look products are always published.
	v, err := e.svc.CreateAndAttach(bg, "s1", id, ProductInput{Name: "Tee", Price: 850, Stock: 3, IsActive: false})
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Items) != 1 || v.Items[0].Name != "Tee" || v.Items[0].Status != LookItemAvailable {
		t.Fatalf("unexpected view: %+v", v.Items)
	}
	if p := e.cat.products[v.Items[0].ProductID]; p == nil || !p.IsActive || p.SellerID != "s1" {
		t.Fatalf("product must exist, be published and belong to the seller: %+v", p)
	}
}

func TestOtherSellerCannotTouchMyLook(t *testing.T) {
	e := newLookEnv()
	e.gate.on["s2"] = true
	e.product("p1", "s1", true, 1)
	e.product("q1", "s2", true, 1)
	id := e.newLook(t)
	if _, err := e.svc.Get(bg, "s2", id); code(err) != apperr.CodeNotFound {
		t.Fatalf("get: want NOT_FOUND, got %v", err)
	}
	if _, err := e.svc.AddExisting(bg, "s2", id, "q1"); code(err) != apperr.CodeNotFound {
		t.Fatalf("add: want NOT_FOUND, got %v", err)
	}
	if err := e.svc.Delete(bg, "s2", id); code(err) != apperr.CodeNotFound {
		t.Fatalf("delete: want NOT_FOUND, got %v", err)
	}
	if _, err := e.svc.Publish(bg, "s2", id); code(err) != apperr.CodeNotFound {
		t.Fatalf("publish: want NOT_FOUND, got %v", err)
	}
}

func TestRemoveReplaceReorder(t *testing.T) {
	e := newLookEnv()
	for _, p := range []string{"a", "b", "c", "d"} {
		e.product(p, "s1", true, 1)
	}
	id := e.newLook(t)
	for _, p := range []string{"a", "b", "c"} {
		if _, err := e.svc.AddExisting(bg, "s1", id, p); err != nil {
			t.Fatal(err)
		}
	}
	ids := func(v *LookView) string {
		s := ""
		for _, it := range v.Items {
			s += it.ProductID
		}
		return s
	}

	v, err := e.svc.Reorder(bg, "s1", id, []string{"c", "a", "b"})
	if err != nil || ids(v) != "cab" {
		t.Fatalf("reorder: %v %v", ids(v), err)
	}
	for name, bad := range map[string][]string{
		"missing one": {"c", "a"},
		"duplicate":   {"c", "c", "a"},
		"foreign id":  {"c", "a", "zzz"},
	} {
		if _, err := e.svc.Reorder(bg, "s1", id, bad); err == nil {
			t.Fatalf("%s: reorder must be rejected", name)
		}
	}
	v, err = e.svc.ReplaceItem(bg, "s1", id, "a", "d")
	if err != nil || ids(v) != "cdb" {
		t.Fatalf("replace keeps position: %v %v", ids(v), err)
	}
	if _, err := e.svc.ReplaceItem(bg, "s1", id, "d", "b"); code(err) != "LOOK_ITEM_EXISTS" {
		t.Fatalf("replace with a product already there: got %v", err)
	}
	if _, err := e.svc.ReplaceItem(bg, "s1", id, "zzz", "a"); code(err) != "LOOK_ITEM_NOT_FOUND" {
		t.Fatalf("replace missing: got %v", err)
	}
	v, err = e.svc.RemoveItem(bg, "s1", id, "d")
	if err != nil || ids(v) != "cb" {
		t.Fatalf("remove: %v %v", ids(v), err)
	}
	if _, ok := e.cat.products["d"]; !ok {
		t.Fatal("removing from a look must never delete the product")
	}
}

func TestPublishNeedsAProductAndPublicPageIsLive(t *testing.T) {
	e := newLookEnv()
	e.product("p1", "s1", true, 4)
	e.product("p2", "s1", true, 4)
	id := e.newLook(t)

	if _, err := e.svc.Publish(bg, "s1", id); code(err) != apperr.CodeValidationFailed {
		t.Fatalf("empty look must not publish, got %v", err)
	}
	if _, err := e.svc.Public(bg, id); code(err) != apperr.CodeNotFound {
		t.Fatalf("draft must be hidden, got %v", err)
	}
	_, _ = e.svc.AddExisting(bg, "s1", id, "p1")
	_, _ = e.svc.AddExisting(bg, "s1", id, "p2")
	if _, err := e.svc.Publish(bg, "s1", id); err != nil {
		t.Fatal(err)
	}

	pub, err := e.svc.Public(bg, id)
	if err != nil || len(pub.Items) != 2 || pub.StoreSlug != "my-shop" {
		t.Fatalf("public view: %+v %v", pub, err)
	}

	// Live data: price change, out of stock, unpublished and deleted products.
	e.cat.products["p1"].Price = 777
	e.cat.products["p2"].Stock = 0
	pub, _ = e.svc.Public(bg, id)
	if pub.Items[0].Price != 777 {
		t.Fatalf("price must follow the product, got %v", pub.Items[0].Price)
	}
	if pub.Items[1].Status != LookItemOutOfStock || pub.Items[1].InStock {
		t.Fatalf("out of stock must be shown with its status: %+v", pub.Items[1])
	}
	e.cat.products["p1"].IsActive = false
	pub, _ = e.svc.Public(bg, id)
	if len(pub.Items) != 1 || pub.Items[0].ProductID != "p2" {
		t.Fatalf("an unpublished product must disappear for buyers: %+v", pub.Items)
	}
	delete(e.cat.products, "p2")
	e.cat.products["p1"].IsActive = false
	if _, err := e.svc.Public(bg, id); code(err) != apperr.CodeNotFound {
		t.Fatalf("nothing showable: want NOT_FOUND, got %v", err)
	}

	// The seller still sees what needs attention.
	v, _ := e.svc.Get(bg, "s1", id)
	if v.NeedsAttention != 2 {
		t.Fatalf("needsAttention = %d, want 2", v.NeedsAttention)
	}
}

func TestPublicHiddenWhenAccessRevokedOrStoreHidden(t *testing.T) {
	e := newLookEnv()
	e.product("p1", "s1", true, 1)
	id := e.newLook(t)
	_, _ = e.svc.AddExisting(bg, "s1", id, "p1")
	if _, err := e.svc.Publish(bg, "s1", id); err != nil {
		t.Fatal(err)
	}

	e.gate.on["s1"] = false
	if _, err := e.svc.Public(bg, id); code(err) != apperr.CodeNotFound {
		t.Fatalf("revoked access must hide the look, got %v", err)
	}
	// ...but the seller can still read it, take it down and delete it.
	if _, err := e.svc.Get(bg, "s1", id); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Unpublish(bg, "s1", id); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.AddExisting(bg, "s1", id, "p1"); code(err) != "FEATURE_LOCKED" {
		t.Fatalf("editing needs access, got %v", err)
	}
	e.gate.on["s1"] = true

	_, _ = e.svc.Publish(bg, "s1", id)
	e.svc.SetPublic(
		func(context.Context, string) (models.Status, error) { return models.StatusSuspended, nil },
		func(context.Context, string) string { return "my-shop" },
	)
	if _, err := e.svc.Public(bg, id); code(err) != apperr.CodeNotFound {
		t.Fatalf("suspended seller must hide the look, got %v", err)
	}
}

func TestDeleteLookKeepsProducts(t *testing.T) {
	e := newLookEnv()
	e.product("p1", "s1", true, 1)
	id := e.newLook(t)
	_, _ = e.svc.AddExisting(bg, "s1", id, "p1")
	if err := e.svc.Delete(bg, "s1", id); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.cat.products["p1"]; !ok {
		t.Fatal("deleting a look must not delete its products")
	}
}
