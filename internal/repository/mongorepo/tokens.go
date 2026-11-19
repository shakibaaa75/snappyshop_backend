package mongorepo

import (
	"context"
	"errors"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"

	"shopbd/backend/internal/database"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/repository"
)

// Tokens implements repository.TokenRepository.
type Tokens struct{ col *mongo.Collection }

func NewTokens(db *mongo.Database) *Tokens {
	return &Tokens{col: db.Collection(database.RefreshTokensCollection)}
}

func (r *Tokens) Create(ctx context.Context, t *models.RefreshToken) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	_, err := r.col.InsertOne(ctx, t)
	return err
}

func (r *Tokens) FindByHash(ctx context.Context, hash string) (*models.RefreshToken, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	var t models.RefreshToken
	err := r.col.FindOne(ctx, bson.D{{Key: "tokenHash", Value: hash}}).Decode(&t)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, repository.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// Revoke marks a single token as revoked. It only succeeds if the token is
// still active, so two concurrent refreshes cannot both win.
func (r *Tokens) Revoke(ctx context.Context, id, replacedBy string, at time.Time) (bool, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	res, err := r.col.UpdateOne(ctx,
		bson.D{{Key: "_id", Value: id}, {Key: "revokedAt", Value: nil}},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "revokedAt", Value: at},
			{Key: "replacedBy", Value: replacedBy},
		}}},
	)
	if err != nil {
		return false, err
	}
	return res.ModifiedCount == 1, nil
}

// RevokeFamily revokes every active token in a family (used on reuse detection
// and logout).
func (r *Tokens) RevokeFamily(ctx context.Context, familyID string, at time.Time) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	_, err := r.col.UpdateMany(ctx,
		bson.D{{Key: "familyId", Value: familyID}, {Key: "revokedAt", Value: nil}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "revokedAt", Value: at}}}},
	)
	return err
}

// RevokeAllForUser revokes every active refresh token belonging to userID.
func (r *Tokens) RevokeAllForUser(ctx context.Context, userID string, at time.Time) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	_, err := r.col.UpdateMany(ctx,
		bson.D{{Key: "userId", Value: userID}, {Key: "revokedAt", Value: nil}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "revokedAt", Value: at}}}},
	)
	return err
}
