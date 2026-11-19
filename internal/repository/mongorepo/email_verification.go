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

// EmailVerifications implements repository.EmailVerificationRepository.
type EmailVerifications struct{ col *mongo.Collection }

func NewEmailVerifications(db *mongo.Database) *EmailVerifications {
	return &EmailVerifications{col: db.Collection(database.EmailVerificationsCollection)}
}

// Replace makes v the only pending code for v.Email and resets its attempts.
func (r *EmailVerifications) Replace(ctx context.Context, v *models.EmailVerification) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	// The filter seeds "email" on insert, so it is not repeated in $setOnInsert.
	_, err := r.col.UpdateOne(ctx,
		bson.D{{Key: "email", Value: v.Email}},
		bson.D{
			{Key: "$set", Value: bson.D{
				{Key: "codeHash", Value: v.CodeHash},
				{Key: "attempts", Value: 0},
				{Key: "sentAt", Value: v.SentAt},
				{Key: "expiresAt", Value: v.ExpiresAt},
			}},
			{Key: "$setOnInsert", Value: bson.D{
				{Key: "_id", Value: v.ID},
				{Key: "createdAt", Value: v.CreatedAt},
			}},
		},
		options.Update().SetUpsert(true),
	)
	return err
}

func (r *EmailVerifications) FindByEmail(ctx context.Context, email string) (*models.EmailVerification, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	var v models.EmailVerification
	err := r.col.FindOne(ctx, bson.D{
		{Key: "email", Value: email},
		{Key: "expiresAt", Value: bson.D{{Key: "$gt", Value: time.Now().UTC()}}},
	}).Decode(&v)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, repository.ErrVerificationNotFound
	}
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// IncrementAttempts atomically counts one verification attempt and returns the
// new total, so parallel guesses cannot slip past the attempt limit.
func (r *EmailVerifications) IncrementAttempts(ctx context.Context, email string) (int, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	var v models.EmailVerification
	err := r.col.FindOneAndUpdate(ctx,
		bson.D{{Key: "email", Value: email}},
		bson.D{{Key: "$inc", Value: bson.D{{Key: "attempts", Value: 1}}}},
		options.FindOneAndUpdate().SetReturnDocument(options.After),
	).Decode(&v)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return 0, repository.ErrVerificationNotFound
	}
	if err != nil {
		return 0, err
	}
	return v.Attempts, nil
}

func (r *EmailVerifications) DeleteByEmail(ctx context.Context, email string) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	_, err := r.col.DeleteMany(ctx, bson.D{{Key: "email", Value: email}})
	return err
}
