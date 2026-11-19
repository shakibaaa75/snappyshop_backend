package mongorepo

import (
	"context"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"

	"shopbd/backend/internal/database"
)

// ProductCounter counts a seller's products without loading them.
type ProductCounter struct{ col *mongo.Collection }

func NewProductCounter(db *mongo.Database) *ProductCounter {
	return &ProductCounter{col: db.Collection(database.ProductsCollection)}
}

func (c *ProductCounter) CountBySeller(ctx context.Context, sellerID string) (int, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	n, err := c.col.CountDocuments(ctx, bson.D{{Key: "sellerId", Value: sellerID}})
	return int(n), err
}
