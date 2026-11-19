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

// Shipments implements repository.ShipmentRepository.
type Shipments struct{ col *mongo.Collection }

func NewShipments(db *mongo.Database) *Shipments {
	return &Shipments{col: db.Collection(database.ShipmentsCollection)}
}

func (r *Shipments) Claim(ctx context.Context, s *models.Shipment) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	if _, err := r.col.InsertOne(ctx, s); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return repository.ErrShipmentExists
		}
		return err
	}
	return nil
}

func (r *Shipments) FindByOrder(ctx context.Context, sellerID, orderID string) (*models.Shipment, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	var s models.Shipment
	err := r.col.FindOne(ctx, bson.D{{Key: "sellerId", Value: sellerID}, {Key: "orderId", Value: orderID}}).Decode(&s)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, repository.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

func (r *Shipments) MarkBooked(ctx context.Context, sellerID, id, consignmentID, trackingCode, trackingURL string, at time.Time) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	res, err := r.col.UpdateOne(ctx,
		bson.D{{Key: "_id", Value: id}, {Key: "sellerId", Value: sellerID}},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "state", Value: models.ShipmentBooked},
			{Key: "consignmentId", Value: consignmentID},
			{Key: "trackingCode", Value: trackingCode},
			{Key: "trackingUrl", Value: trackingURL},
			{Key: "updatedAt", Value: at},
		}}})
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		return repository.ErrNotFound
	}
	return nil
}

func (r *Shipments) Release(ctx context.Context, sellerID, id string) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	_, err := r.col.DeleteOne(ctx, bson.D{
		{Key: "_id", Value: id}, {Key: "sellerId", Value: sellerID}, {Key: "state", Value: models.ShipmentPending},
	})
	return err
}

func (r *Shipments) ListBooked(ctx context.Context, sellerID string, limit int) ([]*models.Shipment, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	opts := options.Find().SetSort(bson.D{{Key: "createdAt", Value: -1}}).SetLimit(int64(limit))
	cur, err := r.col.Find(ctx, bson.D{{Key: "sellerId", Value: sellerID}, {Key: "state", Value: models.ShipmentBooked}}, opts)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var out []*models.Shipment
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}
