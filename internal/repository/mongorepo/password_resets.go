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

type PasswordResets struct{ col *mongo.Collection }

func NewPasswordResets(db *mongo.Database) *PasswordResets {
	return &PasswordResets{col: db.Collection(database.PasswordResetsCollection)}
}

func (r *PasswordResets) Create(ctx context.Context, pr *models.PasswordReset) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	_, err := r.col.InsertOne(ctx, pr)
	return err
}

func (r *PasswordResets) FindByUserID(ctx context.Context, userID string) (*models.PasswordReset, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	var pr models.PasswordReset
	err := r.col.FindOne(ctx, bson.D{
		{Key: "userId", Value: userID},
		{Key: "expiresAt", Value: bson.D{{Key: "$gt", Value: time.Now().UTC()}}},
	}).Decode(&pr)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, repository.ErrResetNotFound
	}
	return &pr, err
}

func (r *PasswordResets) DeleteByUserID(ctx context.Context, userID string) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	_, err := r.col.DeleteMany(ctx, bson.D{{Key: "userId", Value: userID}})
	return err
}

// IncrementAttempts atomically counts one guess and returns the new total.
func (r *PasswordResets) IncrementAttempts(ctx context.Context, id string) (int, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	var pr models.PasswordReset
	err := r.col.FindOneAndUpdate(ctx,
		bson.D{
			{Key: "_id", Value: id},
			{Key: "expiresAt", Value: bson.D{{Key: "$gt", Value: time.Now().UTC()}}},
		},
		bson.D{{Key: "$inc", Value: bson.D{{Key: "attempts", Value: 1}}}},
		options.FindOneAndUpdate().SetReturnDocument(options.After),
	).Decode(&pr)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return 0, repository.ErrResetNotFound
	}
	if err != nil {
		return 0, err
	}
	return pr.Attempts, nil
}

// Consume deletes the challenge; only one concurrent caller sees true.
func (r *PasswordResets) Consume(ctx context.Context, id string) (bool, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	res, err := r.col.DeleteOne(ctx, bson.D{
		{Key: "_id", Value: id},
		{Key: "expiresAt", Value: bson.D{{Key: "$gt", Value: time.Now().UTC()}}},
	})
	if err != nil {
		return false, err
	}
	return res.DeletedCount == 1, nil
}
