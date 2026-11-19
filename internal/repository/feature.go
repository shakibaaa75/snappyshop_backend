package repository

import (
	"context"
	"time"

	"shopbd/backend/internal/models"
)

// FeatureEntitlementRepository stores which one-time features a seller owns.
type FeatureEntitlementRepository interface {
	// Get returns ErrNotFound when the seller has no row for the feature.
	Get(ctx context.Context, sellerID, feature string) (*models.FeatureEntitlement, error)
	// Upsert creates or replaces the (seller, feature) row.
	Upsert(ctx context.Context, e *models.FeatureEntitlement) error
	// ListBySeller returns every feature row the seller has.
	ListBySeller(ctx context.Context, sellerID string) ([]models.FeatureEntitlement, error)
}

// FeaturePaymentRepository stores checkout attempts for one-time features.
type FeaturePaymentRepository interface {
	Create(ctx context.Context, p *models.FeaturePayment) error
	FindByTranID(ctx context.Context, tranID string) (*models.FeaturePayment, error)
	// MarkPaid returns true only for the one caller that made the change, so
	// the redirect and the IPN can both confirm safely.
	MarkPaid(ctx context.Context, tranID, valID string, at time.Time) (bool, error)
	ResetPaid(ctx context.Context, tranID string) error
	MarkNotPaid(ctx context.Context, tranID, status string) error
	ListBySeller(ctx context.Context, sellerID, feature string, limit int) ([]models.FeaturePayment, error)
}
