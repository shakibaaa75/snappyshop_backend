package models

import "time"

// Payment statuses for subscription payments.
const (
	SubPayPending   = "pending"
	SubPayPaid      = "paid"
	SubPayFailed    = "failed"
	SubPayCancelled = "cancelled"
)

// Subscription sources.
const (
	SubSourcePayment   = "payment"    // seller paid through the gateway
	SubSourceAdminFree = "admin_free" // admin granted it for free
	SubSourceAdminPaid = "admin_paid" // admin assigned a paid plan (billed offline)
)

// Subscription statuses. An empty status (rows created before this field
// existed) counts as active.
const (
	SubStatusActive   = "active"
	SubStatusInactive = "inactive" // switched off by an admin; seller is on Free
)

// Subscription is a seller's current paid period. One row per seller.
// A plan applies while Status is active AND CurrentPeriodEnd is in the
// future; otherwise the seller is treated as Free. Nothing has to be updated
// when time passes.
type Subscription struct {
	ID               string    `bson:"_id"`
	SellerID         string    `bson:"sellerId"`
	Plan             string    `bson:"plan"`
	Interval         string    `bson:"interval"`
	CurrentPeriodEnd time.Time `bson:"currentPeriodEnd"`
	UpdatedAt        time.Time `bson:"updatedAt"`
	Status           string    `bson:"status,omitempty"`
	Source           string    `bson:"source,omitempty"`
	GrantedBy        string    `bson:"grantedBy,omitempty"` // admin id
	Note             string    `bson:"note,omitempty"`
	StartedAt        time.Time `bson:"startedAt,omitempty"`
}

// IsActive reports whether the subscription currently grants its plan.
func (s *Subscription) IsActive(now time.Time) bool {
	if s.Status == SubStatusInactive {
		return false
	}
	return s.CurrentPeriodEnd.After(now)
}

// SubscriptionPayment is one checkout attempt. The amount is always set by
// the server from the plan catalog, never taken from the client.
type SubscriptionPayment struct {
	ID       string `bson:"_id"`
	SellerID string `bson:"sellerId"`
	Plan     string `bson:"plan"`
	Interval string `bson:"interval"`
	Method   string `bson:"method"` // bkash | nagad | card (the seller's preference)
	Amount   int64  `bson:"amount"` // taka actually charged (after discounts)
	// Price breakdown, kept so receipts and per-minute refunds are exact.
	// Older rows do not have these fields.
	Months        int       `bson:"months,omitempty"`
	ListAmount    int64     `bson:"listAmount,omitempty"`    // before discounts
	DiscountPct   int       `bson:"discountPct,omitempty"`   // total percent off
	FirstPurchase bool      `bson:"firstPurchase,omitempty"` // first-purchase discount applied
	Status        string    `bson:"status"`
	TranID        string    `bson:"tranId"` // random, sent to the gateway
	Slug          string    `bson:"slug"`   // store slug, used for the redirect back
	ValID         string    `bson:"valId,omitempty"`
	CreatedAt     time.Time `bson:"createdAt"`
	PaidAt        time.Time `bson:"paidAt,omitempty"`
}
