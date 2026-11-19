package models

import "time"

// Kinds of problem a buyer can report on an order.
const (
	IssueNotReceived = "not_received"
	IssueWrongItem   = "wrong_item"
	IssueDamaged     = "damaged"
	IssueMissingItem = "missing_item"
	IssueRefund      = "refund_request"
	IssueOther       = "other"
)

// Issue states the buyer sees.
const (
	IssueOpen      = "open"      // reported, waiting for the seller
	IssueResolved  = "resolved"  // the seller (or admin) closed it as solved
	IssueRejected  = "rejected"  // the seller (or admin) closed it without a fix
	IssueWithdrawn = "withdrawn" // the buyer took it back
)

// OrderIssue is stored in "order_issues": one "Problem with this order" report.
type OrderIssue struct {
	ID          string     `bson:"_id"`
	OrderID     string     `bson:"orderId"`
	SellerID    string     `bson:"sellerId"`
	BuyerID     string     `bson:"buyerId"`
	OrderNumber string     `bson:"orderNumber"`
	Type        string     `bson:"type"`
	Message     string     `bson:"message"`
	Status      string     `bson:"status"`
	CreatedAt   time.Time  `bson:"createdAt"`
	UpdatedAt   time.Time  `bson:"updatedAt"`
	ClosedAt    *time.Time `bson:"closedAt,omitempty"`
}
