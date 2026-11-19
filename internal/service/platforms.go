package service

import (
	"context"
	"log/slog"
	"time"

	"shopbd/backend/internal/models"
	"shopbd/backend/internal/repository"
	"shopbd/backend/internal/tracking"
)

// platformDef maps an ad platform to the traffic sources it covers.
// To add a platform (for example Google), add one line here and the matching
// pixel check + logo on the frontend.
type platformDef struct {
	ID      string // "meta" | "tiktok" (also the key the frontend uses)
	Label   string
	Sources []string // normalised source keys, see tracking.NormalizeSource
}

var platformDefs = []platformDef{
	// Meta covers Facebook and Instagram (utm_source=meta / fb / messenger
	// normalise to "facebook").
	{ID: "meta", Label: "Meta", Sources: []string{tracking.SourceFacebook, tracking.SourceInstagram}},
	{ID: "tiktok", Label: "TikTok", Sources: []string{tracking.SourceTikTok}},
}

// PlatformTotals are one platform's numbers for the last 30 days.
type PlatformTotals struct {
	Visits         int     `json:"visits"`
	Views          int     `json:"views"`
	Clicks         int     `json:"clicks"`
	AddToCart      int     `json:"addToCart"`
	Purchases      int     `json:"purchases"`      // orders placed
	PurchaseValue  float64 `json:"purchaseValue"`  // total of those orders
	ConversionRate float64 `json:"conversionRate"` // purchases ÷ visits, in %
	CartToPurchase float64 `json:"cartToPurchase"` // purchases ÷ add to cart, in %
}

// PlatformChannel is one source inside a platform (Facebook, Instagram).
type PlatformChannel struct {
	Source    string  `json:"source"`
	Label     string  `json:"label"`
	Visits    int     `json:"visits"`
	Views     int     `json:"views"`
	Clicks    int     `json:"clicks"`
	AddToCart int     `json:"addToCart"`
	Purchases int     `json:"purchases"`
	Revenue   float64 `json:"revenue"`
}

// PlatformDay is one calendar day of a platform's events.
type PlatformDay struct {
	Date      string `json:"date"` // YYYY-MM-DD
	Visits    int    `json:"visits"`
	Views     int    `json:"views"`
	Clicks    int    `json:"clicks"`
	AddToCart int    `json:"addToCart"`
	Purchases int    `json:"purchases"`
}

// PlatformStats is one ad platform's section on the Analytics page.
type PlatformStats struct {
	Platform string            `json:"platform"`
	Label    string            `json:"label"`
	Totals   PlatformTotals    `json:"totals"`
	Channels []PlatformChannel `json:"channels"`
	Daily    []PlatformDay     `json:"daily"` // oldest first, zero-filled
}

// buildPlatformStats turns the per-source rows (already computed for the
// funnel) into one entry per platform. Every platform is always returned, even
// with zero events: the frontend decides what to show from the seller's pixel
// settings. Pure function, no I/O.
func buildPlatformStats(bySource map[string]*SourceStatRow) []PlatformStats {
	out := make([]PlatformStats, 0, len(platformDefs))
	for _, def := range platformDefs {
		p := PlatformStats{
			Platform: def.ID,
			Label:    def.Label,
			Channels: make([]PlatformChannel, 0, len(def.Sources)),
			Daily:    []PlatformDay{},
		}
		for _, src := range def.Sources {
			ch := PlatformChannel{Source: src, Label: tracking.SourceLabel(src)}
			if r := bySource[src]; r != nil {
				ch.Visits = r.Visits
				ch.Views = r.ProductViews
				ch.Clicks = r.ProductClicks
				ch.AddToCart = r.AddToCart
				ch.Purchases = r.Purchases
				ch.Revenue = r.Revenue
			}
			p.Channels = append(p.Channels, ch)

			p.Totals.Visits += ch.Visits
			p.Totals.Views += ch.Views
			p.Totals.Clicks += ch.Clicks
			p.Totals.AddToCart += ch.AddToCart
			p.Totals.Purchases += ch.Purchases
			p.Totals.PurchaseValue += ch.Revenue
		}
		p.Totals.ConversionRate = ratePct(p.Totals.Purchases, p.Totals.Visits)
		p.Totals.CartToPurchase = ratePct(p.Totals.Purchases, p.Totals.AddToCart)
		out = append(out, p)
	}
	return out
}

// zeroDays returns n consecutive UTC days starting at start, all zero.
func zeroDays(start time.Time, n int) []PlatformDay {
	days := make([]PlatformDay, 0, n)
	for i := 0; i < n; i++ {
		days = append(days, PlatformDay{Date: start.AddDate(0, 0, i).Format("2006-01-02")})
	}
	return days
}

// applyDailyStats adds the grouped (day, source, kind) rows into each
// platform's zero-filled Daily series. Rows outside the window or for sources
// that belong to no platform are ignored. Pure function, no I/O.
func applyDailyStats(platforms []PlatformStats, stats []repository.DailySourceKindStat) {
	platformBySource := map[string]int{}
	for i, def := range platformDefs {
		for _, src := range def.Sources {
			platformBySource[src] = i
		}
	}
	for _, st := range stats {
		pi, ok := platformBySource[st.Source]
		if !ok || pi >= len(platforms) {
			continue
		}
		days := platforms[pi].Daily
		for di := range days {
			if days[di].Date != st.Date {
				continue
			}
			switch st.Kind {
			case models.AnalyticsEventTraffic:
				days[di].Visits += st.Count
			case models.AnalyticsEventProductView:
				days[di].Views += st.Count
			case models.AnalyticsEventClick:
				days[di].Clicks += st.Count
			case models.AnalyticsEventAddToCart:
				days[di].AddToCart += st.Count
			case models.AnalyticsEventPurchase:
				days[di].Purchases += st.Count
			}
			break
		}
	}
}

// fillPlatformDaily fills each platform's Daily series for [startDay, endDay).
// It never fails the whole overview: if the daily query errors (or the
// repository does not support it), the charts stay at zero and the totals,
// which come from the funnel query, are unaffected.
func (s *AnalyticsService) fillPlatformDaily(ctx context.Context, sellerID string, startDay, endDay time.Time, out *AnalyticsOverview) {
	n := int(endDay.Sub(startDay).Hours() / 24)
	for i := range out.Platforms {
		out.Platforms[i].Daily = zeroDays(startDay, n)
	}

	counter, ok := s.events.(repository.DailySourceCounter)
	if !ok {
		return
	}
	var sources []string
	for _, def := range platformDefs {
		sources = append(sources, def.Sources...)
	}
	stats, err := counter.DailySourceCounts(ctx, sellerID, sources, startDay, endDay)
	if err != nil {
		slog.Warn("analytics: platform daily counts failed", "error", err.Error())
		return
	}
	applyDailyStats(out.Platforms, stats)
}
