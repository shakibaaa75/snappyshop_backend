package models

import "time"

// Analytics event kinds stored in the "analytics_events" collection.
const (
	AnalyticsEventProductView = "product_view"
	AnalyticsEventAddToCart   = "add_to_cart"
	AnalyticsEventSearch      = "search"
	AnalyticsEventTraffic     = "traffic" // a store visit
	AnalyticsEventClick       = "product_click"
	// AnalyticsEventPurchase is written by the server when an order is placed.
	// It is never accepted from the public track endpoint, so it cannot be forged.
	AnalyticsEventPurchase = "purchase"
)

// AnalyticsEvent is a single tracked interaction against a seller's store.
// One document per event keeps ingestion simple; overview queries aggregate.
type AnalyticsEvent struct {
	ID        string    `bson:"_id"`
	SellerID  string    `bson:"sellerId"`
	Kind      string    `bson:"kind"` // product_view | product_click | add_to_cart | search | traffic | purchase
	ProductID string    `bson:"productId,omitempty"`
	Term      string    `bson:"term,omitempty"`   // search keyword
	Source    string    `bson:"source,omitempty"` // normalised traffic source key, e.g. "direct", "facebook"
	CreatedAt time.Time `bson:"createdAt"`

	// Visitor is a one-way hash of the visitor (see tracking.VisitorHash). It
	// lets the overview count unique visitors without storing any raw ID, IP
	// or user agent. Different for every seller.
	Visitor string `bson:"visitor,omitempty"`
	// OrderID and Value are set on purchase events only (Value = order total).
	OrderID string  `bson:"orderId,omitempty"`
	Value   float64 `bson:"value,omitempty"`

	// Dedupe is a hash of seller + kind + product/source + visitor + UTC day.
	// A unique partial index on it makes the same device count once per day.
	// Empty for search events, which are never deduplicated.
	Dedupe string `bson:"dedupe,omitempty"`
}
