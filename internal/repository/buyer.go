package repository

import (
	"context"

	"shopbd/backend/internal/models"
)

// BuyerProfileFields is what a buyer may change on their own profile.
type BuyerProfileFields struct {
	Phone     string
	AvatarURL string
	Bio       string
	Language  string
}

// BuyerRepository stores buyer profiles and their saved addresses.
type BuyerRepository interface {
	// Get returns ErrNotFound when the buyer has not saved anything yet.
	Get(ctx context.Context, userID string) (*models.BuyerProfile, error)
	// SaveProfile creates the profile if needed and replaces the profile fields.
	// It never touches the saved addresses.
	SaveProfile(ctx context.Context, userID string, f BuyerProfileFields) error
	// SaveHomeTheme creates the profile if needed and sets only the home page
	// theme. It never touches the profile fields or the saved addresses.
	SaveHomeTheme(ctx context.Context, userID, theme string) error
	// SaveAddresses creates the profile if needed and replaces the address list.
	SaveAddresses(ctx context.Context, userID string, addrs []models.BuyerAddress) error
}
