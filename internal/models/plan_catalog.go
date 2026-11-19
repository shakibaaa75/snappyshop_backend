package models

import (
	"time"

	"shopbd/backend/internal/plans"
)

// PlanCatalogID is the _id of the one stored catalog document.
const PlanCatalogID = "catalog"

// PlanCatalogDoc is the plan prices, limits, feature switches and discounts an
// admin saved. There is a single document. Until it exists the built-in plans
// in the plans package are used.
type PlanCatalogDoc struct {
	ID        string       `bson:"_id"`
	Config    plans.Config `bson:"config"`
	UpdatedAt time.Time    `bson:"updatedAt"`
	UpdatedBy string       `bson:"updatedBy,omitempty"` // admin id
}
