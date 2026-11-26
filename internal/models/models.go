// Package models holds the persisted document shapes. Models carry bson tags
// only; they do not import the MongoDB driver.
package models

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

type Role string

const (
	RoleBuyer  Role = "BUYER"
	RoleSeller Role = "SELLER"
	RoleAdmin  Role = "ADMIN"
)

type Status string

const (
	StatusActive    Status = "active"
	StatusSuspended Status = "suspended"
)

// User is stored in the "users" collection.
type User struct {
	ID            string    `bson:"_id" json:"-"`
	Name          string    `bson:"name" json:"-"`
	Email         string    `bson:"email" json:"-"` // always lowercase, trimmed
	EmailVerified bool      `bson:"emailVerified" json:"-"`
	PasswordHash  string    `bson:"passwordHash" json:"-"` // empty for accounts that only use Google / Apple
	GoogleID      string    `bson:"googleId,omitempty" json:"-"`
	AppleID       string    `bson:"appleId,omitempty" json:"-"`
	Role          Role      `bson:"role" json:"-"`
	Status        Status    `bson:"status" json:"-"`
	CreatedAt     time.Time `bson:"createdAt" json:"-"`
	UpdatedAt     time.Time `bson:"updatedAt" json:"-"`

	// Personal contact details, kept apart from anything shown on a public
	// storefront (store contact numbers, WhatsApp, ...). Phone is stored as
	// 01XXXXXXXXX; empty means none. A phone is only PhoneVerified after the
	// owner proved it with a one-time code.
	Phone         string `bson:"phone,omitempty" json:"-"`
	PhoneVerified bool   `bson:"phoneVerified" json:"-"`
	// AvatarURL is the owner's personal profile picture (an R2 image they
	// uploaded). It is not the store logo.
	AvatarURL string `bson:"avatarUrl,omitempty" json:"-"`
}

// Social sign-in providers.
const (
	ProviderGoogle = "google"
	ProviderApple  = "apple"
)

// ProviderField is the users-collection field that holds the provider's
// account ID, or "" for an unknown provider.
func ProviderField(provider string) string {
	switch provider {
	case ProviderGoogle:
		return "googleId"
	case ProviderApple:
		return "appleId"
	}
	return ""
}

// SetProviderID records the provider's account ID on the user (in memory).
func (u *User) SetProviderID(provider, id string) {
	switch provider {
	case ProviderGoogle:
		u.GoogleID = id
	case ProviderApple:
		u.AppleID = id
	}
}

// RefreshToken is stored in the "refresh_tokens" collection. Only a SHA-256
// hash of the token is stored, never the token itself.
type RefreshToken struct {
	ID         string     `bson:"_id"`
	UserID     string     `bson:"userId"`
	TokenHash  string     `bson:"tokenHash"`
	FamilyID   string     `bson:"familyId"`
	ExpiresAt  time.Time  `bson:"expiresAt"`
	RevokedAt  *time.Time `bson:"revokedAt,omitempty"`
	ReplacedBy string     `bson:"replacedBy,omitempty"`
	CreatedAt  time.Time  `bson:"createdAt"`
}

// NewID returns a random 128-bit identifier as 32 hex characters.
func NewID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("models: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}
