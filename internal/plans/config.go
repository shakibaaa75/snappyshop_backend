package plans

import (
	"fmt"
	"hash/fnv"
	"regexp"
	"strings"
)

// Extra is a feature line an admin added to a plan, for example "Priority
// support". The built-in switches (analytics, custom domain, ...) are real
// checks in the backend; an Extra is listed on the pricing page and its key is
// sent to the app inside the seller's entitlements, so any screen can gate on
// it (entitlements.extras.includes("priority_support")).
type Extra struct {
	Key      string `json:"key" bson:"key"`
	Label    string `json:"label" bson:"label"`
	Included bool   `json:"included" bson:"included"` // false = shown as locked on this plan
}

// PlanConfig is the part of a Plan an admin may edit. It is also what the
// public pricing endpoint returns.
type PlanConfig struct {
	ID           string `json:"id" bson:"id"`
	Name         string `json:"name" bson:"name"`
	Blurb        string `json:"blurb" bson:"blurb"`
	About        string `json:"about" bson:"about"`
	Popular      bool   `json:"popular" bson:"popular"`
	MonthlyPrice int64  `json:"monthlyPrice" bson:"monthlyPrice"` // whole taka
	MaxProducts  int    `json:"maxProducts" bson:"maxProducts"`

	Analytics     bool `json:"analytics" bson:"analytics"`
	CustomDomain  bool `json:"customDomain" bson:"customDomain"`
	MetaPixel     bool `json:"metaPixel" bson:"metaPixel"`
	Pyxel         bool `json:"pyxel" bson:"pyxel"`
	AutoCourier   bool `json:"autoCourier" bson:"autoCourier"`
	PremiumDesign bool `json:"premiumDesign" bson:"premiumDesign"`
	// AllDesigns true = every theme and store template. False = only the free
	// starter themes and templates (FreeThemeIDs / FreeTemplateIDs).
	AllDesigns bool `json:"allDesigns" bson:"allDesigns"`

	Extras []Extra `json:"extras" bson:"extras"`
}

// Config is the whole editable catalog: the three plans plus the discount
// percentages that apply to every paid plan.
type Config struct {
	Plans            []PlanConfig `json:"plans" bson:"plans"`
	HalfYearlyPct    int          `json:"halfYearlyPct" bson:"halfYearlyPct"`
	YearlyPct        int          `json:"yearlyPct" bson:"yearlyPct"`
	FirstPurchasePct int          `json:"firstPurchasePct" bson:"firstPurchasePct"`
}

// Limits on what an admin can enter. They keep the pricing page readable and
// make sure a quote can never be zero or negative.
const (
	MaxPrice         = 1_000_000 // taka per month
	MaxProductsCap   = 1_000_000
	MaxDiscountPct   = 90 // each of the three percentages
	MaxTotalPct      = 95 // a period discount + the first-purchase discount
	MaxExtrasPerPlan = 20
	maxNameLen       = 30
	maxBlurbLen      = 80
	maxAboutLen      = 600
	maxLabelLen      = 120
)

// ValidationError is returned by Apply / Normalize for bad input. The message
// is safe to show to the admin.
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

func bad(format string, a ...any) error {
	return &ValidationError{Msg: fmt.Sprintf(format, a...)}
}

var extraKeyRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_]{0,47}$`)
var nonKeyChars = regexp.MustCompile(`[^a-z0-9]+`)

// keyFromLabel makes a stable key from a label. A label with no Latin letters
// or digits (for example Bangla text) gets a short hash instead, so the same
// label always produces the same key.
func keyFromLabel(label string) string {
	k := strings.Trim(nonKeyChars.ReplaceAllString(strings.ToLower(label), "_"), "_")
	if len(k) > 40 {
		k = strings.Trim(k[:40], "_")
	}
	if k == "" {
		h := fnv.New32a()
		_, _ = h.Write([]byte(label))
		k = fmt.Sprintf("x_%08x", h.Sum32())
	}
	return k
}

// Normalize validates cfg and returns a cleaned copy (trimmed text, generated
// extra keys). It never changes the live catalog.
func Normalize(cfg Config) (Config, error) {
	for _, p := range []struct {
		name string
		v    int
	}{{"Half-yearly discount", cfg.HalfYearlyPct}, {"Yearly discount", cfg.YearlyPct}, {"First-purchase discount", cfg.FirstPurchasePct}} {
		if p.v < 0 || p.v > MaxDiscountPct {
			return Config{}, bad("%s must be between 0 and %d%%", p.name, MaxDiscountPct)
		}
	}
	worst := cfg.HalfYearlyPct
	if cfg.YearlyPct > worst {
		worst = cfg.YearlyPct
	}
	if worst+cfg.FirstPurchasePct > MaxTotalPct {
		return Config{}, bad("A period discount plus the first-purchase discount cannot go above %d%%", MaxTotalPct)
	}

	want := map[string]bool{Free: true, Pro: true, Business: true}
	seen := map[string]bool{}
	out := Config{
		HalfYearlyPct:    cfg.HalfYearlyPct,
		YearlyPct:        cfg.YearlyPct,
		FirstPurchasePct: cfg.FirstPurchasePct,
	}
	for _, in := range cfg.Plans {
		if !want[in.ID] {
			return Config{}, bad("Unknown plan %q", in.ID)
		}
		if seen[in.ID] {
			return Config{}, bad("Plan %q is listed twice", in.ID)
		}
		seen[in.ID] = true

		p := in
		p.Name = strings.TrimSpace(p.Name)
		p.Blurb = strings.TrimSpace(p.Blurb)
		p.About = strings.TrimSpace(p.About)
		if p.Name == "" || len([]rune(p.Name)) > maxNameLen {
			return Config{}, bad("Plan name must be 1-%d characters", maxNameLen)
		}
		if len([]rune(p.Blurb)) > maxBlurbLen {
			return Config{}, bad("%s: the short description is limited to %d characters", p.Name, maxBlurbLen)
		}
		if len([]rune(p.About)) > maxAboutLen {
			return Config{}, bad("%s: the long description is limited to %d characters", p.Name, maxAboutLen)
		}
		if p.MaxProducts < 1 || p.MaxProducts > MaxProductsCap {
			return Config{}, bad("%s: product limit must be between 1 and %d", p.Name, MaxProductsCap)
		}
		if p.ID == Free {
			if p.MonthlyPrice != 0 {
				return Config{}, bad("The Free plan price must stay 0")
			}
			p.Popular = false
		} else if p.MonthlyPrice < 1 || p.MonthlyPrice > MaxPrice {
			return Config{}, bad("%s: monthly price must be between 1 and %d taka", p.Name, MaxPrice)
		}
		if len(p.Extras) > MaxExtrasPerPlan {
			return Config{}, bad("%s: at most %d extra features per plan", p.Name, MaxExtrasPerPlan)
		}
		extras := make([]Extra, 0, len(p.Extras))
		keys := map[string]bool{}
		for _, e := range p.Extras {
			e.Label = strings.TrimSpace(e.Label)
			if e.Label == "" {
				continue // a blank row is just dropped
			}
			if len([]rune(e.Label)) > maxLabelLen {
				return Config{}, bad("%s: a feature name is limited to %d characters", p.Name, maxLabelLen)
			}
			e.Key = strings.TrimSpace(e.Key)
			if e.Key == "" {
				e.Key = keyFromLabel(e.Label)
			}
			if !extraKeyRe.MatchString(e.Key) {
				return Config{}, bad("%s: feature key %q is not valid", p.Name, e.Key)
			}
			if keys[e.Key] {
				return Config{}, bad("%s: two features share the key %q; rename one", p.Name, e.Key)
			}
			keys[e.Key] = true
			extras = append(extras, e)
		}
		p.Extras = extras
		out.Plans = append(out.Plans, p)
	}
	for id := range want {
		if !seen[id] {
			return Config{}, bad("Plan %q is missing", id)
		}
	}
	// Only one plan can carry the "Popular" badge.
	popular := 0
	for _, p := range out.Plans {
		if p.Popular {
			popular++
		}
	}
	if popular > 1 {
		return Config{}, bad("Only one plan can be marked Popular")
	}
	return out, nil
}

// planFromConfig builds the runtime Plan, keeping the theme/template rules.
func planFromConfig(c PlanConfig) Plan {
	p := Plan{
		ID: c.ID, Name: c.Name, Blurb: c.Blurb, About: c.About, Popular: c.Popular,
		MaxProducts: c.MaxProducts,
		Analytics:   c.Analytics, CustomDomain: c.CustomDomain, MetaPixel: c.MetaPixel,
		Pyxel: c.Pyxel, AutoCourier: c.AutoCourier, PremiumDesign: c.PremiumDesign,
		Extras: append([]Extra(nil), c.Extras...),
		Price:  map[string]int64{Monthly: c.MonthlyPrice},
	}
	if !c.AllDesigns {
		p.FreeThemes = FreeThemeIDs
		p.FreeTemplates = FreeTemplateIDs
	}
	return p
}

func configFromPlan(p Plan) PlanConfig {
	return PlanConfig{
		ID: p.ID, Name: p.Name, Blurb: p.Blurb, About: p.About, Popular: p.Popular,
		MonthlyPrice: p.Price[Monthly], MaxProducts: p.MaxProducts,
		Analytics: p.Analytics, CustomDomain: p.CustomDomain, MetaPixel: p.MetaPixel,
		Pyxel: p.Pyxel, AutoCourier: p.AutoCourier, PremiumDesign: p.PremiumDesign,
		AllDesigns: p.FreeThemes == nil,
		Extras:     append([]Extra{}, p.Extras...),
	}
}

// Apply validates cfg and, if it is good, makes it the live catalog. On error
// nothing changes. It is safe to call while requests are being served.
func Apply(cfg Config) error {
	clean, err := Normalize(cfg)
	if err != nil {
		return err
	}
	byID := make(map[string]PlanConfig, len(clean.Plans))
	for _, p := range clean.Plans {
		byID[p.ID] = p
	}
	next := make([]Plan, 0, 3)
	for _, id := range []string{Free, Pro, Business} { // fixed display order
		next = append(next, planFromConfig(byID[id]))
	}
	mu.Lock()
	catalog = next
	halfPct, yearlyPct, firstPct = clean.HalfYearlyPct, clean.YearlyPct, clean.FirstPurchasePct
	mu.Unlock()
	return nil
}

// Snapshot returns the live catalog in editable form.
func Snapshot() Config {
	mu.RLock()
	defer mu.RUnlock()
	cfg := Config{HalfYearlyPct: halfPct, YearlyPct: yearlyPct, FirstPurchasePct: firstPct}
	for _, p := range catalog {
		cfg.Plans = append(cfg.Plans, configFromPlan(p))
	}
	return cfg
}

// Reset puts the built-in plans and discounts back (used by tests and by the
// admin "restore defaults" button).
func Reset() {
	mu.Lock()
	catalog = defaultCatalog()
	halfPct, yearlyPct, firstPct = DefaultHalfYearlyPct, DefaultYearlyPct, DefaultFirstPurchasePct
	mu.Unlock()
}

// DefaultConfig is the built-in catalog in editable form.
func DefaultConfig() Config {
	cfg := Config{HalfYearlyPct: DefaultHalfYearlyPct, YearlyPct: DefaultYearlyPct, FirstPurchasePct: DefaultFirstPurchasePct}
	for _, p := range defaultCatalog() {
		cfg.Plans = append(cfg.Plans, configFromPlan(p))
	}
	return cfg
}

// ExtraKeys lists the keys of the extras this plan includes.
func (p Plan) ExtraKeys() []string {
	keys := []string{}
	for _, e := range p.Extras {
		if e.Included {
			keys = append(keys, e.Key)
		}
	}
	return keys
}
