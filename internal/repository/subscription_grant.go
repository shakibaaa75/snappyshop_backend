package repository

import (
	"context"
	"time"

	"shopbd/backend/internal/models"
)

// SubscriptionGrantRepository stores free-subscription offers.
type SubscriptionGrantRepository interface {
	Create(ctx context.Context, g *models.SubscriptionGrant) error
	// GetByID returns ErrNotFound when there is no such grant.
	GetByID(ctx context.Context, id string) (*models.SubscriptionGrant, error)
	// ListPendingBySeller returns the seller's unclaimed offers, newest first.
	ListPendingBySeller(ctx context.Context, sellerID string) ([]*models.SubscriptionGrant, error)
	// MarkClaimed flips pending -> claimed for this seller. It returns true
	// only for the one caller that made the change, so a double click or two
	// tabs can never claim the same offer twice.
	MarkClaimed(ctx context.Context, id, sellerID string, at time.Time) (bool, error)
	// Reopen puts a claimed grant back to pending (used if activating the
	// plan failed after it was marked claimed).
	Reopen(ctx context.Context, id string) error
	// Revoke flips pending -> revoked. True only if this call changed it.
	Revoke(ctx context.Context, id, sellerID string, at time.Time) (bool, error)
}
