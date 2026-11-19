package repository

import (
	"context"
	"errors"
	"time"

	"shopbd/backend/internal/models"
)

// ErrCourierDuplicate means the seller already connected this courier account.
var ErrCourierDuplicate = errors.New("repository: courier account already connected")

// CourierConnectionRepository stores courier connections. Every method takes
// the sellerID and scopes its query by it, so one seller can never read or
// change another seller's connection even if an ID is guessed.
type CourierConnectionRepository interface {
	// Create returns ErrCourierDuplicate if (seller, provider, fingerprint) exists.
	Create(ctx context.Context, c *models.CourierConnection) error
	List(ctx context.Context, sellerID string) ([]*models.CourierConnection, error)
	// FindByID returns ErrNotFound if the connection is not this seller's.
	FindByID(ctx context.Context, sellerID, id string) (*models.CourierConnection, error)
	// SetStatus records a health check result.
	SetStatus(ctx context.Context, sellerID, id string, status models.CourierStatus, message string, at time.Time) error
	// ReplaceCredentials swaps in verified credentials and marks the
	// connection connected. Returns ErrCourierDuplicate or ErrNotFound.
	ReplaceCredentials(ctx context.Context, sellerID, id string, u CourierCredentialUpdate) error
	// Delete removes the connection and its encrypted credentials.
	Delete(ctx context.Context, sellerID, id string) error
}

type CourierCredentialUpdate struct {
	EncryptedCredentials string
	AccountFingerprint   string
	AccountHint          string
	At                   time.Time
}
