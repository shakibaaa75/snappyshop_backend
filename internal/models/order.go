package models

import "time"

// OrderStatus is the lifecycle of an order. The seller moves it forward.
type OrderStatus string

const (
	OrderPending   OrderStatus = "pending"   // just placed, not yet confirmed
	OrderConfirmed OrderStatus = "confirmed" // seller accepted it
	OrderShipped   OrderStatus = "shipped"   // handed to courier
	OrderDelivered OrderStatus = "delivered" // customer received it
	OrderCancelled OrderStatus = "cancelled" // cancelled by seller or buyer
)

// PaymentMethod mirrors the frontend payment options.
type PaymentMethod string

const (
	PayBkash  PaymentMethod = "bkash"
	PayNagad  PaymentMethod = "nagad"
	PayRocket PaymentMethod = "rocket"
	PayCOD    PaymentMethod = "cod"
	PayCard   PaymentMethod = "card"
)

// OrderLine is one product in the order. Price and name are snapshotted
// at purchase time so later edits to the product don't rewrite history.
type OrderLine struct {
	ProductID string  `bson:"productId"`
	Name      string  `bson:"name"`
	ImageURL  string  `bson:"imageUrl,omitempty"`
	Emoji     string  `bson:"emoji,omitempty"`
	Hue       int     `bson:"hue,omitempty"`
	VariantID string  `bson:"variantId,omitempty"`
	Variant   string  `bson:"variant,omitempty"` // "Sky Blue / M"
	Price     float64 `bson:"price"`
	Qty       int     `bson:"qty"`
	Subtotal  float64 `bson:"subtotal"`
}

// OrderStatusEvent records a status change (audit trail).
type OrderStatusEvent struct {
	Status OrderStatus `bson:"status"`
	At     time.Time   `bson:"at"`
	Note   string      `bson:"note,omitempty"`
}

// Order is stored in the "orders" collection.
type Order struct {
	ID       string `bson:"_id"` // UUIDv7 (time-ordered); orders before the change have 32-hex IDs
	SellerID string `bson:"sellerId"`
	// Number is the human-readable order number shown to customers and
	// sellers, e.g. SNAPPY-20261115-000123 (prefix, local date, then a
	// database-issued sequence). Older orders keep their SB-YYYYMMDD-XXXX number.
	Number string `bson:"number"`

	// Customer (guest checkout allowed)
	CustomerName    string `bson:"customerName"`
	CustomerPhone   string `bson:"customerPhone"`
	CustomerEmail   string `bson:"customerEmail,omitempty"`
	ShippingAddress string `bson:"shippingAddress"`
	ShippingCity    string `bson:"shippingCity"`
	ShippingArea    string `bson:"shippingArea,omitempty"`
	Notes           string `bson:"notes,omitempty"`

	// Payment
	PaymentMethod PaymentMethod `bson:"paymentMethod"`
	Paid          bool          `bson:"paid"`

	// Money (all in BDT, formatted on the client)
	Subtotal float64 `bson:"subtotal"`
	Delivery float64 `bson:"delivery"`
	// Discount is an order-level amount taken off the total (BDT, never
	// negative). Nothing sets it yet, so it is 0 on every order; Total is
	// always Subtotal + Delivery - Discount.
	Discount float64 `bson:"discount,omitempty"`
	Total    float64 `bson:"total"`

	// How Delivery was worked out, so a receipt can always be explained
	// whatever the seller changes later. Orders placed before Delivery
	// Settings existed have none of these (zone empty).
	//
	// DeliveryZone is where the order went. DeliveryBase is the charge before
	// any "free delivery over X" waiver; DeliveryWaived is true when that
	// waiver brought Delivery down to 0.
	DeliveryZone   DeliveryZone `bson:"deliveryZone,omitempty"`
	DeliveryBase   float64      `bson:"deliveryBase,omitempty"`
	DeliveryWaived bool         `bson:"deliveryWaived,omitempty"`

	// Contents
	Lines []OrderLine `bson:"lines"`

	// Inventory. StockReserved is true while this order is holding units of
	// stock (they were taken off the products when the order was placed). It
	// is what makes release idempotent: stock is returned only when the flag
	// flips true -> false, so a double cancel can never restock twice.
	// Orders created before inventory tracking existed have it false and are
	// never restocked (their stock was never reduced).
	StockReserved bool `bson:"stockReserved"`
	// ReservationExpiresAt is set for orders that must be paid online
	// (bKash / Nagad / Rocket / card). If the order is still pending and
	// unpaid at this time it is auto-cancelled and the stock is released.
	// Nil for cash on delivery and for orders without a payment deadline.
	ReservationExpiresAt *time.Time `bson:"reservationExpiresAt,omitempty"`

	// Status
	Status OrderStatus        `bson:"status"`
	Events []OrderStatusEvent `bson:"events"`

	CreatedAt time.Time `bson:"createdAt"`
	UpdatedAt time.Time `bson:"updatedAt"`
}
