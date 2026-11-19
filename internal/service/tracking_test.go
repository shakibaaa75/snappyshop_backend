package service

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"

	"shopbd/backend/internal/models"
	"shopbd/backend/internal/repository"
)

/* ---------------- in-memory fakes ---------------- */

type fakeEvents struct {
	mu   sync.Mutex
	rows []*models.AnalyticsEvent
	seen map[string]bool
}

func newFakeEvents() *fakeEvents { return &fakeEvents{seen: map[string]bool{}} }

func (f *fakeEvents) Insert(_ context.Context, e *models.AnalyticsEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if e.Dedupe != "" {
		if f.seen[e.Dedupe] {
			return nil
		}
		f.seen[e.Dedupe] = true
	}
	cp := *e
	f.rows = append(f.rows, &cp)
	return nil
}

func (f *fakeEvents) match(sellerID, kind string, from, to time.Time, fn func(*models.AnalyticsEvent)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range f.rows {
		if e.SellerID != sellerID || (kind != "" && e.Kind != kind) || e.CreatedAt.Before(from) {
			continue
		}
		if !to.IsZero() && !e.CreatedAt.Before(to) {
			continue
		}
		fn(e)
	}
}

func (f *fakeEvents) CountByKind(_ context.Context, sellerID, kind string, since time.Time) (int, error) {
	n := 0
	f.match(sellerID, kind, since, time.Time{}, func(*models.AnalyticsEvent) { n++ })
	return n, nil
}

func (f *fakeEvents) ProductCounts(_ context.Context, sellerID, kind string, since time.Time, limit int) ([]repository.ProductCount, error) {
	m := map[string]int{}
	f.match(sellerID, kind, since, time.Time{}, func(e *models.AnalyticsEvent) { m[e.ProductID]++ })
	var out []repository.ProductCount
	for id, n := range m {
		out = append(out, repository.ProductCount{ProductID: id, Count: n})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Count > out[j].Count })
	return out, nil
}

func (f *fakeEvents) TermCounts(context.Context, string, time.Time, int) ([]repository.TermCount, error) {
	return nil, nil
}

func (f *fakeEvents) SourceCounts(_ context.Context, sellerID string, since time.Time, _ int) ([]repository.SourceCount, error) {
	m := map[string]int{}
	f.match(sellerID, models.AnalyticsEventTraffic, since, time.Time{}, func(e *models.AnalyticsEvent) { m[e.Source]++ })
	var out []repository.SourceCount
	for s, n := range m {
		out = append(out, repository.SourceCount{Source: s, Count: n})
	}
	return out, nil
}

func (f *fakeEvents) ProductCountsInRange(_ context.Context, sellerID, kind string, from, to time.Time) (map[string]int, error) {
	m := map[string]int{}
	f.match(sellerID, kind, from, to, func(e *models.AnalyticsEvent) { m[e.ProductID]++ })
	return m, nil
}

func (f *fakeEvents) DailyCounts(_ context.Context, sellerID, kind string, from, to time.Time) (map[string]int, error) {
	m := map[string]int{}
	f.match(sellerID, kind, from, to, func(e *models.AnalyticsEvent) { m[e.CreatedAt.Format("2006-01-02")]++ })
	return m, nil
}

func (f *fakeEvents) SourceBreakdown(_ context.Context, sellerID string, since time.Time) ([]repository.SourceKindStat, error) {
	type key struct{ s, k string }
	type agg struct {
		count int
		value float64
		vis   map[string]bool
	}
	m := map[key]*agg{}
	f.match(sellerID, "", since, time.Time{}, func(e *models.AnalyticsEvent) {
		if e.Source == "" || e.Kind == models.AnalyticsEventSearch {
			return
		}
		a := m[key{e.Source, e.Kind}]
		if a == nil {
			a = &agg{vis: map[string]bool{}}
			m[key{e.Source, e.Kind}] = a
		}
		a.count++
		a.value += e.Value
		if e.Visitor != "" {
			a.vis[e.Visitor] = true
		}
	})
	var out []repository.SourceKindStat
	for k, a := range m {
		out = append(out, repository.SourceKindStat{Source: k.s, Kind: k.k, Count: a.count, Visitors: len(a.vis), Value: a.value})
	}
	return out, nil
}

func (f *fakeEvents) UniqueVisitors(_ context.Context, sellerID string, since time.Time) (int, error) {
	v := map[string]bool{}
	f.match(sellerID, models.AnalyticsEventTraffic, since, time.Time{}, func(e *models.AnalyticsEvent) {
		if e.Visitor != "" {
			v[e.Visitor] = true
		}
	})
	return len(v), nil
}

type fakeProducts struct{ repository.ProductRepository }

func (fakeProducts) FindByID(_ context.Context, sellerID, id string) (*models.Product, error) {
	return &models.Product{ID: id, Name: "P-" + id}, nil
}
func (fakeProducts) List(context.Context, string, int) ([]*models.Product, error) { return nil, nil }

type analyticsOrders struct{ repository.OrderRepository }

func (analyticsOrders) List(context.Context, string, repository.OrderFilter) ([]*models.Order, error) {
	return nil, nil
}

type fakeDesigns struct {
	repository.StoreDesignRepository
	bySlug map[string]string // slug -> sellerID (published stores only)
}

func (f fakeDesigns) GetPublishedBySlug(_ context.Context, slug string) (*models.StoreDesign, error) {
	if id, ok := f.bySlug[slug]; ok {
		return &models.StoreDesign{SellerID: id, Slug: slug, IsPublished: true}, nil
	}
	return nil, repository.ErrNotFound
}

type fakeSettings struct {
	m map[string]*models.TrackingSettings
}

func (f *fakeSettings) Get(_ context.Context, id string) (*models.TrackingSettings, error) {
	if s, ok := f.m[id]; ok {
		cp := *s
		return &cp, nil
	}
	return nil, repository.ErrNotFound
}
func (f *fakeSettings) Upsert(_ context.Context, s *models.TrackingSettings) error {
	cp := *s
	f.m[s.SellerID] = &cp
	return nil
}

func newAnalytics() (*AnalyticsService, *fakeEvents) {
	ev := newFakeEvents()
	d := fakeDesigns{bySlug: map[string]string{"shop-a": "sellerA", "shop-b": "sellerB"}}
	return NewAnalyticsService(ev, fakeProducts{}, analyticsOrders{}, d), ev
}

/* ---------------- analytics tests ---------------- */

func TestTrackRejectsPurchaseFromPublicEndpoint(t *testing.T) {
	svc, ev := newAnalytics()
	err := svc.Track(context.Background(), TrackEventInput{StoreSlug: "shop-a", Kind: "purchase"})
	if err == nil {
		t.Fatal("purchase must not be accepted from the public endpoint")
	}
	if len(ev.rows) != 0 {
		t.Fatal("nothing should be stored")
	}
}

func TestTrackNormalisesSourceAndHashesVisitor(t *testing.T) {
	svc, ev := newAnalytics()
	ctx := context.Background()
	in := TrackEventInput{StoreSlug: "shop-a", Kind: "product_view", ProductID: "p1",
		Source: "https://l.facebook.com/x?u=1", VisitorID: "visitor-12345678", ClientIP: "1.2.3.4", UserAgent: "UA"}
	if err := svc.Track(ctx, in); err != nil {
		t.Fatal(err)
	}
	if err := svc.Track(ctx, in); err != nil { // refresh: counted once
		t.Fatal(err)
	}
	if len(ev.rows) != 1 {
		t.Fatalf("want 1 event, got %d", len(ev.rows))
	}
	e := ev.rows[0]
	if e.Source != "facebook" || e.SellerID != "sellerA" {
		t.Errorf("source/seller = %q/%q", e.Source, e.SellerID)
	}
	if e.Visitor == "" || e.Visitor == "visitor-12345678" || e.Visitor == "1.2.3.4" {
		t.Errorf("visitor must be a hash, got %q", e.Visitor)
	}
	// junk sources collapse instead of creating new buckets
	_ = svc.Track(ctx, TrackEventInput{StoreSlug: "shop-a", Kind: "traffic", Source: "zzz-random-123"})
	if got := ev.rows[len(ev.rows)-1].Source; got != "other" {
		t.Errorf("junk source = %q, want other", got)
	}
}

func TestOverviewFunnelAndSourceIsolation(t *testing.T) {
	svc, _ := newAnalytics()
	ctx := context.Background()
	visit := func(slug, src, vid string) {
		t.Helper()
		if err := svc.Track(ctx, TrackEventInput{StoreSlug: slug, Kind: "traffic", Source: src, VisitorID: vid}); err != nil {
			t.Fatal(err)
		}
	}
	// Store A: 3 facebook visitors, 1 tiktok visitor, 1 google visitor.
	visit("shop-a", "facebook", "fb-visitor-1")
	visit("shop-a", "fb", "fb-visitor-2")
	visit("shop-a", "facebook.com", "fb-visitor-3")
	visit("shop-a", "tiktok", "tt-visitor-1")
	visit("shop-a", "", "direct-visitor-1")
	// Store B gets traffic too; it must never show in A.
	visit("shop-b", "facebook", "fb-visitor-1")
	visit("shop-b", "facebook", "b-visitor-9")

	for _, k := range []string{"product_view", "product_click", "add_to_cart"} {
		if err := svc.Track(ctx, TrackEventInput{StoreSlug: "shop-a", Kind: k, ProductID: "p1", Source: "facebook", VisitorID: "fb-visitor-1"}); err != nil {
			t.Fatal(err)
		}
	}
	for i, o := range []string{"o1", "o2"} {
		src := "facebook"
		if i == 1 {
			src = "tiktok"
		}
		if err := svc.TrackPurchase(ctx, PurchaseInput{SellerID: "sellerA", OrderID: o, Total: 500, Source: src, VisitorID: "fb-visitor-1"}); err != nil {
			t.Fatal(err)
		}
	}
	// replaying the same order (retry) must not double count
	_ = svc.TrackPurchase(ctx, PurchaseInput{SellerID: "sellerA", OrderID: "o1", Total: 500, Source: "facebook"})

	a, err := svc.Overview(ctx, "sellerA")
	if err != nil {
		t.Fatal(err)
	}
	f := a.Funnel
	if f.Visits != 5 || f.Visitors != 5 {
		t.Errorf("visits/visitors = %d/%d, want 5/5", f.Visits, f.Visitors)
	}
	if f.ProductViews != 1 || f.ProductClicks != 1 || f.AddToCart != 1 {
		t.Errorf("views/clicks/carts = %d/%d/%d", f.ProductViews, f.ProductClicks, f.AddToCart)
	}
	if f.Purchases != 2 || f.PurchaseValue != 1000 {
		t.Errorf("purchases = %d value %v", f.Purchases, f.PurchaseValue)
	}
	if f.ConversionRate != 40 { // 2 purchases / 5 visits
		t.Errorf("conversion = %v, want 40", f.ConversionRate)
	}
	if len(a.Sources) == 0 || a.Sources[0].Source != "facebook" {
		t.Fatalf("top source = %+v", a.Sources)
	}
	fb := a.Sources[0]
	if fb.Visits != 3 || fb.Visitors != 3 || fb.Purchases != 1 || fb.Revenue != 500 || fb.Label != "Facebook" {
		t.Errorf("facebook row = %+v", fb)
	}
	if fb.ConversionRate < 33.3 || fb.ConversionRate > 33.4 {
		t.Errorf("facebook conversion = %v", fb.ConversionRate)
	}
	var sawTikTok bool
	for _, r := range a.Sources {
		if r.Source == "tiktok" {
			sawTikTok = true
			if r.Visits != 1 || r.Purchases != 1 || r.ConversionRate != 100 {
				t.Errorf("tiktok row = %+v", r)
			}
		}
	}
	if !sawTikTok {
		t.Error("tiktok missing from sources")
	}

	b, _ := svc.Overview(ctx, "sellerB")
	if b.Funnel.Visits != 2 || b.Funnel.Purchases != 0 || b.Funnel.PurchaseValue != 0 {
		t.Errorf("store B leaked or lost data: %+v", b.Funnel)
	}
	// the same person on two stores is two different visitors
	if a.Funnel.Visitors+b.Funnel.Visitors != 7 {
		t.Errorf("visitor hashes should differ per store")
	}
	// the daily series carries visits and purchases for today
	last := a.DailyViews[len(a.DailyViews)-1]
	if last.Visits != 5 || last.Purchases != 2 || last.Clicks != 1 {
		t.Errorf("today's row = %+v", last)
	}
}

func TestUnconsentedVisitorsAreNotLinkedAcrossDaysOrPeople(t *testing.T) {
	svc, ev := newAnalytics()
	ctx := context.Background()
	// No visitor ID (visitor did not consent). Two different people, same store.
	_ = svc.Track(ctx, TrackEventInput{StoreSlug: "shop-a", Kind: "traffic", ClientIP: "1.1.1.1", UserAgent: "A"})
	_ = svc.Track(ctx, TrackEventInput{StoreSlug: "shop-a", Kind: "traffic", ClientIP: "2.2.2.2", UserAgent: "B"})
	if len(ev.rows) != 2 || ev.rows[0].Visitor == ev.rows[1].Visitor {
		t.Fatalf("two anonymous visitors should stay distinct: %+v", ev.rows)
	}
}

/* ---------------- tracking settings tests ---------------- */

func newTracking() *TrackingService {
	d := fakeDesigns{bySlug: map[string]string{"shop-a": "sellerA", "shop-b": "sellerB"}}
	return NewTrackingService(&fakeSettings{m: map[string]*models.TrackingSettings{}}, d)
}

func TestTrackingSettingsDefaultsAndSave(t *testing.T) {
	svc := newTracking()
	ctx := context.Background()

	got, err := svc.Get(ctx, "sellerA")
	if err != nil || !got.RequireConsent || got.MetaPixelID != "" {
		t.Fatalf("defaults = %+v, %v", got, err)
	}

	saved, err := svc.Save(ctx, "sellerA", TrackingSettings{MetaPixelID: " 123456789012345 ", TikTokPixelID: " c4abc123def456ghi789 ", RequireConsent: true})
	if err != nil {
		t.Fatal(err)
	}
	if saved.MetaPixelID != "123456789012345" || saved.TikTokPixelID != "C4ABC123DEF456GHI789" {
		t.Errorf("not normalised: %+v", saved)
	}

	// invalid IDs are rejected (and that stops script injection)
	for _, bad := range []TrackingSettings{
		{MetaPixelID: "1'); alert(1);//"},
		{TikTokPixelID: "x'; alert(1)//"},
		{MetaPixelID: "123"},
	} {
		if _, err := svc.Save(ctx, "sellerA", bad); err == nil {
			t.Errorf("should reject %+v", bad)
		}
	}
	after, _ := svc.Get(ctx, "sellerA")
	if after.MetaPixelID != "123456789012345" {
		t.Error("a rejected save must not change stored settings")
	}

	// empty strings switch pixels off; consent can be turned off by the seller
	cleared, err := svc.Save(ctx, "sellerA", TrackingSettings{RequireConsent: false})
	if err != nil || cleared.MetaPixelID != "" || cleared.TikTokPixelID != "" || cleared.RequireConsent {
		t.Errorf("cleared = %+v, %v", cleared, err)
	}
}

func TestTrackingSettingsAreIsolatedPerStore(t *testing.T) {
	svc := newTracking()
	ctx := context.Background()
	if _, err := svc.Save(ctx, "sellerA", TrackingSettings{MetaPixelID: "111111111111", RequireConsent: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Save(ctx, "sellerB", TrackingSettings{MetaPixelID: "222222222222", TikTokPixelID: "BBBBBBBBBBBB", RequireConsent: true}); err != nil {
		t.Fatal(err)
	}
	a, err := svc.PublicBySlug(ctx, "shop-a")
	if err != nil || a.MetaPixelID != "111111111111" || a.TikTokPixelID != "" {
		t.Errorf("store A public = %+v, %v", a, err)
	}
	b, _ := svc.PublicBySlug(ctx, "SHOP-B ")
	if b.MetaPixelID != "222222222222" || b.TikTokPixelID != "BBBBBBBBBBBB" {
		t.Errorf("store B public = %+v", b)
	}
	if _, err := svc.PublicBySlug(ctx, "no-such-shop"); err == nil {
		t.Error("unknown / unpublished store must be a 404")
	}
}
