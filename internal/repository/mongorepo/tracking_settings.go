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

// TrackingSettings implements repository.TrackingSettingsRepository.
type TrackingSettings struct{ col *mongo.Collection }

func NewTrackingSettings(db *mongo.Database) *TrackingSettings {
	return &TrackingSettings{col: db.Collection(database.TrackingSettingsCollection)}
}

// Get loads the settings for exactly one seller (_id == sellerID).
func (r *TrackingSettings) Get(ctx context.Context, sellerID string) (*models.TrackingSettings, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	var out models.TrackingSettings
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
func (r *TrackingSettings) Upsert(ctx context.Context, s *models.TrackingSettings) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	_, err := r.col.ReplaceOne(ctx,
		bson.D{{Key: "_id", Value: s.SellerID}},
		s,
		options.Replace().SetUpsert(true),
	)
	return err
}
