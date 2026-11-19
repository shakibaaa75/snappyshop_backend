package models

import "time"

// Premium features sold once (not a subscription). Add new keys here.
const (
	FeatureTryTheLook = "try_the_look"
)

// ValidFeature reports whether key is a known one-time feature.
func ValidFeature(key string) bool { return key == FeatureTryTheLook }

// Feature entitlement statuses.
const (
	FeatureActive   = "active"   // seller can use the feature
	FeatureDisabled = "disabled" // switched off by an admin, can be switched back on
	FeatureRevoked  = "revoked"  // access withdrawn by an admin
)

// Feature entitlement sources.
const (
	FeatureSourcePurchase   = "purchased"     // seller paid through the gateway
	FeatureSourceAdminGrant = "admin_granted" // admin gave it for free
)

// FeatureEntitlement is the permanent "this seller owns this feature" record.
// One row per (seller, feature). It never expires; only an admin can change it.
type FeatureEntitlement struct {
	ID          string     `bson:"_id"`
	SellerID    string     `bson:"sellerId"`
	Feature     string     `bson:"feature"`
	Status      string     `bson:"status"`
	Source      string     `bson:"source"`
	Amount      int64      `bson:"amount,omitempty"` // taka paid (0 for admin grants)
	GrantedBy   string     `bson:"grantedBy,omitempty"`
	Note        string     `bson:"note,omitempty"`
	PurchasedAt *time.Time `bson:"purchasedAt,omitempty"`
	CreatedAt   time.Time  `bson:"createdAt"`
	UpdatedAt   time.Time  `bson:"updatedAt"`
}

// IsActive reports whether the seller may use the feature right now.
func (e *FeatureEntitlement) IsActive() bool { return e != nil && e.Status == FeatureActive }

// FeaturePayment is one checkout attempt for a one-time feature. The amount is
// always set by the server, never taken from the client.
type FeaturePayment struct {
	ID        string    `bson:"_id"`
	SellerID  string    `bson:"sellerId"`
	Feature   string    `bson:"feature"`
	Method    string    `bson:"method"` // bkash | nagad | card
	Amount    int64     `bson:"amount"` // taka
	Status    string    `bson:"status"` // reuses SubPay* values
	TranID    string    `bson:"tranId"`
	Slug      string    `bson:"slug"`
	ValID     string    `bson:"valId,omitempty"`
	CreatedAt time.Time `bson:"createdAt"`
	PaidAt    time.Time `bson:"paidAt,omitempty"`
}
