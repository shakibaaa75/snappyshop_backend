package models

import "time"

// FraudSettings is the platform-wide fraud-check configuration. It lives in
// the "fraud_settings" collection as ONE document with _id "global", edited
// only by the super admin.
type FraudSettings struct {
	ID string `bson:"_id"` // always FraudSettingsID

	// Enabled is the master switch. When false no seller sees the feature and
	// no order events are recorded.
	Enabled bool `bson:"enabled"`

	// Third-party provider (Fraud BD). The API key is stored sealed with
	// secretbox and is never returned to any client.
	ProviderEnabled bool   `bson:"providerEnabled"`
	ProviderID      string `bson:"providerId,omitempty"` // "fraudbd"
	BaseURL         string `bson:"baseUrl,omitempty"`    // empty = provider default
	APIKeySealed    string `bson:"apiKeySealed,omitempty"`
	// APIUsername is the provider account's username that goes with the key.
	// It identifies the account and is not secret, so it is stored as plain text.
	APIUsername string `bson:"apiUsername,omitempty"`
	// APIKeyHint is the last 4 characters of the key, so the admin can tell
	// which key is saved. Not secret enough to matter on its own.
	APIKeyHint string `bson:"apiKeyHint,omitempty"`

	// MinOrders is how many finished orders our own history needs before we
	// trust it without asking the provider.
	MinOrders int `bson:"minOrders"`
	// CacheDays is how long a provider answer is reused before asking again.
	CacheDays int `bson:"cacheDays"`
	// RetentionDays is how long a customer record is kept after its last
	// activity. Expired records are deleted by the cleanup job.
	RetentionDays int `bson:"retentionDays"`
	// StoreProviderData lets us keep provider answers for reuse. When false
	// the provider is still queried but the answer is not saved.
	StoreProviderData bool `bson:"storeProviderData"`

	UpdatedAt time.Time `bson:"updatedAt"`
	UpdatedBy string    `bson:"updatedBy,omitempty"`
}

const FraudSettingsID = "global"

// DefaultFraudSettings is used until the admin saves anything. Everything is
// off, so deploying this changes nothing.
func DefaultFraudSettings() FraudSettings {
	return FraudSettings{
		ID: FraudSettingsID, MinOrders: 3, CacheDays: 7,
		RetentionDays: 365, StoreProviderData: true, ProviderID: "fraudbd",
	}
}

// FraudCustomer is the platform-wide history of one phone number, stored in
// "fraud_customers" with _id = normalized phone (01XXXXXXXXX).
//
// Counters are platform-wide on purpose: that is what lets a seller see a
// buyer's behaviour with OTHER sellers. They carry no seller or order detail.
type FraudCustomer struct {
	Phone string `bson:"_id"`

	Delivered int `bson:"delivered"`
	Cancelled int `bson:"cancelled"`
	Failed    int `bson:"failed"`
	// Open counts orders accepted/confirmed/shipped but not finished yet.
	Open int `bson:"open"`

	FirstSeenAt time.Time `bson:"firstSeenAt"`
	LastEventAt time.Time `bson:"lastEventAt"`

	// Latest third-party answer, kept for reuse (see FraudSettings.CacheDays).
	External *FraudExternal `bson:"external,omitempty"`
}

// Finished is the number of orders with a known outcome.
func (c *FraudCustomer) Finished() int { return c.Delivered + c.Cancelled + c.Failed }

// FraudExternal is a saved provider answer.
type FraudExternal struct {
	Provider  string    `bson:"provider"`
	Total     int       `bson:"total"`
	Success   int       `bson:"success"`
	Cancel    int       `bson:"cancel"`
	Rating    string    `bson:"rating,omitempty"`    // e.g. good_customer (Pathao)
	RiskLevel string    `bson:"riskLevel,omitempty"` // low|medium|high|very_high|unknown
	CheckedAt time.Time `bson:"checkedAt"`
}

// FraudOutcome is what happened to an order, as far as fraud history cares.
type FraudOutcome string

const (
	FraudOpen      FraudOutcome = "open"      // accepted / confirmed / shipped
	FraudDelivered FraudOutcome = "delivered" // delivered / completed
	FraudCancelled FraudOutcome = "cancelled"
	FraudFailed    FraudOutcome = "failed" // failed delivery / returned
)

// FraudOrderEvent is stored in "fraud_order_events" with _id = orderID, so an
// order is counted once and a status change MOVES it between counters rather
// than adding a second count. Only the order's current outcome is kept.
type FraudOrderEvent struct {
	OrderID   string       `bson:"_id"`
	Phone     string       `bson:"phone"`
	SellerID  string       `bson:"sellerId"`
	Outcome   FraudOutcome `bson:"outcome"`
	UpdatedAt time.Time    `bson:"updatedAt"`
}

// FraudCheckSnapshot is the last fraud check a seller ran on one order, kept
// in "fraud_order_checks" with _id = orderID so reopening the order shows it
// again without spending provider quota. Only counts and the verdict are kept.
type FraudCheckSnapshot struct {
	OrderID  string `bson:"_id"`
	SellerID string `bson:"sellerId"`
	Phone    string `bson:"phone"`

	Internal       *FraudCounts   `bson:"internal,omitempty"`
	External       *FraudExternal `bson:"external,omitempty"`
	ExternalStatus string         `bson:"externalStatus,omitempty"`

	RiskLevel   string   `bson:"riskLevel"`
	Sample      int      `bson:"sample"`
	SuccessRate *float64 `bson:"successRate,omitempty"`
	Reasons     []string `bson:"reasons,omitempty"`

	CheckedAt time.Time `bson:"checkedAt"`
}

// FraudCounts are a customer's platform-wide counters at check time.
type FraudCounts struct {
	Delivered int `bson:"delivered"`
	Cancelled int `bson:"cancelled"`
	Failed    int `bson:"failed"`
	Open      int `bson:"open"`
}
