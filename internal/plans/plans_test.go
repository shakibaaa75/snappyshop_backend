package plans

import (
	"strings"
	"sync"
	"testing"
)

func cfgWith(t *testing.T, edit func(c *Config)) Config {
	t.Helper()
	c := DefaultConfig()
	edit(&c)
	return c
}

func planOf(c *Config, id string) *PlanConfig {
	for i := range c.Plans {
		if c.Plans[i].ID == id {
			return &c.Plans[i]
		}
	}
	return nil
}

func TestDefaultsRoundTrip(t *testing.T) {
	Reset()
	if err := Apply(DefaultConfig()); err != nil {
		t.Fatalf("defaults must be valid: %v", err)
	}
	pro, _ := Get(Pro)
	if pro.Price[Monthly] != 299 || !pro.Analytics || pro.FreeThemes != nil {
		t.Errorf("pro changed by applying defaults: %+v", pro)
	}
	free, _ := Get(Free)
	if free.FreeThemes == nil || free.MaxProducts != FreeMaxProducts {
		t.Errorf("free changed by applying defaults: %+v", free)
	}
}

func TestPriceChangeFlowsIntoQuotes(t *testing.T) {
	t.Cleanup(Reset)
	err := Apply(cfgWith(t, func(c *Config) {
		planOf(c, Pro).MonthlyPrice = 399
		c.YearlyPct = 30
		c.FirstPurchasePct = 10
	}))
	if err != nil {
		t.Fatal(err)
	}
	q, ok := QuoteFor(Pro, Monthly, false)
	if !ok || q.Amount != 399 {
		t.Errorf("monthly = %+v", q)
	}
	// 399*12 = 4788, 40% off (30 + 10 first purchase) = 2872.8 -> 2873
	q, _ = QuoteFor(Pro, Yearly, true)
	if q.ListAmount != 4788 || q.TotalPct != 40 || q.Amount != 2873 {
		t.Errorf("yearly first = %+v", q)
	}
	if PeriodDiscountPct(HalfYearly) != DefaultHalfYearlyPct {
		t.Errorf("half yearly pct should be untouched")
	}
}

func TestFeatureSwitchesAndExtras(t *testing.T) {
	t.Cleanup(Reset)
	err := Apply(cfgWith(t, func(c *Config) {
		p := planOf(c, Pro)
		p.CustomDomain = true
		p.Analytics = false
		p.MaxProducts = 800
		p.Extras = []Extra{
			{Label: "  Priority support  ", Included: true},
			{Label: "Dedicated manager", Included: false},
			{Label: "", Included: true}, // blank row dropped
		}
	}))
	if err != nil {
		t.Fatal(err)
	}
	pro, _ := Get(Pro)
	if !pro.CustomDomain || pro.Analytics || pro.MaxProducts != 800 {
		t.Errorf("switches not applied: %+v", pro)
	}
	if len(pro.Extras) != 2 || pro.Extras[0].Key != "priority_support" || pro.Extras[0].Label != "Priority support" {
		t.Errorf("extras = %+v", pro.Extras)
	}
	keys := pro.ExtraKeys()
	if len(keys) != 1 || keys[0] != "priority_support" {
		t.Errorf("included keys = %v", keys)
	}
}

func TestNonLatinExtraGetsStableKey(t *testing.T) {
	a := keyFromLabel("অগ্রাধিকার সাপোর্ট")
	b := keyFromLabel("অগ্রাধিকার সাপোর্ট")
	if a != b || !extraKeyRe.MatchString(a) {
		t.Errorf("key %q / %q", a, b)
	}
}

func TestApplyRejectsBadInput(t *testing.T) {
	t.Cleanup(Reset)
	cases := map[string]func(c *Config){
		"zero paid price":      func(c *Config) { planOf(c, Pro).MonthlyPrice = 0 },
		"negative price":       func(c *Config) { planOf(c, Pro).MonthlyPrice = -5 },
		"free price set":       func(c *Config) { planOf(c, Free).MonthlyPrice = 10 },
		"huge price":           func(c *Config) { planOf(c, Pro).MonthlyPrice = MaxPrice + 1 },
		"zero products":        func(c *Config) { planOf(c, Pro).MaxProducts = 0 },
		"empty name":           func(c *Config) { planOf(c, Pro).Name = "  " },
		"discount over cap":    func(c *Config) { c.YearlyPct = 91 },
		"negative discount":    func(c *Config) { c.HalfYearlyPct = -1 },
		"combined over cap":    func(c *Config) { c.YearlyPct = 90; c.FirstPurchasePct = 10 },
		"unknown plan":         func(c *Config) { c.Plans = append(c.Plans, PlanConfig{ID: "gold", Name: "Gold"}) },
		"missing plan":         func(c *Config) { c.Plans = c.Plans[:2] },
		"duplicate plan":       func(c *Config) { c.Plans = append(c.Plans, c.Plans[1]) },
		"two popular":          func(c *Config) { planOf(c, Business).Popular = true },
		"duplicate extra keys": func(c *Config) { planOf(c, Pro).Extras = []Extra{{Label: "A b"}, {Label: "a-b"}} },
		"bad explicit key":     func(c *Config) { planOf(c, Pro).Extras = []Extra{{Key: "Bad Key", Label: "x"}} },
		"too many extras": func(c *Config) {
			for i := 0; i <= MaxExtrasPerPlan; i++ {
				p := planOf(c, Pro)
				p.Extras = append(p.Extras, Extra{Label: strings.Repeat("x", 3) + string(rune('a'+i))})
			}
		},
	}
	for name, edit := range cases {
		before := Snapshot()
		err := Apply(cfgWith(t, edit))
		if err == nil {
			t.Errorf("%s: expected an error", name)
			continue
		}
		if _, ok := err.(*ValidationError); !ok {
			t.Errorf("%s: want *ValidationError, got %T", name, err)
		}
		after := Snapshot()
		if p, _ := Get(Pro); p.Price[Monthly] != 299 || before.YearlyPct != after.YearlyPct {
			t.Errorf("%s: a rejected update changed the live catalog", name)
		}
	}
}

func TestAllDesignsToggle(t *testing.T) {
	t.Cleanup(Reset)
	if err := Apply(cfgWith(t, func(c *Config) {
		planOf(c, Free).AllDesigns = true
		planOf(c, Pro).AllDesigns = false
	})); err != nil {
		t.Fatal(err)
	}
	free, _ := Get(Free)
	pro, _ := Get(Pro)
	if !free.ThemeAllowed("anything") || pro.ThemeAllowed("anything") || !pro.ThemeAllowed("emerald") {
		t.Errorf("design rules not applied: free=%v pro=%v", free.FreeThemes, pro.FreeThemes)
	}
}

func TestGetReturnsACopy(t *testing.T) {
	t.Cleanup(Reset)
	p, _ := Get(Pro)
	p.Price[Monthly] = 1
	p.Extras = append(p.Extras, Extra{Key: "x", Label: "x"})
	again, _ := Get(Pro)
	if again.Price[Monthly] != 299 || len(again.Extras) != 0 {
		t.Errorf("catalog was modified through a returned copy: %+v", again)
	}
}

func TestConcurrentApplyAndRead(t *testing.T) {
	t.Cleanup(Reset)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			c := DefaultConfig()
			planOf(&c, Pro).MonthlyPrice = int64(300 + i)
			_ = Apply(c)
		}(i)
		go func() {
			defer wg.Done()
			_, _ = QuoteFor(Pro, Yearly, true)
			_ = All()
		}()
	}
	wg.Wait()
}
