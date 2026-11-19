package plans

import (
	"testing"
	"time"
)

func TestQuoteFor(t *testing.T) {
	cases := []struct {
		name     string
		plan     string
		interval string
		first    bool
		list     int64
		pct      int
		amount   int64
	}{
		{"pro 1 month", Pro, Monthly, false, 299, 0, 299},
		{"pro 6 months", Pro, HalfYearly, false, 1794, 10, 1615}, // 1614.6 -> 1615
		{"pro 12 months", Pro, Yearly, false, 3588, 20, 2870},    // 2870.4 -> 2870
		{"pro 1 month first", Pro, Monthly, true, 299, 5, 284},   // 284.05 -> 284
		{"pro 6 months first", Pro, HalfYearly, true, 1794, 15, 1525},
		{"pro 12 months first", Pro, Yearly, true, 3588, 25, 2691},
		{"business 1 month", Business, Monthly, false, 499, 0, 499},
		{"business 6 months", Business, HalfYearly, false, 2994, 10, 2695}, // 2694.6 -> 2695
		{"business 12 months", Business, Yearly, false, 5988, 20, 4790},    // 4790.4 -> 4790
		{"business 12 months first", Business, Yearly, true, 5988, 25, 4491},
	}
	for _, c := range cases {
		q, ok := QuoteFor(c.plan, c.interval, c.first)
		if !ok {
			t.Fatalf("%s: not ok", c.name)
		}
		if q.ListAmount != c.list || q.TotalPct != c.pct || q.Amount != c.amount {
			t.Errorf("%s: got list=%d pct=%d amount=%d, want list=%d pct=%d amount=%d",
				c.name, q.ListAmount, q.TotalPct, q.Amount, c.list, c.pct, c.amount)
		}
	}
}

func TestQuoteForRejects(t *testing.T) {
	if _, ok := QuoteFor(Free, Monthly, false); ok {
		t.Error("free plan must not be purchasable")
	}
	if _, ok := QuoteFor("nope", Monthly, false); ok {
		t.Error("unknown plan must be rejected")
	}
	if _, ok := QuoteFor(Pro, "weekly", false); ok {
		t.Error("unknown period must be rejected")
	}
}

func TestAddPeriod(t *testing.T) {
	from := time.Date(2027, 1, 31, 10, 0, 0, 0, time.UTC)
	if got := AddPeriod(from, HalfYearly); got.Month() != time.August && got.Month() != time.July {
		t.Errorf("6 months from Jan 31 landed in %v", got.Month())
	}
	if got := AddPeriod(from, Yearly); got.Year() != 2028 {
		t.Errorf("12 months from Jan 2027 gave year %d", got.Year())
	}
	if !ValidInterval(HalfYearly) || ValidInterval("weekly") {
		t.Error("ValidInterval wrong")
	}
}
