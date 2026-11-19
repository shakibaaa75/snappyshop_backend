package models

import "time"

// CourierStatus is the health of a seller's courier connection.
type CourierStatus string

const (
	CourierConnected         CourierStatus = "connected"
	CourierFailed            CourierStatus = "failed"             // could not reach the courier / unexpected answer
	CourierNeedsReconnection CourierStatus = "needs_reconnection" // courier rejected the saved credentials
)

// CourierConnection is stored in the "courier_connections" collection.
// One seller can have many connections, across providers.
type CourierConnection struct {
	ID       string `bson:"_id"`
	SellerID string `bson:"sellerId"`
	Provider string `bson:"provider"` // adapter ID, e.g. "steadfast"
	Label    string `bson:"label,omitempty"`

	Status        CourierStatus `bson:"status"`
	StatusMessage string        `bson:"statusMessage,omitempty"`

	// AccountHint is a masked, display-safe label (e.g. "••••1234").
	AccountHint string `bson:"accountHint,omitempty"`
	// AccountFingerprint is a keyed hash of the courier account identity,
	// scoped to the seller and provider. It is used only to stop the same
	// account being connected twice by one seller.
	AccountFingerprint string `bson:"accountFingerprint"`
	// EncryptedCredentials is the sealed credentials JSON. It must never be
	// copied into a DTO, a log line or an error.
	EncryptedCredentials string `bson:"encryptedCredentials"`

	ConnectedAt   time.Time  `bson:"connectedAt"`
	LastCheckedAt *time.Time `bson:"lastCheckedAt,omitempty"`
	CreatedAt     time.Time  `bson:"createdAt"`
	UpdatedAt     time.Time  `bson:"updatedAt"`
}
