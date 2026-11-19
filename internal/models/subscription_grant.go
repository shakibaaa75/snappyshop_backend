package models

import "time"

// Grant statuses.
const (
	GrantPending = "pending" // offered by an admin, waiting for the seller to claim
	GrantClaimed = "claimed" // seller claimed it; the plan is active
	GrantRevoked = "revoked" // admin withdrew the offer before it was claimed
)

// SubscriptionGrant is a FREE subscription an admin offered to a seller.
// Nothing changes on the seller's plan until the seller claims it.
type SubscriptionGrant struct {
	ID             string     `bson:"_id"`
	SellerID       string     `bson:"sellerId"`
	Plan           string     `bson:"plan"`     // pro | business
	Interval       string     `bson:"interval"` // monthly | yearly (label; used when Days == 0)
	Days           int        `bson:"days"`     // 0 = one interval
	Extend         bool       `bson:"extend"`   // add on top of the seller's active period of the same plan
	Reason         string     `bson:"reason"`   // shown to the seller
	AdminID        string     `bson:"adminId"`
	Status         string     `bson:"status"`
	NotificationID string     `bson:"notificationId,omitempty"`
	CreatedAt      time.Time  `bson:"createdAt"`
	ClaimedAt      *time.Time `bson:"claimedAt,omitempty"`
	RevokedAt      *time.Time `bson:"revokedAt,omitempty"`
}
