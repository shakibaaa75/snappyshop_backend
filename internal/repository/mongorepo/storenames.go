package mongorepo

import (
	"context"
	"strings"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"

	"shopbd/backend/internal/database"
)

// StoreNames looks up a seller's shop name (used to record which store
// created a new category).
type StoreNames struct{ col *mongo.Collection }

func NewStoreNames(db *mongo.Database) *StoreNames {
	return &StoreNames{col: db.Collection(database.StoreDesignsCollection)}
}

// NameFor returns the shop name, falling back to the store URL slug, then "".
// It never fails: a missing name must not block a product from saving.
func (r *StoreNames) NameFor(ctx context.Context, sellerID string) string {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	var row struct {
		StoreName string `bson:"storeName"`
		Slug      string `bson:"slug"`
	}
	if err := r.col.FindOne(ctx, bson.D{{Key: "sellerId", Value: sellerID}}).Decode(&row); err != nil {
		return ""
	}
	if n := strings.TrimSpace(row.StoreName); n != "" {
		return n
	}
	return row.Slug
}
