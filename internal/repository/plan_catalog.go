package repository

import (
	"context"

	"shopbd/backend/internal/models"
)

// PlanCatalogRepository stores the admin-edited plan catalog (one document).
type PlanCatalogRepository interface {
	// Get returns ErrNotFound until an admin has saved the catalog once.
	Get(ctx context.Context) (*models.PlanCatalogDoc, error)
	// Save creates or replaces the catalog document.
	Save(ctx context.Context, doc *models.PlanCatalogDoc) error
}
