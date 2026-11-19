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

// SubscriptionGrants implements repository.SubscriptionGrantRepository.
type SubscriptionGrants struct{ col *mongo.Collection }

func NewSubscriptionGrants(db *mongo.Database) *SubscriptionGrants {
	return &SubscriptionGrants{col: db.Collection(database.SubscriptionGrantsCollection)}
}

func (r *SubscriptionGrants) Create(ctx context.Context, g *models.SubscriptionGrant) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	_, err := r.col.InsertOne(ctx, g)
	return err
}

func (r *SubscriptionGrants) GetByID(ctx context.Context, id string) (*models.SubscriptionGrant, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	var g models.SubscriptionGrant
	err := r.col.FindOne(ctx, bson.D{{Key: "_id", Value: id}}).Decode(&g)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, repository.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &g, nil
}

func (r *SubscriptionGrants) ListPendingBySeller(ctx context.Context, sellerID string) ([]*models.SubscriptionGrant, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	cur, err := r.col.Find(ctx,
		bson.D{{Key: "sellerId", Value: sellerID}, {Key: "status", Value: models.GrantPending}},
		options.Find().SetSort(bson.D{{Key: "createdAt", Value: -1}}).SetLimit(20))
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	out := []*models.SubscriptionGrant{}
	for cur.Next(ctx) {
		var g models.SubscriptionGrant
		if err := cur.Decode(&g); err != nil {
			return nil, err
		}
		out = append(out, &g)
	}
	return out, cur.Err()
}

func (r *SubscriptionGrants) MarkClaimed(ctx context.Context, id, sellerID string, at time.Time) (bool, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	res, err := r.col.UpdateOne(ctx,
		bson.D{{Key: "_id", Value: id}, {Key: "sellerId", Value: sellerID}, {Key: "status", Value: models.GrantPending}},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "status", Value: models.GrantClaimed},
			{Key: "claimedAt", Value: at},
		}}})
	if err != nil {
		return false, err
	}
	return res.ModifiedCount == 1, nil
}

func (r *SubscriptionGrants) Reopen(ctx context.Context, id string) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	_, err := r.col.UpdateOne(ctx,
		bson.D{{Key: "_id", Value: id}, {Key: "status", Value: models.GrantClaimed}},
		bson.D{
			{Key: "$set", Value: bson.D{{Key: "status", Value: models.GrantPending}}},
			{Key: "$unset", Value: bson.D{{Key: "claimedAt", Value: ""}}},
		})
	return err
}

func (r *SubscriptionGrants) Revoke(ctx context.Context, id, sellerID string, at time.Time) (bool, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	res, err := r.col.UpdateOne(ctx,
		bson.D{{Key: "_id", Value: id}, {Key: "sellerId", Value: sellerID}, {Key: "status", Value: models.GrantPending}},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "status", Value: models.GrantRevoked},
			{Key: "revokedAt", Value: at},
		}}})
	if err != nil {
		return false, err
	}
	return res.ModifiedCount == 1, nil
}
