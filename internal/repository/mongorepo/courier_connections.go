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

// CourierConnections implements repository.CourierConnectionRepository.
// Every query filters by sellerId, so a guessed _id from another seller
// matches nothing.
type CourierConnections struct{ col *mongo.Collection }

func NewCourierConnections(db *mongo.Database) *CourierConnections {
	return &CourierConnections{col: db.Collection(database.CourierConnectionsCollection)}
}

func (r *CourierConnections) Create(ctx context.Context, c *models.CourierConnection) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	if _, err := r.col.InsertOne(ctx, c); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return repository.ErrCourierDuplicate
		}
		return err
	}
	return nil
}

func (r *CourierConnections) List(ctx context.Context, sellerID string) ([]*models.CourierConnection, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	opts := options.Find().SetSort(bson.D{{Key: "createdAt", Value: 1}})
	cur, err := r.col.Find(ctx, bson.D{{Key: "sellerId", Value: sellerID}}, opts)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var out []*models.CourierConnection
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *CourierConnections) FindByID(ctx context.Context, sellerID, id string) (*models.CourierConnection, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	var c models.CourierConnection
	err := r.col.FindOne(ctx, bson.D{
		{Key: "_id", Value: id},
		{Key: "sellerId", Value: sellerID},
	}).Decode(&c)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, repository.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (r *CourierConnections) SetStatus(ctx context.Context, sellerID, id string, status models.CourierStatus, message string, at time.Time) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	set := bson.D{
		{Key: "status", Value: status},
		{Key: "statusMessage", Value: message},
		{Key: "lastCheckedAt", Value: at},
		{Key: "updatedAt", Value: at},
	}
	res, err := r.col.UpdateOne(ctx,
		bson.D{{Key: "_id", Value: id}, {Key: "sellerId", Value: sellerID}},
		bson.D{{Key: "$set", Value: set}},
	)
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		return repository.ErrNotFound
	}
	return nil
}

func (r *CourierConnections) ReplaceCredentials(ctx context.Context, sellerID, id string, u repository.CourierCredentialUpdate) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	set := bson.D{
		{Key: "encryptedCredentials", Value: u.EncryptedCredentials},
		{Key: "accountFingerprint", Value: u.AccountFingerprint},
		{Key: "accountHint", Value: u.AccountHint},
		{Key: "status", Value: models.CourierConnected},
		{Key: "statusMessage", Value: ""},
		{Key: "lastCheckedAt", Value: u.At},
		{Key: "updatedAt", Value: u.At},
	}
	res, err := r.col.UpdateOne(ctx,
		bson.D{{Key: "_id", Value: id}, {Key: "sellerId", Value: sellerID}},
		bson.D{{Key: "$set", Value: set}},
	)
	if err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return repository.ErrCourierDuplicate
		}
		return err
	}
	if res.MatchedCount == 0 {
		return repository.ErrNotFound
	}
	return nil
}

func (r *CourierConnections) Delete(ctx context.Context, sellerID, id string) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	res, err := r.col.DeleteOne(ctx, bson.D{
		{Key: "_id", Value: id},
		{Key: "sellerId", Value: sellerID},
	})
	if err != nil {
		return err
	}
	if res.DeletedCount == 0 {
		return repository.ErrNotFound
	}
	return nil
}
