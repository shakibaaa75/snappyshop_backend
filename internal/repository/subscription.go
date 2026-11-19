package repository

import (
	"context"
	"time"

	"shopbd/backend/internal/models"
)

type SubscriptionRepository interface {
	// GetBySeller returns ErrNotFound if the seller has never paid.
	GetBySeller(ctx context.Context, sellerID string) (*models.Subscription, error)
	// Upsert creates or updates the seller's subscription row.
	Upsert(ctx context.Context, s *models.Subscription) error
}

type SubscriptionPaymentRepository interface {
	Create(ctx context.Context, p *models.SubscriptionPayment) error
	// FindByTranID returns ErrNotFound if there is no such payment.
	FindByTranID(ctx context.Context, tranID string) (*models.SubscriptionPayment, error)
	// MarkPaid sets the payment to paid unless it already is. It returns true
	// only for the one caller that made the change, which makes confirming
	// safe to run from both the redirect and the IPN.
	MarkPaid(ctx context.Context, tranID, valID string, at time.Time) (bool, error)
	// ResetPaid puts a paid payment back to pending (used if activating the
	// plan failed after marking it paid, so a later callback can retry).
	ResetPaid(ctx context.Context, tranID string) error
	// MarkNotPaid sets a still-pending payment to failed or cancelled.
	MarkNotPaid(ctx context.Context, tranID, status string) error
	ListBySeller(ctx context.Context, sellerID string, limit int) ([]models.SubscriptionPayment, error)
	// HasPaid reports whether the seller has ever completed a paid purchase.
	// It decides who gets the first-purchase discount.
	HasPaid(ctx context.Context, sellerID string) (bool, error)
}
