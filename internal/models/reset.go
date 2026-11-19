package models

import "time"

// PasswordReset is one reset challenge: a single emailed code for one user,
// stored in the "password_resets" collection. There is at most one per user.
// Only an HMAC of the code is stored, never the code itself.
type PasswordReset struct {
	ID        string    `bson:"_id"`
	UserID    string    `bson:"userId"`
	CodeHash  string    `bson:"codeHash"`
	Attempts  int       `bson:"attempts"` // guesses made against this challenge
	ExpiresAt time.Time `bson:"expiresAt"`
	CreatedAt time.Time `bson:"createdAt"`
}
