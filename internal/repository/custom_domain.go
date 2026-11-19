package repository

import (
	"context"
	"errors"

	"shopbd/backend/internal/models"
)

// ErrDomainTaken means another seller already has this domain verified.
var ErrDomainTaken = errors.New("repository: custom domain taken")

// CustomDomainRepository stores each seller's custom domain.
type CustomDomainRepository interface {
	// GetBySeller returns the seller's domain, or ErrNotFound.
	GetBySeller(ctx context.Context, sellerID string) (*models.CustomDomain, error)
	// GetVerifiedByDomain returns the VERIFIED row for a domain, or ErrNotFound.
	GetVerifiedByDomain(ctx context.Context, domain string) (*models.CustomDomain, error)
	// Save creates or replaces the seller's row. It returns ErrDomainTaken when
	// the row is verified and another seller already verified the same domain.
	Save(ctx context.Context, d *models.CustomDomain) error
	// Delete removes the seller's row. Deleting a missing row is not an error.
	Delete(ctx context.Context, sellerID string) error
}
