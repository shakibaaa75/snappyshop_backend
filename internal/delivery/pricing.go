// Package delivery prices the delivery of an order. It is pure: no database,
// no network, no clock. The same functions decide the charge when an order is
// placed and when a price is shown, so the two can never disagree.
//
// The rules, in one place:
//
//  1. The destination is one of two zones. Inside Dhaka means the Dhaka
//     DISTRICT only; every other district is Outside Dhaka.
//  2. Each product has a charge per zone: its own override if the seller set
//     one, otherwise the seller's global charge.
//  3. An order is one parcel from one seller, so it pays ONE delivery charge:
//     the highest charge among the products in it. Adding more products never
//     adds more delivery.
//  4. If the seller offers "free delivery over X" and the subtotal reaches X,
//     the charge becomes 0.
package delivery

import (
	"math"
	"strings"

	"shopbd/backend/internal/models"
)

const (
	// MaxCharge is the largest charge a seller can set, in BDT. It only exists
	// to catch typos such as an extra zero or a pasted phone number.
	MaxCharge = 10000.0

	// LegacyFlatFee is the flat fee the store used before Delivery Settings
	// existed. It only applies to sellers who have not saved settings yet.
	LegacyFlatFee = 60.0
)

// ZoneFor returns the delivery zone for the district the customer chose.
//
// Only the Dhaka district is Inside Dhaka. The match is case- and
// space-insensitive and accepts "Dhaka District" and the Bangla spelling, but
// deliberately NOT "Dhaka Division". Anything unrecognised is Outside Dhaka.
func ZoneFor(district string) models.DeliveryZone {
	switch normalize(district) {
	case "dhaka", "dhaka district", "dhaka zila", "dhaka zilla", "ঢাকা", "ঢাকা জেলা":
		return models.ZoneInsideDhaka
	}
	return models.ZoneOutsideDhaka
}

func normalize(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

// ValidCharge reports whether v is an acceptable charge: a real number from 0
// to MaxCharge. Zero is allowed (free delivery).
func ValidCharge(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v <= MaxCharge
}

// Round rounds a money amount to 2 decimal places.
func Round(v float64) float64 {
	return math.Round(v*100) / 100
}

// ChargesFor returns the charges that apply to one product: its override if
// the seller set one, otherwise the global charges. A nil settings value means
// free delivery.
func ChargesFor(s *models.DeliverySettings, productID string) models.DeliveryCharges {
	if s == nil {
		return models.DeliveryCharges{}
	}
	if c, ok := s.Products[productID]; ok {
		return c
	}
	return s.Global
}

// Legacy returns settings that reproduce the old behaviour exactly: a flat
// LegacyFlatFee when the seller has a "free over X" threshold, otherwise free
// delivery. It is used for sellers who have never saved Delivery Settings, so
// switching this feature on does not change what their customers pay.
func Legacy(freeOver float64) *models.DeliverySettings {
	if freeOver <= 0 {
		return &models.DeliverySettings{}
	}
	return &models.DeliverySettings{
		Global: models.DeliveryCharges{
			InsideDhaka:  LegacyFlatFee,
			OutsideDhaka: LegacyFlatFee,
		},
	}
}

// Breakdown is how a delivery charge was worked out. Orders store it so a
// receipt can always be explained, whatever the seller changes later.
type Breakdown struct {
	// Zone is where the order is going.
	Zone models.DeliveryZone
	// PerProduct is the charge each product would carry in this zone, before
	// the one-charge-per-order rule picks the highest.
	PerProduct map[string]float64
	// Base is the charge for the order before any free-delivery waiver: the
	// highest value in PerProduct (0 when there are no products).
	Base float64
	// Waived is true when the free-delivery threshold removed a charge that
	// would otherwise have applied.
	Waived bool
	// Charge is what the customer pays.
	Charge float64
}

// Calculate prices the delivery of one order.
//
// productIDs lists the products in the order (repeats and variants of the same
// product are fine). subtotal is the product total before delivery. freeOver
// is the seller's "free delivery over X" threshold, or 0 for none.
func Calculate(
	s *models.DeliverySettings,
	zone models.DeliveryZone,
	productIDs []string,
	subtotal, freeOver float64,
) Breakdown {
	if !zone.Valid() {
		zone = models.ZoneOutsideDhaka
	}
	b := Breakdown{
		Zone:       zone,
		PerProduct: make(map[string]float64, len(productIDs)),
	}
	for _, id := range productIDs {
		c := Round(ChargesFor(s, id).For(zone))
		b.PerProduct[id] = c
		if c > b.Base {
			b.Base = c
		}
	}
	b.Charge = b.Base
	if freeOver > 0 && subtotal >= freeOver && b.Base > 0 {
		b.Waived = true
		b.Charge = 0
	}
	return b
}
