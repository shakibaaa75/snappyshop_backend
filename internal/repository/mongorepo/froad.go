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

// FraudSettings implements repository.FraudSettingsRepository.
type FraudSettings struct{ col *mongo.Collection }

func NewFraudSettings(db *mongo.Database) *FraudSettings {
	return &FraudSettings{col: db.Collection(database.FraudSettingsCollection)}
}

func (r *FraudSettings) Get(ctx context.Context) (*models.FraudSettings, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	var out models.FraudSettings
	err := r.col.FindOne(ctx, bson.D{{Key: "_id", Value: models.FraudSettingsID}}).Decode(&out)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, repository.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (r *FraudSettings) Upsert(ctx context.Context, s *models.FraudSettings) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	s.ID = models.FraudSettingsID
	_, err := r.col.ReplaceOne(ctx, bson.D{{Key: "_id", Value: s.ID}}, s, options.Replace().SetUpsert(true))
	return err
}

// FraudHistory implements repository.FraudHistoryRepository.
type FraudHistory struct {
	customers *mongo.Collection
	events    *mongo.Collection
	checks    *mongo.Collection
}

func NewFraudHistory(db *mongo.Database) *FraudHistory {
	return &FraudHistory{
		customers: db.Collection(database.FraudCustomersCollection),
		events:    db.Collection(database.FraudOrderEventsCollection),
		checks:    db.Collection(database.FraudChecksCollection),
	}
}

func (r *FraudHistory) SwapEvent(ctx context.Context, ev *models.FraudOrderEvent) (*models.FraudOrderEvent, bool, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	// Matches only while the stored record is not newer than ev. With upsert,
	// a newer record makes the insert collide on _id (duplicate key) = stale.
	filter := bson.D{
		{Key: "_id", Value: ev.OrderID},
		{Key: "updatedAt", Value: bson.D{{Key: "$lte", Value: ev.UpdatedAt}}},
	}
	var prev models.FraudOrderEvent
	err := r.events.FindOneAndReplace(ctx, filter, ev,
		options.FindOneAndReplace().SetUpsert(true).SetReturnDocument(options.Before),
	).Decode(&prev)
	switch {
	case errors.Is(err, mongo.ErrNoDocuments):
		return nil, true, nil // first record for this order
	case mongo.IsDuplicateKeyError(err):
		return nil, false, nil // stale
	case err != nil:
		return nil, false, err
	}
	return &prev, true, nil
}

func (r *FraudHistory) RemoveEvent(ctx context.Context, orderID string, at time.Time) (*models.FraudOrderEvent, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	var prev models.FraudOrderEvent
	err := r.events.FindOneAndDelete(ctx, bson.D{
		{Key: "_id", Value: orderID},
		{Key: "updatedAt", Value: bson.D{{Key: "$lte", Value: at}}},
	}).Decode(&prev)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &prev, nil
}

func (r *FraudHistory) AdjustCustomer(ctx context.Context, phone string, d repository.FraudDelta, at time.Time) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	_, err := r.customers.UpdateOne(ctx,
		bson.D{{Key: "_id", Value: phone}},
		bson.D{
			{Key: "$inc", Value: bson.D{
				{Key: "delivered", Value: d.Delivered},
				{Key: "cancelled", Value: d.Cancelled},
				{Key: "failed", Value: d.Failed},
				{Key: "open", Value: d.Open},
			}},
			{Key: "$set", Value: bson.D{{Key: "lastEventAt", Value: at}}},
			{Key: "$setOnInsert", Value: bson.D{{Key: "firstSeenAt", Value: at}}},
		},
		options.Update().SetUpsert(true),
	)
	return err
}

func (r *FraudHistory) GetCustomer(ctx context.Context, phone string) (*models.FraudCustomer, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	var out models.FraudCustomer
	err := r.customers.FindOne(ctx, bson.D{{Key: "_id", Value: phone}}).Decode(&out)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, repository.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (r *FraudHistory) SaveExternal(ctx context.Context, phone string, ext *models.FraudExternal, at time.Time) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	_, err := r.customers.UpdateOne(ctx,
		bson.D{{Key: "_id", Value: phone}},
		bson.D{
			{Key: "$set", Value: bson.D{{Key: "external", Value: ext}}},
			{Key: "$setOnInsert", Value: bson.D{
				{Key: "delivered", Value: 0}, {Key: "cancelled", Value: 0},
				{Key: "failed", Value: 0}, {Key: "open", Value: 0},
				{Key: "firstSeenAt", Value: at}, {Key: "lastEventAt", Value: at},
			}},
		},
		options.Update().SetUpsert(true),
	)
	return err
}

func (r *FraudHistory) DeleteInactiveBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	c, err := r.customers.DeleteMany(ctx, bson.D{{Key: "lastEventAt", Value: bson.D{{Key: "$lt", Value: cutoff}}}})
	if err != nil {
		return 0, err
	}
	e, err := r.events.DeleteMany(ctx, bson.D{{Key: "updatedAt", Value: bson.D{{Key: "$lt", Value: cutoff}}}})
	if err != nil {
		return c.DeletedCount, err
	}
	return c.DeletedCount + e.DeletedCount, nil
}

// FraudOrderSource streams finished orders for the history backfill.
type FraudOrderSource struct{ col *mongo.Collection }

func NewFraudOrderSource(db *mongo.Database) *FraudOrderSource {
	return &FraudOrderSource{col: db.Collection(database.OrdersCollection)}
}

// ForEachTracked calls fn for every order whose status the fraud history
// tracks (confirmed, shipped, delivered, cancelled), oldest first. It stops at
// the first error fn returns.
func (r *FraudOrderSource) ForEachTracked(ctx context.Context, fn func(models.Order) error) error {
	cur, err := r.col.Find(ctx,
		bson.D{{Key: "status", Value: bson.D{{Key: "$in", Value: bson.A{
			string(models.OrderConfirmed), string(models.OrderShipped),
			string(models.OrderDelivered), string(models.OrderCancelled),
		}}}}},
		options.Find().SetSort(bson.D{{Key: "createdAt", Value: 1}}).SetBatchSize(200),
	)
	if err != nil {
		return err
	}
	defer cur.Close(ctx)
	for cur.Next(ctx) {
		var o models.Order
		if err := cur.Decode(&o); err != nil {
			return err
		}
		if err := fn(o); err != nil {
			return err
		}
	}
	return cur.Err()
}

// SaveCheck stores (or replaces) the last check result of an order.
// FraudHistory therefore also implements repository.FraudCheckStore.
func (r *FraudHistory) SaveCheck(ctx context.Context, c *models.FraudCheckSnapshot) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	_, err := r.checks.ReplaceOne(ctx, bson.D{{Key: "_id", Value: c.OrderID}}, c, options.Replace().SetUpsert(true))
	return err
}

func (r *FraudHistory) GetCheck(ctx context.Context, sellerID, orderID string) (*models.FraudCheckSnapshot, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	var out models.FraudCheckSnapshot
	err := r.checks.FindOne(ctx, bson.D{
		{Key: "_id", Value: orderID},
		{Key: "sellerId", Value: sellerID},
	}).Decode(&out)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, repository.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

var _ repository.FraudCheckStore = (*FraudHistory)(nil)
