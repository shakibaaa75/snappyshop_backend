package service

import (
	"testing"
	"time"

	"shopbd/backend/internal/models"
	"shopbd/backend/internal/repository"
)

func TestBuildPlatformStats_MetaCombinesFacebookAndInstagram(t *testing.T) {
	bySource := map[string]*SourceStatRow{
		"facebook":  {Source: "facebook", Visits: 100, ProductViews: 60, ProductClicks: 40, AddToCart: 10, Purchases: 4, Revenue: 4000},
		"instagram": {Source: "instagram", Visits: 50, ProductViews: 30, ProductClicks: 20, AddToCart: 5, Purchases: 1, Revenue: 1000},
		"tiktok":    {Source: "tiktok", Visits: 20, ProductViews: 10, ProductClicks: 8, AddToCart: 2, Purchases: 2, Revenue: 1500},
		"google":    {Source: "google", Visits: 999, Purchases: 99}, // must not leak into any platform
	}
	got := buildPlatformStats(bySource)
	if len(got) != 2 {
		t.Fatalf("want 2 platforms, got %d", len(got))
	}

	meta := got[0]
	if meta.Platform != "meta" {
		t.Fatalf("first platform = %q, want meta", meta.Platform)
	}
	want := PlatformTotals{Visits: 150, Views: 90, Clicks: 60, AddToCart: 15, Purchases: 5, PurchaseValue: 5000}
	g := meta.Totals
	if g.Visits != want.Visits || g.Views != want.Views || g.Clicks != want.Clicks ||
		g.AddToCart != want.AddToCart || g.Purchases != want.Purchases || g.PurchaseValue != want.PurchaseValue {
		t.Fatalf("meta totals = %+v, want %+v", g, want)
	}
	if len(meta.Channels) != 2 {
		t.Fatalf("meta channels = %d, want 2 (facebook, instagram)", len(meta.Channels))
	}
	// 5 purchases / 150 visits = 3.33...%
	if g.ConversionRate < 3.33 || g.ConversionRate > 3.34 {
		t.Fatalf("meta conversion = %v, want ~3.33", g.ConversionRate)
	}

	tt := got[1]
	if tt.Platform != "tiktok" || tt.Totals.Visits != 20 || tt.Totals.Purchases != 2 {
		t.Fatalf("tiktok = %+v", tt.Totals)
	}
}

func TestBuildPlatformStats_AlwaysReturnsEveryPlatform(t *testing.T) {
	got := buildPlatformStats(map[string]*SourceStatRow{})
	if len(got) != 2 {
		t.Fatalf("want 2 platforms even with no data, got %d", len(got))
	}
	for _, p := range got {
		if p.Totals.Visits != 0 || p.Totals.ConversionRate != 0 {
			t.Fatalf("%s should be all zero: %+v", p.Platform, p.Totals)
		}
		if len(p.Channels) == 0 {
			t.Fatalf("%s should list its channels", p.Platform)
		}
	}
}

func TestApplyDailyStats(t *testing.T) {
	start := time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)
	platforms := buildPlatformStats(map[string]*SourceStatRow{})
	for i := range platforms {
		platforms[i].Daily = zeroDays(start, 4) // 25, 26, 27, 28 Oct
	}

	applyDailyStats(platforms, []repository.DailySourceKindStat{
		{Date: "2026-10-26", Source: "facebook", Kind: models.AnalyticsEventProductView, Count: 5},
		{Date: "2026-10-26", Source: "instagram", Kind: models.AnalyticsEventProductView, Count: 3},
		{Date: "2026-10-27", Source: "facebook", Kind: models.AnalyticsEventPurchase, Count: 2},
		{Date: "2026-10-27", Source: "tiktok", Kind: models.AnalyticsEventAddToCart, Count: 7},
		{Date: "2026-10-27", Source: "google", Kind: models.AnalyticsEventProductView, Count: 50},   // ignored
		{Date: "2026-09-01", Source: "facebook", Kind: models.AnalyticsEventProductView, Count: 50}, // outside window
	})

	meta, tt := platforms[0], platforms[1]
	if meta.Daily[1].Views != 8 {
		t.Fatalf("meta views on 26th = %d, want 8 (facebook + instagram)", meta.Daily[1].Views)
	}
	if meta.Daily[2].Purchases != 2 {
		t.Fatalf("meta purchases on 27th = %d, want 2", meta.Daily[2].Purchases)
	}
	if tt.Daily[2].AddToCart != 7 {
		t.Fatalf("tiktok add-to-cart on 27th = %d, want 7", tt.Daily[2].AddToCart)
	}
	var total int
	for _, d := range meta.Daily {
		total += d.Views
	}
	if total != 8 {
		t.Fatalf("meta total daily views = %d, want 8 (google and out-of-window rows ignored)", total)
	}
}
