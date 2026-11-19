// Package plans is the single source of truth for subscription plans,
// prices and limits. Prices are whole taka (BDT).
package plans

import (
	"sync"
	"time"
)

const (
	Free     = "free"
	Pro      = "pro"
	Business = "business"

	// Billing periods a seller can buy.
	Monthly    = "monthly"     // 1 month
	HalfYearly = "half_yearly" // 6 months
	Yearly     = "yearly"      // 12 months
)

// Default discounts. The live values can be changed by an admin (Apply) and
// are read with FirstPurchasePct / PeriodDiscountPct.
const (
	DefaultFirstPurchasePct = 5
	DefaultHalfYearlyPct    = 10
	DefaultYearlyPct        = 20
)

// periodMonths is fixed: it is the meaning of the billing period names.
var periodMonths = map[string]int{Monthly: 1, HalfYearly: 6, Yearly: 12}

type Plan struct {
	ID            string
	Name          string
	MaxProducts   int
	FreeThemes    []string // theme IDs allowed on this plan; nil = all themes
	FreeCards     []string // card-design IDs allowed; nil = all designs
	FreeTemplates []string // store template IDs allowed; nil = all templates
	Analytics     bool
	CustomDomain  bool
	MetaPixel     bool // Meta (Facebook) Pixel integration
	Pyxel         bool // Pyxel integration
	AutoCourier   bool // automatic courier integration (connect + auto-ship)
	PremiumDesign bool // premium storefront design: Gallery grid card, Image gallery section, arranging homepage sections
	// Text shown on the pricing page (editable by an admin).
	Blurb   string
	About   string
	Popular bool
	// Extras are feature lines an admin added. Included ones are listed as
	// part of the plan and their keys are sent to the app as entitlements.
	Extras []Extra
	// Price is the MONTHLY list price in taka under the key Monthly. Longer
	// periods are priced from it with QuoteFor (never stored separately).
	Price map[string]int64
}

// FreeThemeIDs are the first three store themes in the theme list
// (frontend: themePresets in lib/store-design/types.ts). Keep both in sync.
var FreeThemeIDs = []string{"emerald", "ocean", "sunset"}

// FreeTemplateIDs are the first three store templates in the template list
// (frontend: storeTemplates in lib/store-design/templates.ts) plus
// "marketplace", the template every new store starts on, so a brand-new
// Free store can always save. Keep both lists in sync.
var FreeTemplateIDs = []string{"minimal", "premium", "promo", "marketplace"}

// FreeMaxProducts is the product cap on the Free plan.
const FreeMaxProducts = 20

// defaultCatalog is the built-in plan list. It is what runs until an admin
// saves changes, and what Reset restores.
func defaultCatalog() []Plan {
	return []Plan{
		{
			ID:            Free,
			Name:          "Free",
			Blurb:         "Start selling today",
			About:         "A free store to try ShopBD and make your first sales. You can list products, take orders and run your shop with a limit on products and designs. Upgrade whenever you outgrow it.",
			MaxProducts:   FreeMaxProducts,
			FreeThemes:    FreeThemeIDs,
			FreeTemplates: FreeTemplateIDs,
			// FreeCards stays nil (all designs): the placeholder IDs never matched
			// the real card styles, and card styles are not part of the lock rules.
			Price: map[string]int64{Monthly: 0},
		},
		{
			ID:            Pro,
			Name:          "Pro",
			Blurb:         "For growing stores",
			About:         "Everything you need to grow: more products, every theme and template, a premium storefront design, ad tracking and automatic courier booking, plus an analytics dashboard to see what works.",
			Popular:       true,
			MaxProducts:   500,
			Analytics:     true,
			MetaPixel:     true,
			Pyxel:         true,
			AutoCourier:   true,
			PremiumDesign: true,
			Price:         map[string]int64{Monthly: 299},
		},
		{
			ID:            Business,
			Name:          "Business",
			Blurb:         "For large catalogs",
			About:         "Built for big catalogs and established brands. You get everything in Pro, a much higher product limit, and your own custom domain so your store looks like your brand.",
			MaxProducts:   1500,
			Analytics:     true,
			CustomDomain:  true,
			MetaPixel:     true,
			Pyxel:         true,
			AutoCourier:   true,
			PremiumDesign: true,
			Price:         map[string]int64{Monthly: 499},
		},
	}
}

// The live catalog. Reads take the read lock; Apply swaps everything under the
// write lock, so a request never sees half of an update.
var (
	mu        sync.RWMutex
	catalog   = defaultCatalog()
	halfPct   = DefaultHalfYearlyPct
	yearlyPct = DefaultYearlyPct
	firstPct  = DefaultFirstPurchasePct
)

// clone returns a copy that shares no mutable state with the catalog.
func (p Plan) clone() Plan {
	out := p
	out.Extras = append([]Extra(nil), p.Extras...)
	out.Price = make(map[string]int64, len(p.Price))
	for k, v := range p.Price {
		out.Price[k] = v
	}
	return out
}

func All() []Plan {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]Plan, len(catalog))
	for i, p := range catalog {
		out[i] = p.clone()
	}
	return out
}

func Get(id string) (Plan, bool) {
	mu.RLock()
	defer mu.RUnlock()
	for _, p := range catalog {
		if p.ID == id {
			return p.clone(), true
		}
	}
	return Plan{}, false
}

// FirstPurchasePct is the extra percent off a seller's first paid purchase. It
// can be combined with any billing period.
func FirstPurchasePct() int {
	mu.RLock()
	defer mu.RUnlock()
	return firstPct
}

func ValidInterval(interval string) bool {
	_, ok := periodMonths[interval]
	return ok
}

// Months returns the length of a billing period in months (0 if unknown).
func Months(interval string) int { return periodMonths[interval] }

// PeriodDiscountPct returns the percent off for a billing period.
func PeriodDiscountPct(interval string) int {
	mu.RLock()
	defer mu.RUnlock()
	switch interval {
	case HalfYearly:
		return halfPct
	case Yearly:
		return yearlyPct
	}
	return 0
}

// Quote is the full price breakdown of one purchase. Amount is what the
// seller pays; it is always calculated on the server.
type Quote struct {
	Plan             string
	Interval         string
	Months           int
	ListAmount       int64 // monthly price x months, before any discount
	PeriodPct        int   // percent off for the period (set by an admin)
	FirstPurchasePct int   // 0 or the first-purchase percent (set by an admin)
	TotalPct         int   // PeriodPct + FirstPurchasePct
	Amount           int64 // whole taka the seller pays
}

// QuoteFor prices a paid plan for a billing period.
//
//	1 month   : list price
//	6 months  : half-yearly discount (default 10%)
//	12 months : yearly discount (default 20%)
//	first purchase: a further discount (default 5%), added to the percentages
//	above, so a default first 12-month purchase is 25% off the list price.
//
// The prices and percentages are the live ones an admin can change.
//
// The result is rounded to the nearest whole taka. ok is false for unknown
// plans, unknown periods and the free plan.
func QuoteFor(planID, interval string, firstPurchase bool) (Quote, bool) {
	p, ok := Get(planID)
	if !ok {
		return Quote{}, false
	}
	monthly := p.Price[Monthly]
	months := Months(interval)
	if monthly <= 0 || months <= 0 {
		return Quote{}, false
	}
	q := Quote{
		Plan:       planID,
		Interval:   interval,
		Months:     months,
		ListAmount: monthly * int64(months),
		PeriodPct:  PeriodDiscountPct(interval),
	}
	if firstPurchase {
		q.FirstPurchasePct = FirstPurchasePct()
	}
	q.TotalPct = q.PeriodPct + q.FirstPurchasePct
	// Integer math: round(list * (100 - pct) / 100), halves round up.
	q.Amount = (q.ListAmount*int64(100-q.TotalPct) + 50) / 100
	return q, true
}

// AddPeriod returns `from` plus one billing period (calendar months).
func AddPeriod(from time.Time, interval string) time.Time {
	m := Months(interval)
	if m == 0 {
		m = 1
	}
	return from.AddDate(0, m, 0)
}

func (p Plan) ThemeAllowed(id string) bool { return allowed(p.FreeThemes, id) }
func (p Plan) CardAllowed(id string) bool  { return allowed(p.FreeCards, id) }

// TemplateIDs is the effective template allowlist. Free/unknown plans cannot
// inherit nil (= all) from a permissive or stale catalog configuration.
// Keep this shared by enforcement and the entitlements response.
func (p Plan) TemplateIDs() []string {
	if p.ID != Pro && p.ID != Business {
		return append([]string{}, FreeTemplateIDs...)
	}
	return p.FreeTemplates
}

func (p Plan) TemplateAllowed(id string) bool { return allowed(p.TemplateIDs(), id) }

func allowed(list []string, id string) bool {
	if list == nil {
		return true
	}
	for _, v := range list {
		if v == id {
			return true
		}
	}
	return false
}
