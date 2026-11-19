package mongorepo

import (
	"context"
	"errors"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"shopbd/backend/internal/database"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/repository"
)

// DeliverySettings implements repository.DeliverySettingsRepository.
type DeliverySettings struct{ col *mongo.Collection }

func NewDeliverySettings(db *mongo.Database) *DeliverySettings {
	return &DeliverySettings{col: db.Collection(database.DeliverySettingsCollection)}
}

// Get loads the settings for exactly one seller (_id == sellerID).
func (r *DeliverySettings) Get(ctx context.Context, sellerID string) (*models.DeliverySettings, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	var out models.DeliverySettings
	err := r.col.FindOne(ctx, bson.D{{Key: "_id", Value: sellerID}}).Decode(&out)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, repository.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// Upsert replaces the seller's settings document.
func (r *DeliverySettings) Upsert(ctx context.Context, s *models.DeliverySettings) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	_, err := r.col.ReplaceOne(ctx,
		bson.D{{Key: "_id", Value: s.SellerID}},
		s,
		options.Replace().SetUpsert(true),
	)
	return err
}
