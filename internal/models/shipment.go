package models

import "time"

// Shipment states. A row is "pending" from the moment we claim the order
// until the courier confirms the parcel, so two clicks (or two tabs) can never
// book the same order twice.
const (
	ShipmentPending = "pending"
	ShipmentBooked  = "booked"
)

// Shipment is stored in the "shipments" collection: one per order.
type Shipment struct {
	ID           string `bson:"_id"`
	SellerID     string `bson:"sellerId"`
	OrderID      string `bson:"orderId"`
	OrderNumber  string `bson:"orderNumber"`
	ConnectionID string `bson:"connectionId"`
	Provider     string `bson:"provider"`
	State        string `bson:"state"`

	ConsignmentID string `bson:"consignmentId,omitempty"`
	TrackingCode  string `bson:"trackingCode,omitempty"`
	TrackingURL   string `bson:"trackingUrl,omitempty"`

	CreatedAt time.Time `bson:"createdAt"`
	UpdatedAt time.Time `bson:"updatedAt"`
}
