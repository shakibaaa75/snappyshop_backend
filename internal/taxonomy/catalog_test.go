package taxonomy

import (
	"context"
	"testing"
)

// fakeStore is an in-memory NodeStore.
type fakeStore struct{ nodes map[string]Node }

func newFake() *fakeStore { return &fakeStore{nodes: map[string]Node{}} }

func (f *fakeStore) ListNodes(context.Context) ([]Node, error) {
	out := make([]Node, 0, len(f.nodes))
	for _, n := range f.nodes {
		out = append(out, n)
	}
	return out, nil
}
func (f *fakeStore) InsertMissing(_ context.Context, ns []Node) error {
	for _, n := range ns {
		if _, ok := f.nodes[n.ID]; !ok {
			f.nodes[n.ID] = n
		}
	}
	return nil
}
func (f *fakeStore) InsertIfAbsent(_ context.Context, n Node) (bool, error) {
	if _, ok := f.nodes[n.ID]; ok {
		return false, nil
	}
	f.nodes[n.ID] = n
	return true, nil
}
func (f *fakeStore) UpdateNode(_ context.Context, n Node) error {
	if _, ok := f.nodes[n.ID]; !ok {
		return ErrNotFound
	}
	f.nodes[n.ID] = n
	return nil
}
func (f *fakeStore) DeleteNode(_ context.Context, id string) error { delete(f.nodes, id); return nil }
func (f *fakeStore) MarkSeen(_ context.Context, ids []string) error {
	for id, n := range f.nodes {
		if len(ids) == 0 || containsStr(ids, id) {
			n.AdminSeen = true
			f.nodes[id] = n
		}
	}
	return nil
}
func (f *fakeStore) CountNodes(context.Context) (int64, error) { return int64(len(f.nodes)), nil }

func TestSeedHasExactlyTenMainTypesAndNoOther(t *testing.T) {
	mains := 0
	for _, n := range Seed() {
		if n.ID == OtherID {
			t.Fatal("seed must not contain an Other main type")
		}
		if IsMain(n) {
			mains++
		}
	}
	if mains != 10 {
		t.Fatalf("want 10 main types, got %d", mains)
	}
}

func TestCatalogSeedsOnceAndKeepsAdminDeletions(t *testing.T) {
	ctx := context.Background()
	st := newFake()
	c, err := NewCatalog(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Index.Node("fashion.hoodie"); !ok {
		t.Fatal("starter category missing after first seed")
	}

	// Admin deletes a starter category; restart must not resurrect it.
	delete(st.nodes, "fashion.hoodie")
	c2, err := NewCatalog(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c2.Index.Node("fashion.hoodie"); ok {
		t.Fatal("deleted starter category came back")
	}
}

func TestCatalogAlwaysRestoresMainTypes(t *testing.T) {
	ctx := context.Background()
	st := newFake()
	if _, err := NewCatalog(ctx, st); err != nil {
		t.Fatal(err)
	}
	delete(st.nodes, "automotive")
	c, err := NewCatalog(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Index.Node("automotive"); !ok {
		t.Fatal("main type was not restored")
	}
}

func TestReloadPicksUpChangesInPlace(t *testing.T) {
	ctx := context.Background()
	st := newFake()
	c, _ := NewCatalog(ctx, st)
	shared := c.Index // the pointer other code holds

	st.nodes["fashion.pet-wear"] = Node{ID: "fashion.pet-wear", ParentID: "fashion", Name: "Pet Wear", IsActive: true}
	if err := c.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := shared.Node("fashion.pet-wear"); !ok {
		t.Fatal("shared index did not see the new category after Reload")
	}
	if m := shared.ResolveText("pet wear", IsSub); m.NodeID != "fashion.pet-wear" {
		t.Fatalf("new category not matchable, got %q", m.NodeID)
	}
}
