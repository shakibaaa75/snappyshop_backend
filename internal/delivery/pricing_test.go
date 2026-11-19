package delivery

import (
	"math"
	"testing"

	"shopbd/backend/internal/models"
)

func TestZoneFor(t *testing.T) {
	cases := []struct {
		in   string
		want models.DeliveryZone
	}{
		// Inside Dhaka: the Dhaka district only.
		{"Dhaka", models.ZoneInsideDhaka},
		{"dhaka", models.ZoneInsideDhaka},
		{"  DHAKA  ", models.ZoneInsideDhaka},
		{"Dhaka District", models.ZoneInsideDhaka},
		{"dhaka   zila", models.ZoneInsideDhaka},
		{"ঢাকা", models.ZoneInsideDhaka},

		// Dhaka DIVISION districts are outside Dhaka.
		{"Gazipur", models.ZoneOutsideDhaka},
		{"Narayanganj", models.ZoneOutsideDhaka},
		{"Madaripur", models.ZoneOutsideDhaka},
		{"Tangail", models.ZoneOutsideDhaka},
		{"Dhaka Division", models.ZoneOutsideDhaka},

		// Everything else, including junk, is outside.
		{"Chattogram", models.ZoneOutsideDhaka},
		{"Sylhet", models.ZoneOutsideDhaka},
		{"", models.ZoneOutsideDhaka},
		{"Dhakaa", models.ZoneOutsideDhaka},
	}
	for _, c := range cases {
		if got := ZoneFor(c.in); got != c.want {
			t.Errorf("ZoneFor(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestValidCharge(t *testing.T) {
	ok := []float64{0, 1, 60, 120.5, MaxCharge}
	bad := []float64{-1, -0.01, MaxCharge + 1, math.NaN(), math.Inf(1), math.Inf(-1)}
	for _, v := range ok {
		if !ValidCharge(v) {
			t.Errorf("ValidCharge(%v) = false, want true", v)
		}
	}
	for _, v := range bad {
		if ValidCharge(v) {
			t.Errorf("ValidCharge(%v) = true, want false", v)
		}
	}
}

func TestChargesForOverrideWins(t *testing.T) {
	s := &models.DeliverySettings{
		Global: models.DeliveryCharges{InsideDhaka: 60, OutsideDhaka: 120},
		Products: map[string]models.DeliveryCharges{
			"heavy": {InsideDhaka: 100, OutsideDhaka: 200},
			"free":  {InsideDhaka: 0, OutsideDhaka: 0}, // 0 is a real override
		},
	}
	if got := ChargesFor(s, "plain"); got != s.Global {
		t.Errorf("no override: got %+v, want global %+v", got, s.Global)
	}
	if got := ChargesFor(s, "heavy"); got.InsideDhaka != 100 || got.OutsideDhaka != 200 {
		t.Errorf("override: got %+v", got)
	}
	if got := ChargesFor(s, "free"); got.InsideDhaka != 0 || got.OutsideDhaka != 0 {
		t.Errorf("zero override must beat global, got %+v", got)
	}
	if got := ChargesFor(nil, "x"); got != (models.DeliveryCharges{}) {
		t.Errorf("nil settings must be free, got %+v", got)
	}
}

func TestCalculateOneChargePerOrder(t *testing.T) {
	s := &models.DeliverySettings{
		Global: models.DeliveryCharges{InsideDhaka: 60, OutsideDhaka: 120},
		Products: map[string]models.DeliveryCharges{
			"heavy": {InsideDhaka: 100, OutsideDhaka: 200},
		},
	}

	// Three ordinary products: ONE global charge, not three.
	b := Calculate(s, models.ZoneInsideDhaka, []string{"a", "b", "c"}, 3000, 0)
	if b.Charge != 60 || b.Base != 60 || b.Waived {
		t.Errorf("3 plain products inside: %+v, want charge 60", b)
	}

	// Mixed: the highest charge wins, still just one charge.
	b = Calculate(s, models.ZoneInsideDhaka, []string{"a", "heavy"}, 1000, 0)
	if b.Charge != 100 {
		t.Errorf("plain+heavy inside: charge %v, want 100", b.Charge)
	}
	b = Calculate(s, models.ZoneOutsideDhaka, []string{"a", "heavy"}, 1000, 0)
	if b.Charge != 200 {
		t.Errorf("plain+heavy outside: charge %v, want 200", b.Charge)
	}

	// Two variants of the same product are one product.
	b = Calculate(s, models.ZoneOutsideDhaka, []string{"heavy", "heavy"}, 1000, 0)
	if b.Charge != 200 || len(b.PerProduct) != 1 {
		t.Errorf("same product twice: %+v, want charge 200 and 1 entry", b)
	}

	// Per-product charges are reported for the receipt.
	b = Calculate(s, models.ZoneInsideDhaka, []string{"a", "heavy"}, 1000, 0)
	if b.PerProduct["a"] != 60 || b.PerProduct["heavy"] != 100 {
		t.Errorf("PerProduct = %v", b.PerProduct)
	}
}

func TestCalculateZeroOverrideDoesNotLowerOthers(t *testing.T) {
	s := &models.DeliverySettings{
		Global: models.DeliveryCharges{InsideDhaka: 60, OutsideDhaka: 120},
		Products: map[string]models.DeliveryCharges{
			"free": {InsideDhaka: 0, OutsideDhaka: 0},
		},
	}
	// A free product alone ships free.
	if b := Calculate(s, models.ZoneOutsideDhaka, []string{"free"}, 500, 0); b.Charge != 0 {
		t.Errorf("free product alone: charge %v, want 0", b.Charge)
	}
	// Next to a normal product the order still pays the normal charge.
	if b := Calculate(s, models.ZoneOutsideDhaka, []string{"free", "a"}, 500, 0); b.Charge != 120 {
		t.Errorf("free + plain: charge %v, want 120", b.Charge)
	}
}

func TestCalculateFreeDeliveryThreshold(t *testing.T) {
	s := &models.DeliverySettings{
		Global: models.DeliveryCharges{InsideDhaka: 60, OutsideDhaka: 120},
	}

	// Below the threshold: charged.
	b := Calculate(s, models.ZoneOutsideDhaka, []string{"a"}, 1999.99, 2000)
	if b.Charge != 120 || b.Waived {
		t.Errorf("below threshold: %+v, want charge 120 not waived", b)
	}

	// At the threshold: waived, but the base is kept for the receipt.
	b = Calculate(s, models.ZoneOutsideDhaka, []string{"a"}, 2000, 2000)
	if b.Charge != 0 || !b.Waived || b.Base != 120 {
		t.Errorf("at threshold: %+v, want charge 0, waived, base 120", b)
	}

	// Nothing to waive when the charge was already 0.
	free := &models.DeliverySettings{}
	b = Calculate(free, models.ZoneInsideDhaka, []string{"a"}, 5000, 2000)
	if b.Charge != 0 || b.Waived {
		t.Errorf("already free: %+v, want charge 0 and Waived=false", b)
	}

	// No threshold set: never waived.
	b = Calculate(s, models.ZoneInsideDhaka, []string{"a"}, 1e9, 0)
	if b.Charge != 60 || b.Waived {
		t.Errorf("no threshold: %+v, want charge 60", b)
	}
}

func TestCalculateEdgeCases(t *testing.T) {
	s := &models.DeliverySettings{
		Global: models.DeliveryCharges{InsideDhaka: 60, OutsideDhaka: 120},
	}

	// No products: nothing to deliver, nothing to charge.
	b := Calculate(s, models.ZoneInsideDhaka, nil, 0, 0)
	if b.Charge != 0 || b.Base != 0 {
		t.Errorf("no products: %+v, want 0", b)
	}

	// An invalid zone falls back to Outside Dhaka (the safe, higher default).
	b = Calculate(s, models.DeliveryZone("mars"), []string{"a"}, 100, 0)
	if b.Zone != models.ZoneOutsideDhaka || b.Charge != 120 {
		t.Errorf("bad zone: %+v, want outside / 120", b)
	}

	// Money is rounded to 2 decimals.
	odd := &models.DeliverySettings{
		Global: models.DeliveryCharges{InsideDhaka: 59.999, OutsideDhaka: 0},
	}
	b = Calculate(odd, models.ZoneInsideDhaka, []string{"a"}, 100, 0)
	if b.Charge != 60 {
		t.Errorf("rounding: charge %v, want 60", b.Charge)
	}
}

// Legacy must reproduce the pre-Delivery-Settings rule exactly:
// threshold > 0 -> flat 60 below it, free at/above it; no threshold -> free.
func TestLegacyMatchesOldBehaviour(t *testing.T) {
	old := func(threshold, subtotal float64) float64 {
		if threshold <= 0 {
			return 0
		}
		if subtotal >= threshold {
			return 0
		}
		return 60
	}
	for _, threshold := range []float64{0, 500, 2000} {
		for _, subtotal := range []float64{0, 499, 500, 1999.5, 2000, 10000} {
			for _, zone := range []models.DeliveryZone{models.ZoneInsideDhaka, models.ZoneOutsideDhaka} {
				got := Calculate(Legacy(threshold), zone, []string{"a", "b"}, subtotal, threshold).Charge
				if want := old(threshold, subtotal); got != want {
					t.Errorf("threshold=%v subtotal=%v zone=%v: got %v, want %v",
						threshold, subtotal, zone, got, want)
				}
			}
		}
	}
}
