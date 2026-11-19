package repository

import (
	"context"
	"errors"
	"time"

	"shopbd/backend/internal/models"
)

// Errors a look repository returns besides ErrNotFound (look not found).
var (
	ErrLookFull         = errors.New("look already has the maximum number of products")
	ErrLookItemExists   = errors.New("product is already in this look")
	ErrLookItemMissing  = errors.New("product is not in this look")
	ErrLookItemsChanged = errors.New("look products changed, reload and try again")
)

// LookRepository stores Try the Look showcases. Every method is scoped to the
// seller, so one seller can never read or change another seller's look.
//
// AddItem, RemoveItem, ReplaceItem and SetOrder are single atomic updates:
// two tabs or a double click can never push a look past its limit or put the
// same product in twice.
type LookRepository interface {
	Create(ctx context.Context, l *models.Look) error
	// FindByID returns ErrNotFound for a missing look or another seller's look.
	FindByID(ctx context.Context, sellerID, id string) (*models.Look, error)
	// FindPublishedByID returns a published look for the public page.
	FindPublishedByID(ctx context.Context, id string) (*models.Look, error)
	ListBySeller(ctx context.Context, sellerID string) ([]*models.Look, error)
	CountBySeller(ctx context.Context, sellerID string) (int, error)
	// UpdateMeta changes title, gender and model image.
	UpdateMeta(ctx context.Context, sellerID, id, title, gender, modelImageURL string, at time.Time) error
	SetStatus(ctx context.Context, sellerID, id, status string, at time.Time) error
	Delete(ctx context.Context, sellerID, id string) error

	// AddItem appends a product. ErrLookFull / ErrLookItemExists / ErrNotFound.
	AddItem(ctx context.Context, sellerID, id, productID string, max int, at time.Time) error
	// RemoveItem drops a product. ErrLookItemMissing / ErrNotFound.
	RemoveItem(ctx context.Context, sellerID, id, productID string, at time.Time) error
	// ReplaceItem swaps one product for another in the same position.
	// ErrLookItemMissing / ErrLookItemExists / ErrNotFound.
	ReplaceItem(ctx context.Context, sellerID, id, oldProductID, newProductID string, at time.Time) error
	// SetOrder rewrites the order. It only applies if the look still holds
	// exactly these products, otherwise ErrLookItemsChanged.
	SetOrder(ctx context.Context, sellerID, id string, productIDs []string, at time.Time) error
}
