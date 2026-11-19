package mongorepo

import (
	"context"
	"errors"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"shopbd/backend/internal/database"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/repository"
)

// Buyers implements repository.BuyerRepository.
type Buyers struct{ db *mongo.Database }

func NewBuyers(db *mongo.Database) *Buyers { return &Buyers{db: db} }

func (r *Buyers) col() *mongo.Collection { return r.db.Collection(database.BuyerProfilesCollection) }

func (r *Buyers) Get(ctx context.Context, userID string) (*models.BuyerProfile, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	var p models.BuyerProfile
	err := r.col().FindOne(ctx, bson.D{{Key: "_id", Value: userID}}).Decode(&p)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, repository.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (r *Buyers) SaveProfile(ctx context.Context, userID string, f repository.BuyerProfileFields) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	now := time.Now().UTC()
	_, err := r.col().UpdateOne(ctx,
		bson.D{{Key: "_id", Value: userID}},
		bson.D{
			{Key: "$set", Value: bson.D{
				{Key: "phone", Value: f.Phone},
				{Key: "avatarUrl", Value: f.AvatarURL},
				{Key: "bio", Value: f.Bio},
				{Key: "language", Value: f.Language},
				{Key: "updatedAt", Value: now},
			}},
			{Key: "$setOnInsert", Value: bson.D{
				{Key: "addresses", Value: bson.A{}},
				{Key: "createdAt", Value: now},
			}},
		},
		options.Update().SetUpsert(true))
	return err
}

func (r *Buyers) SaveAddresses(ctx context.Context, userID string, addrs []models.BuyerAddress) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	if addrs == nil {
		addrs = []models.BuyerAddress{}
	}
	now := time.Now().UTC()
	_, err := r.col().UpdateOne(ctx,
		bson.D{{Key: "_id", Value: userID}},
		bson.D{
			{Key: "$set", Value: bson.D{
				{Key: "addresses", Value: addrs},
				{Key: "updatedAt", Value: now},
			}},
			{Key: "$setOnInsert", Value: bson.D{{Key: "createdAt", Value: now}}},
		},
		options.Update().SetUpsert(true))
	return err
}

func (r *Buyers) SaveHomeTheme(ctx context.Context, userID, theme string) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	now := time.Now().UTC()
	_, err := r.col().UpdateOne(ctx,
		bson.D{{Key: "_id", Value: userID}},
		bson.D{
			{Key: "$set", Value: bson.D{
				{Key: "homeTheme", Value: theme},
				{Key: "updatedAt", Value: now},
			}},
			{Key: "$setOnInsert", Value: bson.D{
				{Key: "addresses", Value: bson.A{}},
				{Key: "createdAt", Value: now},
			}},
		},
		options.Update().SetUpsert(true))
	return err
}
