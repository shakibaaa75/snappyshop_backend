package service

import (
	"context"
	"sort"
	"testing"

	"shopbd/backend/internal/models"
)

// ListPublished lets the shared fake serve the public list.
func (f *fakeLooks) ListPublished(_ context.Context, seller, gender string, limit int) ([]*models.Look, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*models.Look
	for _, l := range f.rows {
		if l.Status != models.LookPublished || (seller != "" && l.SellerID != seller) || (gender != "" && l.Gender != gender) {
			continue
		}
		out = append(out, clone(l))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func publishedLook(t *testing.T, e *lookEnv, pids ...string) string {
	t.Helper()
	id := e.newLook(t)
	for _, p := range pids {
		if _, err := e.svc.AddExisting(bg, "s1", id, p); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.svc.Publish(bg, "s1", id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestPublicListShowsOnlyShowableLooks(t *testing.T) {
	e := newLookEnv()
	e.svc.SetSlugResolver(func(_ context.Context, slug string) (string, error) {
		if slug == "my-shop" {
			return "s1", nil
		}
		return "", nil
	})
	e.product("p1", "s1", true, 3)
	e.product("p2", "s1", true, 0)
	good := publishedLook(t, e, "p1", "p2")
	_ = e.newLook(t) // a draft must never be listed

	rows, err := e.svc.PublicList(bg, "", "", 0)
	if err != nil || len(rows) != 1 || rows[0].ID != good || rows[0].ItemCount != 2 || rows[0].StoreSlug != "my-shop" {
		t.Fatalf("list: %+v %v", rows, err)
	}
	if rows, _ := e.svc.PublicList(bg, "my-shop", "", 10); len(rows) != 1 {
		t.Fatalf("store filter: %+v", rows)
	}
	if rows, _ := e.svc.PublicList(bg, "nobody", "", 10); len(rows) != 0 {
		t.Fatalf("unknown store must be empty: %+v", rows)
	}
	if rows, _ := e.svc.PublicList(bg, "", "male", 10); len(rows) != 0 {
		t.Fatalf("gender filter: %+v", rows)
	}

	// Nothing showable left: the look drops out.
	e.cat.products["p1"].IsActive = false
	e.cat.products["p2"].IsActive = false
	if rows, _ := e.svc.PublicList(bg, "", "", 10); len(rows) != 0 {
		t.Fatalf("look with nothing showable must be hidden: %+v", rows)
	}
}

func TestPublicListHiddenWhenAccessRevoked(t *testing.T) {
	e := newLookEnv()
	e.product("p1", "s1", true, 3)
	publishedLook(t, e, "p1")
	e.gate.on["s1"] = false
	if rows, _ := e.svc.PublicList(bg, "", "", 10); len(rows) != 0 {
		t.Fatalf("revoked seller's looks must be hidden: %+v", rows)
	}
}
