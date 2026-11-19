package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/repository"
	"shopbd/backend/internal/tracking"
)

type AnalyticsService struct {
	events   repository.AnalyticsRepository
	products repository.ProductRepository
	orders   repository.OrderRepository
	designs  repository.StoreDesignRepository
}

func NewAnalyticsService(
	events repository.AnalyticsRepository,
	products repository.ProductRepository,
	orders repository.OrderRepository,
	designs repository.StoreDesignRepository,
) *AnalyticsService {
	return &AnalyticsService{
		events:   events,
		products: products,
		orders:   orders,
		designs:  designs,
	}
}

/* ------------------------------------------------------------------ */
/* Track (public)                                                      */
/* ------------------------------------------------------------------ */

type TrackEventInput struct {
	StoreSlug string
	Kind      string // product_view | product_click | add_to_cart | search | traffic
	ProductID string
	Term      string
	Source    string // utm_source / referrer host; normalised by tracking.NormalizeSource

	// Used only to dedupe and to count unique visitors. Only a one-way hash is
	// stored. VisitorID is empty when the visitor has not consented to a local
	// device ID; then IP + user agent + day are hashed instead.
	VisitorID string // random id from the browser (localStorage)
	ClientIP  string // fallback when VisitorID is missing
	UserAgent string // fallback when VisitorID is missing
}

// viewWindow: one product_view / add_to_cart per device per product per window.
// Change this one constant to tune it (e.g. 1*time.Hour, 24*time.Hour).
const viewWindow = 6 * time.Hour

// dedupeKey hashes the parts into a fixed-size key for the unique index.
// Empty parts become "-" so a missing field is stable across requests.
func dedupeKey(parts ...string) string {
	norm := make([]string, len(parts))
	for i, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			p = "-"
		}
		norm[i] = p
	}
	sum := sha256.Sum256([]byte(strings.Join(norm, "|")))
	return hex.EncodeToString(sum[:])
}

func (s *AnalyticsService) Track(ctx context.Context, in TrackEventInput) error {
	kind := strings.TrimSpace(strings.ToLower(in.Kind))
	switch kind {
	case models.AnalyticsEventProductView, models.AnalyticsEventAddToCart,
		models.AnalyticsEventSearch, models.AnalyticsEventTraffic,
		models.AnalyticsEventClick:
		// "purchase" is deliberately NOT accepted here: it is recorded by the
		// server when an order is created (see TrackPurchase), so it can't be forged.
	default:
		return apperr.Validation(map[string]string{
			"kind": "must be product_view, product_click, add_to_cart, search, or traffic",
		})
	}

	slug := strings.TrimSpace(strings.ToLower(in.StoreSlug))
	if slug == "" {
		return apperr.Validation(map[string]string{"storeSlug": "required"})
	}

	design, err := s.designs.GetPublishedBySlug(ctx, slug)
	if err != nil {
		if err == repository.ErrNotFound {
			return apperr.NotFound()
		}
		return err
	}

	e := &models.AnalyticsEvent{
		ID:        models.NewID(),
		SellerID:  design.SellerID,
		Kind:      kind,
		CreatedAt: time.Now().UTC(),
	}

	switch kind {
	case models.AnalyticsEventProductView, models.AnalyticsEventAddToCart, models.AnalyticsEventClick:
		pid := strings.TrimSpace(in.ProductID)
		if pid == "" {
			return apperr.Validation(map[string]string{"productId": "required"})
		}
		if _, err := s.products.FindByID(ctx, design.SellerID, pid); err != nil {
			if err == repository.ErrNotFound {
				return apperr.NotFound()
			}
			return err
		}
		e.ProductID = pid
	case models.AnalyticsEventSearch:
		term := strings.ToLower(strings.TrimSpace(in.Term))
		if term == "" {
			return apperr.Validation(map[string]string{"term": "required"})
		}
		if len(term) > 100 {
			term = term[:100]
		}
		e.Term = term
	case models.AnalyticsEventTraffic:
		e.Source = tracking.NormalizeSource(in.Source) // empty => "direct"
	}

	// Product events carry the visitor's traffic source too, so the overview
	// can show views / carts / purchases per source. Empty stays empty (old clients).
	if kind != models.AnalyticsEventSearch && kind != models.AnalyticsEventTraffic &&
		strings.TrimSpace(in.Source) != "" {
		e.Source = tracking.NormalizeSource(in.Source)
	}

	/* ---- visitor hash + dedupe ---- */
	day := e.CreatedAt.UTC().Format("2006-01-02")
	bucket := strconv.FormatInt(e.CreatedAt.Unix()/int64(viewWindow/time.Second), 10)
	// One-way hash, different per seller. Without a visitor ID it also includes
	// the day, so an unconsenting visitor can't be followed across days.
	visitor := tracking.VisitorHash(design.SellerID, in.VisitorID, in.ClientIP, in.UserAgent, day)
	if kind != models.AnalyticsEventSearch {
		e.Visitor = visitor
	}

	switch kind {
	case models.AnalyticsEventProductView, models.AnalyticsEventAddToCart, models.AnalyticsEventClick:
		// one event per device per product per viewWindow
		e.Dedupe = dedupeKey(design.SellerID, kind, e.ProductID, visitor, bucket)
	case models.AnalyticsEventTraffic:
		e.Dedupe = dedupeKey(design.SellerID, kind, e.Source, visitor, day)
	}
	// search events get no Dedupe, so every search is stored.

	// Insert treats "already counted" as success.
	return s.events.Insert(ctx, e)
}

// PurchaseInput describes a freshly placed order for conversion tracking.
type PurchaseInput struct {
	SellerID  string
	OrderID   string
	Total     float64
	Source    string // from the browser; normalised here
	VisitorID string
	ClientIP  string
	UserAgent string
}

// TrackPurchase records one "purchase" event for a placed order. The order
// handler calls it on the server after the order is stored, so purchases can't
// be forged through the public track endpoint. It is idempotent per order.
func (s *AnalyticsService) TrackPurchase(ctx context.Context, in PurchaseInput) error {
	if strings.TrimSpace(in.SellerID) == "" || strings.TrimSpace(in.OrderID) == "" {
		return apperr.Validation(map[string]string{"orderId": "required"})
	}
	now := time.Now().UTC()
	e := &models.AnalyticsEvent{
		ID:        models.NewID(),
		SellerID:  in.SellerID,
		Kind:      models.AnalyticsEventPurchase,
		OrderID:   in.OrderID,
		Value:     in.Total,
		CreatedAt: now,
		Visitor: tracking.VisitorHash(in.SellerID, in.VisitorID, in.ClientIP, in.UserAgent,
			now.Format("2006-01-02")),
		Dedupe: dedupeKey(in.SellerID, models.AnalyticsEventPurchase, in.OrderID),
	}
	// Orders placed without a recorded source count as direct.
	e.Source = tracking.NormalizeSource(in.Source)
	return s.events.Insert(ctx, e)
}

/* ------------------------------------------------------------------ */
/* Overview (seller)                                                   */
/* ------------------------------------------------------------------ */

type MonthlyRow struct {
	Month   string  `json:"month"`
	Orders  int     `json:"orders"`
	Revenue float64 `json:"revenue"`
}

type ProductViewRow struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Views int    `json:"views"`
}

// ProductCartRow is one row of the "top added to cart" table.
type ProductCartRow struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	AddToCart int     `json:"addToCart"`
	Views     int     `json:"views"`
	Rate      float64 `json:"rate"` // added ÷ views, in %
}

type TrendingRow struct {
	ID     string  `json:"id"`
	Name   string  `json:"name"`
	Views  int     `json:"views"`
	Orders int     `json:"orders"`
	Growth float64 `json:"growth"`
}

type TrafficRow struct {
	Source string `json:"source"`
	Label  string `json:"label"`
	Count  int    `json:"count"`
}

type KeywordRow struct {
	Term  string `json:"term"`
	Count int    `json:"count"`
}

type ComparisonRow struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	Views     int     `json:"views"`
	AddToCart int     `json:"addToCart"`
	CartRate  float64 `json:"cartRate"` // added ÷ views, in %
	Orders    int     `json:"orders"`
	Revenue   float64 `json:"revenue"`
	Trending  bool    `json:"trending"`
}

type DailyViewRow struct {
	Date      string `json:"date"` // YYYY-MM-DD
	Views     int    `json:"views"`
	AddToCart int    `json:"addToCart"`
	Visits    int    `json:"visits"`    // store visits
	Clicks    int    `json:"clicks"`    // product clicks
	Purchases int    `json:"purchases"` // orders placed
}

// FunnelStats is the store funnel for the last 30 days.
type FunnelStats struct {
	Visits         int     `json:"visits"`   // store visits (one per visitor per source per day)
	Visitors       int     `json:"visitors"` // unique visitors
	ProductViews   int     `json:"productViews"`
	ProductClicks  int     `json:"productClicks"`
	AddToCart      int     `json:"addToCart"`
	Purchases      int     `json:"purchases"`      // orders placed
	PurchaseValue  float64 `json:"purchaseValue"`  // total of those orders
	ConversionRate float64 `json:"conversionRate"` // purchases ÷ visits, in %
	CartToPurchase float64 `json:"cartToPurchase"` // purchases ÷ add to cart, in %
}

// SourceStatRow is one traffic source with its funnel, for the source table.
type SourceStatRow struct {
	Source         string  `json:"source"`
	Label          string  `json:"label"`
	Visits         int     `json:"visits"`
	Visitors       int     `json:"visitors"`
	ProductViews   int     `json:"productViews"`
	ProductClicks  int     `json:"productClicks"`
	AddToCart      int     `json:"addToCart"`
	Purchases      int     `json:"purchases"`
	Revenue        float64 `json:"revenue"`        // value of orders placed from this source
	ConversionRate float64 `json:"conversionRate"` // purchases ÷ visits, in %
}

type AnalyticsOverview struct {
	Overview struct {
		Views          int          `json:"views"`
		AddToCartRate  float64      `json:"addToCartRate"`
		Orders30       int          `json:"orders30"`     // confirmed orders, last 30 days
		OrderPerView   float64      `json:"orderPerView"` // confirmed orders ÷ product views, in %
		OrderPerCart   float64      `json:"orderPerCart"` // confirmed orders ÷ add to cart, in %
		Monthly        []MonthlyRow `json:"monthly"`
		ConfirmRate    float64      `json:"confirmRate"`
		AddToCartCount int          `json:"addToCartCount"` // add-to-cart events, last 30 days
		Confirmed30    int          `json:"confirmed30"`    // confirmed orders, last 30 days // confirmed orders ÷ all orders, in %
	} `json:"overview"`
	DailyViews       []DailyViewRow   `json:"dailyViews"`
	ProductViews     []ProductViewRow `json:"productViews"`
	ProductCarts     []ProductCartRow `json:"productCarts"`
	TrendingProducts []TrendingRow    `json:"trendingProducts"`
	TrafficSources   []TrafficRow     `json:"trafficSources"`
	SearchKeywords   []KeywordRow     `json:"searchKeywords"`
	Comparisons      []ComparisonRow  `json:"comparisons"`
	Funnel           FunnelStats      `json:"funnel"`
	Sources          []SourceStatRow  `json:"sources"` // per-source visits, visitors and conversion
	// Platforms holds one entry per ad platform (Meta, TikTok): the store events
	// that came from that platform's traffic. The page shows a platform only when
	// the seller has saved that platform's pixel ID.
	Platforms []PlatformStats `json:"platforms"`
}

// Limits for the aggregate queries. Kept in one place so the
// ranking queries and the per-product maps stay consistent.
const (
	overviewProductLimit = 500 // product list / per-product event maps
	overviewTopProductN  = 20  // "top viewed products" table
	overviewTopTrendN    = 10  // trending products table
	overviewTopTrafficN  = 10  // traffic sources table
	overviewTopSourceN   = 10  // per-source conversion table
	overviewTopTermN     = 15  // search keywords table
	overviewTopCompareN  = 30  // product comparison table
	overviewDailyDays    = 14  // daily-views window
	overviewMonthlyN     = 6   // months in the monthly revenue chart
)

func (s *AnalyticsService) Overview(ctx context.Context, sellerID string) (*AnalyticsOverview, error) {
	now := time.Now().UTC()
	since30 := now.AddDate(0, 0, -30)
	since7 := now.AddDate(0, 0, -7)
	prev7 := now.AddDate(0, 0, -14)

	out := &AnalyticsOverview{
		DailyViews:       []DailyViewRow{},
		ProductViews:     []ProductViewRow{},
		ProductCarts:     []ProductCartRow{},
		TrendingProducts: []TrendingRow{},
		TrafficSources:   []TrafficRow{},
		SearchKeywords:   []KeywordRow{},
		Comparisons:      []ComparisonRow{},
		Sources:          []SourceStatRow{},
		Platforms:        []PlatformStats{},
	}
	out.Overview.Monthly = []MonthlyRow{}

	/* ---- headline counters ---- */
	views, err := s.events.CountByKind(ctx, sellerID, models.AnalyticsEventProductView, since30)
	if err != nil {
		return nil, err
	}
	addToCart, err := s.events.CountByKind(ctx, sellerID, models.AnalyticsEventAddToCart, since30)
	if err != nil {
		return nil, err
	}
	out.Overview.Views = views
	out.Overview.AddToCartRate = ratePct(addToCart, views)

	/* ---- monthly revenue ---- */
	monthly, err := s.monthlyFromOrders(ctx, sellerID, now)
	if err != nil {
		return nil, err
	}
	out.Overview.Monthly = monthly

	/* ---- daily views + add-to-cart (last N calendar days, oldest first) ---- */
	startDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).
		AddDate(0, 0, -(overviewDailyDays - 1))
	endDay := startDay.AddDate(0, 0, overviewDailyDays) // exclusive

	byDay, err := s.events.DailyCounts(ctx, sellerID, models.AnalyticsEventProductView, startDay, endDay)
	if err != nil {
		return nil, err
	}
	byDayCart, err := s.events.DailyCounts(ctx, sellerID, models.AnalyticsEventAddToCart, startDay, endDay)
	if err != nil {
		return nil, err
	}
	byDayVisits, err := s.events.DailyCounts(ctx, sellerID, models.AnalyticsEventTraffic, startDay, endDay)
	if err != nil {
		return nil, err
	}
	byDayClicks, err := s.events.DailyCounts(ctx, sellerID, models.AnalyticsEventClick, startDay, endDay)
	if err != nil {
		return nil, err
	}
	byDayBuys, err := s.events.DailyCounts(ctx, sellerID, models.AnalyticsEventPurchase, startDay, endDay)
	if err != nil {
		return nil, err
	}
	for i := 0; i < overviewDailyDays; i++ {
		d := startDay.AddDate(0, 0, i)
		key := d.Format("2006-01-02")
		out.DailyViews = append(out.DailyViews, DailyViewRow{
			Date:      key,
			Views:     byDay[key],     // 0 if no events that day
			AddToCart: byDayCart[key], // 0 if no events that day
			Visits:    byDayVisits[key],
			Clicks:    byDayClicks[key],
			Purchases: byDayBuys[key],
		})
	}

	/* ---- product name lookup ---- */
	products, err := s.products.List(ctx, sellerID, overviewProductLimit)
	if err != nil {
		return nil, err
	}
	nameByID := make(map[string]string, len(products))
	for _, p := range products {
		nameByID[p.ID] = p.Name
	}

	/* ---- per-product view counts (full set, not just top N) ---- */
	viewCounts, err := s.events.ProductCounts(
		ctx, sellerID, models.AnalyticsEventProductView, since30, overviewProductLimit,
	)
	if err != nil {
		return nil, err
	}
	viewsByID := make(map[string]int, len(viewCounts))
	for _, vc := range viewCounts {
		viewsByID[vc.ProductID] = vc.Count
	}
	// "Top viewed products" table is a prefix of the sorted view counts.
	topViews := make([]repository.ProductCount, len(viewCounts))
	copy(topViews, viewCounts)
	sort.Slice(topViews, func(i, j int) bool { return topViews[i].Count > topViews[j].Count })
	if len(topViews) > overviewTopProductN {
		topViews = topViews[:overviewTopProductN]
	}
	for _, vc := range topViews {
		name := nameByID[vc.ProductID]
		if name == "" {
			name = "Product"
		}
		out.ProductViews = append(out.ProductViews, ProductViewRow{
			ID: vc.ProductID, Name: name, Views: vc.Count,
		})
	}

	/* ---- per-product sales (orders + revenue) ---- */
	orderList, err := s.orders.List(ctx, sellerID, repository.OrderFilter{Limit: 200})
	if err != nil {
		return nil, err
	}

	/* ---- confirmed-order conversion rates (last 30 days, same window as views) ---- */
	/* ---- order confirm rate: confirmed ÷ all orders placed ---- */
	/* ---- order confirm rate: average of confirmed ÷ views and confirmed ÷ add-to-cart (30 days) ---- */
	/* ---- order rate: average of orders ÷ views and orders ÷ add-to-cart (30 days) ---- */
	placed30 := 0
	for _, o := range orderList {
		if o.Status != models.OrderCancelled && !o.CreatedAt.Before(since30) {
			placed30++
		}
	}
	avgRate, n := 0.0, 0
	if views > 0 {
		avgRate += ratePct(placed30, views)
		n++
	}
	if addToCart > 0 {
		avgRate += ratePct(placed30, addToCart)
		n++
	}
	if n > 0 {
		avgRate /= float64(n)
	}
	out.Overview.AddToCartCount = addToCart
	out.Overview.Confirmed30 = placed30
	out.Overview.ConfirmRate = avgRate
	slog.Info("analytics rate", "views", views, "carts", addToCart, "orders30", placed30, "rate", avgRate)

	type prodSales struct {
		orders  int
		revenue float64
		week    int
		prev    int
	}
	sales := make(map[string]*prodSales)
	for _, o := range orderList {
		if o.Status == models.OrderCancelled {
			continue
		}
		// Revenue counts only delivered or paid orders, matching monthlyFromOrders.
		revenueCounts := o.Status == models.OrderDelivered || o.Paid
		seen := make(map[string]bool)
		for _, line := range o.Lines {
			ps := sales[line.ProductID]
			if ps == nil {
				ps = &prodSales{}
				sales[line.ProductID] = ps
			}
			if revenueCounts {
				ps.revenue += line.Subtotal
			}
			if !seen[line.ProductID] {
				ps.orders++
				seen[line.ProductID] = true
				if !o.CreatedAt.Before(since7) {
					ps.week++
				} else if !o.CreatedAt.Before(prev7) {
					ps.prev++
				}
			}
		}
	}

	/* ---- week-over-week view counts ---- */
	thisWeekViews, err := s.events.ProductCountsInRange(ctx, sellerID, models.AnalyticsEventProductView, since7, now)
	if err != nil {
		return nil, err
	}
	prevWeekViews, err := s.events.ProductCountsInRange(ctx, sellerID, models.AnalyticsEventProductView, prev7, since7)
	if err != nil {
		return nil, err
	}

	/* ---- trending products ---- */
	type trendCand struct {
		id, name      string
		views, orders int
		growth        float64
		score         float64
	}
	var cands []trendCand
	ids := make(map[string]struct{})
	for id := range thisWeekViews {
		ids[id] = struct{}{}
	}
	for id := range prevWeekViews {
		ids[id] = struct{}{}
	}
	for id := range sales {
		ids[id] = struct{}{}
	}
	for id := range ids {
		name := nameByID[id]
		if name == "" {
			continue
		}
		tv := thisWeekViews[id]
		pv := prevWeekViews[id]
		growth := growthPct(tv, pv)
		if tv == 0 && pv == 0 {
			if ps := sales[id]; ps != nil {
				growth = growthPct(ps.week, ps.prev)
			}
		}
		ord := 0
		if ps := sales[id]; ps != nil {
			ord = ps.orders
		}
		// Score balances relative growth against absolute traffic so that a
		// single view on a brand-new product doesn't outrank a high-traffic
		// product with the same percentage growth. log1p dampens the volume
		// term without zeroing out low-volume candidates.
		volume := float64(tv) + float64(ord)
		score := growth * math.Log1p(volume)
		cands = append(cands, trendCand{
			id: id, name: name, views: tv, orders: ord,
			growth: growth, score: score,
		})
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].score != cands[j].score {
			return cands[i].score > cands[j].score
		}
		return cands[i].views > cands[j].views
	})
	for _, c := range cands {
		if len(out.TrendingProducts) >= overviewTopTrendN {
			break
		}
		if c.views == 0 && c.orders == 0 && c.growth == 0 {
			continue
		}
		out.TrendingProducts = append(out.TrendingProducts, TrendingRow{
			ID: c.id, Name: c.name, Views: c.views, Orders: c.orders, Growth: c.growth,
		})
	}

	/* ---- traffic sources ---- */
	sources, err := s.events.SourceCounts(ctx, sellerID, since30, overviewTopTrafficN)
	if err != nil {
		return nil, err
	}
	for _, sc := range sources {
		out.TrafficSources = append(out.TrafficSources, TrafficRow{
			Source: sc.Source, Label: tracking.SourceLabel(sc.Source), Count: sc.Count,
		})
	}

	/* ---- funnel + per-source conversion (last 30 days) ---- */
	if err := s.fillFunnel(ctx, sellerID, since30, out); err != nil {
		return nil, err
	}

	/* ---- per-platform daily series (Meta, TikTok), same 14-day window ---- */
	s.fillPlatformDaily(ctx, sellerID, startDay, endDay, out)

	/* ---- search keywords ---- */
	terms, err := s.events.TermCounts(ctx, sellerID, since30, overviewTopTermN)
	if err != nil {
		return nil, err
	}
	for _, t := range terms {
		out.SearchKeywords = append(out.SearchKeywords, KeywordRow{Term: t.Term, Count: t.Count})
	}

	/* ---- per-product add-to-cart counts (full set) ---- */
	atcCounts, err := s.events.ProductCounts(
		ctx, sellerID, models.AnalyticsEventAddToCart, since30, overviewProductLimit,
	)
	if err != nil {
		return nil, err
	}
	atcByID := make(map[string]int, len(atcCounts))
	for _, a := range atcCounts {
		atcByID[a.ProductID] = a.Count
	}

	/* ---- top added-to-cart products (atcCounts is sorted by count, desc) ---- */
	for i, a := range atcCounts {
		if i >= overviewTopProductN {
			break
		}
		name := nameByID[a.ProductID]
		if name == "" {
			name = "Product"
		}
		out.ProductCarts = append(out.ProductCarts, ProductCartRow{
			ID:        a.ProductID,
			Name:      name,
			AddToCart: a.Count,
			Views:     viewsByID[a.ProductID],
			Rate:      ratePct(a.Count, viewsByID[a.ProductID]),
		})
	}

	trendingSet := make(map[string]bool)
	for _, t := range out.TrendingProducts {
		if t.Growth > 0 {
			trendingSet[t.ID] = true
		}
	}

	/* ---- product comparison table ---- */
	var comps []ComparisonRow
	for _, p := range products {
		v := viewsByID[p.ID]
		a := atcByID[p.ID]
		ord, rev := 0, 0.0
		if ps := sales[p.ID]; ps != nil {
			ord, rev = ps.orders, ps.revenue
		}
		if v == 0 && a == 0 && ord == 0 {
			continue
		}
		comps = append(comps, ComparisonRow{
			ID: p.ID, Name: p.Name, Views: v, AddToCart: a,
			CartRate: ratePct(a, v),
			Orders:   ord, Revenue: rev, Trending: trendingSet[p.ID],
		})
	}
	sort.Slice(comps, func(i, j int) bool {
		if comps[i].Views != comps[j].Views {
			return comps[i].Views > comps[j].Views
		}
		return comps[i].Orders > comps[j].Orders
	})
	if len(comps) > overviewTopCompareN {
		comps = comps[:overviewTopCompareN]
	}
	out.Comparisons = comps

	return out, nil
}

func growthPct(current, previous int) float64 {
	if previous == 0 {
		if current == 0 {
			return 0
		}
		return 100
	}
	return (float64(current-previous) / float64(previous)) * 100
}

// ratePct returns part/whole as a percentage, capped at 100 (an item can be
// added from a product card without a product page view).
func ratePct(part, whole int) float64 {
	if whole <= 0 {
		return 0
	}
	r := float64(part) / float64(whole) * 100
	if r > 100 {
		r = 100
	}
	return r
}

// isConfirmedOrder is true once the seller has accepted the order: anything
// except pending or cancelled (confirmed, shipped, delivered, ...).
// If your pending status uses a different string, change "pending" below.
func isConfirmedOrder(status string) bool {
	s := strings.ToLower(strings.TrimSpace(status))
	return s != "" && s != "pending" && s != string(models.OrderCancelled)
}

func (s *AnalyticsService) monthlyFromOrders(ctx context.Context, sellerID string, now time.Time) ([]MonthlyRow, error) {
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).
		AddDate(0, -(overviewMonthlyN - 1), 0)
	list, err := s.orders.List(ctx, sellerID, repository.OrderFilter{Limit: 200, Before: now.Add(24 * time.Hour)})
	if err != nil {
		return nil, err
	}

	type bucket struct {
		orders  int
		revenue float64
	}
	byKey := make(map[string]*bucket)
	for i := 0; i < overviewMonthlyN; i++ {
		m := start.AddDate(0, i, 0)
		key := m.Format("2006-01")
		byKey[key] = &bucket{}
	}
	for _, o := range list {
		if o.CreatedAt.Before(start) {
			continue
		}
		if o.Status == models.OrderCancelled {
			continue
		}
		key := o.CreatedAt.UTC().Format("2006-01")
		b := byKey[key]
		if b == nil {
			continue
		}
		b.orders++
		if o.Status == models.OrderDelivered || o.Paid {
			b.revenue += o.Total
		}
	}

	out := make([]MonthlyRow, 0, overviewMonthlyN)
	for i := 0; i < overviewMonthlyN; i++ {
		m := start.AddDate(0, i, 0)
		key := m.Format("2006-01")
		b := byKey[key]
		out = append(out, MonthlyRow{
			Month:   m.Format("Jan 2006"),
			Orders:  b.orders,
			Revenue: b.revenue,
		})
	}
	return out, nil
}

// fillFunnel fills out.Funnel and out.Sources from one grouped query plus the
// unique-visitor count. Rates are purchases ÷ visits, capped at 100%.
func (s *AnalyticsService) fillFunnel(ctx context.Context, sellerID string, since time.Time, out *AnalyticsOverview) error {
	stats, err := s.events.SourceBreakdown(ctx, sellerID, since)
	if err != nil {
		return err
	}
	visitors, err := s.events.UniqueVisitors(ctx, sellerID, since)
	if err != nil {
		return err
	}

	bySource := make(map[string]*SourceStatRow)
	row := func(src string) *SourceStatRow {
		r := bySource[src]
		if r == nil {
			r = &SourceStatRow{Source: src, Label: tracking.SourceLabel(src)}
			bySource[src] = r
		}
		return r
	}
	for _, st := range stats {
		if st.Source == "" {
			continue
		}
		r := row(st.Source)
		switch st.Kind {
		case models.AnalyticsEventTraffic:
			r.Visits += st.Count
			r.Visitors += st.Visitors
		case models.AnalyticsEventProductView:
			r.ProductViews += st.Count
		case models.AnalyticsEventClick:
			r.ProductClicks += st.Count
		case models.AnalyticsEventAddToCart:
			r.AddToCart += st.Count
		case models.AnalyticsEventPurchase:
			r.Purchases += st.Count
			r.Revenue += st.Value
		}
	}

	// Platform totals come from the full per-source map, BEFORE the table below
	// is cut to the top N sources, so a small platform is never dropped.
	out.Platforms = buildPlatformStats(bySource)

	rows := make([]SourceStatRow, 0, len(bySource))
	var f FunnelStats
	// Views, carts and clicks use the same all-events counters as the KPI tiles
	// (they include events stored before traffic sources were recorded).
	f.ProductViews = out.Overview.Views
	f.AddToCart = out.Overview.AddToCartCount
	if f.ProductClicks, err = s.events.CountByKind(ctx, sellerID, models.AnalyticsEventClick, since); err != nil {
		return err
	}
	for _, r := range bySource {
		r.ConversionRate = ratePct(r.Purchases, r.Visits)
		rows = append(rows, *r)
		f.Visits += r.Visits
		f.Purchases += r.Purchases
		f.PurchaseValue += r.Revenue
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Visits != rows[j].Visits {
			return rows[i].Visits > rows[j].Visits
		}
		if rows[i].Purchases != rows[j].Purchases {
			return rows[i].Purchases > rows[j].Purchases
		}
		return rows[i].Source < rows[j].Source
	})
	if len(rows) > overviewTopSourceN {
		rows = rows[:overviewTopSourceN]
	}
	out.Sources = rows

	f.Visitors = visitors
	f.ConversionRate = ratePct(f.Purchases, f.Visits)
	f.CartToPurchase = ratePct(f.Purchases, f.AddToCart)
	out.Funnel = f
	return nil
}
