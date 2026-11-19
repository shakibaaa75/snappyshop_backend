package models

import "time"

// EmailVerification is a pending sign-up code, stored in the
// "email_verifications" collection. There is at most one per email address.
// Only an HMAC of the code is stored, never the code itself.
type EmailVerification struct {
	ID        string    `bson:"_id"`
	Email     string    `bson:"email"` // always lowercase, trimmed
	CodeHash  string    `bson:"codeHash"`
	Attempts  int       `bson:"attempts"`
	SentAt    time.Time `bson:"sentAt"`
	ExpiresAt time.Time `bson:"expiresAt"`
	CreatedAt time.Time `bson:"createdAt"`
}
