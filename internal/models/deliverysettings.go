package models

import "time"

// DeliveryZone says where an order is going, for pricing delivery.
// There are exactly two zones.
type DeliveryZone string

const (
	// ZoneInsideDhaka is the Dhaka DISTRICT only. Gazipur, Narayanganj,
	// Madaripur and every other district in Dhaka Division are NOT inside Dhaka.
	ZoneInsideDhaka DeliveryZone = "inside_dhaka"
	// ZoneOutsideDhaka is every other district.
	ZoneOutsideDhaka DeliveryZone = "outside_dhaka"
)

// Valid reports whether z is one of the two known zones.
func (z DeliveryZone) Valid() bool {
	return z == ZoneInsideDhaka || z == ZoneOutsideDhaka
}

// DeliveryCharges is the delivery charge (BDT) for each zone.
type DeliveryCharges struct {
	InsideDhaka  float64 `bson:"insideDhaka"`
	OutsideDhaka float64 `bson:"outsideDhaka"`
}

// For returns the charge for one zone.
func (c DeliveryCharges) For(z DeliveryZone) float64 {
	if z == ZoneInsideDhaka {
		return c.InsideDhaka
	}
	return c.OutsideDhaka
}

// DeliverySettings is stored in the "delivery_settings" collection, one
// document per seller (the _id is the seller ID).
//
// Global applies to every product. Products holds per-product overrides
// (productId -> charges); a product that is in the map ignores Global, and a
// charge of 0 in an override is a real value ("free delivery for this
// product"), so "no override" is expressed by the key being absent.
//
// A seller with no document has never opened Delivery Settings. Their orders
// keep using the old rule until they save (see delivery.Legacy).
type DeliverySettings struct {
	SellerID  string                     `bson:"_id"`
	Global    DeliveryCharges            `bson:"global"`
	Products  map[string]DeliveryCharges `bson:"products,omitempty"`
	UpdatedAt time.Time                  `bson:"updatedAt"`
}
