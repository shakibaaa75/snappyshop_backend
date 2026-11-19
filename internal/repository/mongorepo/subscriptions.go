package mongorepo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"shopbd/backend/internal/models"
	"shopbd/backend/internal/repository"
)

const (
	subscriptionsCollection        = "subscriptions"
	subscriptionPaymentsCollection = "subscription_payments"
)

// EnsureSubscriptionIndexes creates the indexes both collections rely on.
// Call it from main.go next to the other EnsureIndexes calls.
func EnsureSubscriptionIndexes(ctx context.Context, db *mongo.Database) error {
	if _, err := db.Collection(subscriptionsCollection).Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "sellerId", Value: 1}},
		Options: options.Index().
			SetName("uniq_subscriptions_seller").
			SetUnique(true),
	}); err != nil {
		return fmt.Errorf("subscriptions index: %w", err)
	}

	pays := db.Collection(subscriptionPaymentsCollection)
	if _, err := pays.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "tranId", Value: 1}},
		Options: options.Index().
			SetName("uniq_sub_payments_tran").
			SetUnique(true),
	}); err != nil {
		return fmt.Errorf("subscription_payments tran index: %w", err)
	}
	if _, err := pays.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{
			{Key: "sellerId", Value: 1},
			{Key: "createdAt", Value: -1},
		},
		Options: options.Index().SetName("idx_sub_payments_seller_created"),
	}); err != nil {
		return fmt.Errorf("subscription_payments seller index: %w", err)
	}
	return nil
}

/* ------------------------------------------------------------------ */
/* Subscriptions                                                       */
/* ------------------------------------------------------------------ */

type Subscriptions struct{ col *mongo.Collection }

func NewSubscriptions(db *mongo.Database) *Subscriptions {
	return &Subscriptions{col: db.Collection(subscriptionsCollection)}
}

func (r *Subscriptions) GetBySeller(ctx context.Context, sellerID string) (*models.Subscription, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()

	var s models.Subscription
	err := r.col.FindOne(ctx, bson.D{{Key: "sellerId", Value: sellerID}}).Decode(&s)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, repository.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

func (r *Subscriptions) Upsert(ctx context.Context, s *models.Subscription) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()

	id := s.ID
	if id == "" {
		id = models.NewID()
	}
	filter := bson.D{{Key: "sellerId", Value: s.SellerID}}
	update := bson.D{
		{Key: "$set", Value: bson.D{
			{Key: "plan", Value: s.Plan},
			{Key: "interval", Value: s.Interval},
			{Key: "currentPeriodEnd", Value: s.CurrentPeriodEnd},
			{Key: "updatedAt", Value: s.UpdatedAt},
			{Key: "status", Value: s.Status},
			{Key: "source", Value: s.Source},
			{Key: "grantedBy", Value: s.GrantedBy},
			{Key: "note", Value: s.Note},
			{Key: "startedAt", Value: s.StartedAt},
		}},
		// sellerId comes from the filter on insert.
		{Key: "$setOnInsert", Value: bson.D{{Key: "_id", Value: id}}},
	}

	// Two concurrent first-time upserts can race on the unique index; one retry fixes it.
	var err error
	for i := 0; i < 2; i++ {
		_, err = r.col.UpdateOne(ctx, filter, update, options.Update().SetUpsert(true))
		if !mongo.IsDuplicateKeyError(err) {
			return err
		}
	}
	return err
}

/* ------------------------------------------------------------------ */
/* Subscription payments                                               */
/* ------------------------------------------------------------------ */

type SubscriptionPayments struct{ col *mongo.Collection }

func NewSubscriptionPayments(db *mongo.Database) *SubscriptionPayments {
	return &SubscriptionPayments{col: db.Collection(subscriptionPaymentsCollection)}
}

func (r *SubscriptionPayments) Create(ctx context.Context, p *models.SubscriptionPayment) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	_, err := r.col.InsertOne(ctx, p)
	return err
}

func (r *SubscriptionPayments) FindByTranID(ctx context.Context, tranID string) (*models.SubscriptionPayment, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()

	var p models.SubscriptionPayment
	err := r.col.FindOne(ctx, bson.D{{Key: "tranId", Value: tranID}}).Decode(&p)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, repository.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (r *SubscriptionPayments) MarkPaid(ctx context.Context, tranID, valID string, at time.Time) (bool, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()

	res, err := r.col.UpdateOne(ctx,
		bson.D{
			{Key: "tranId", Value: tranID},
			{Key: "status", Value: bson.D{{Key: "$ne", Value: models.SubPayPaid}}},
		},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "status", Value: models.SubPayPaid},
			{Key: "valId", Value: valID},
			{Key: "paidAt", Value: at},
		}}},
	)
	if err != nil {
		return false, err
	}
	return res.ModifiedCount == 1, nil
}

func (r *SubscriptionPayments) ResetPaid(ctx context.Context, tranID string) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()

	_, err := r.col.UpdateOne(ctx,
		bson.D{
			{Key: "tranId", Value: tranID},
			{Key: "status", Value: models.SubPayPaid},
		},
		bson.D{
			{Key: "$set", Value: bson.D{{Key: "status", Value: models.SubPayPending}}},
			{Key: "$unset", Value: bson.D{{Key: "paidAt", Value: ""}}},
		},
	)
	return err
}

func (r *SubscriptionPayments) MarkNotPaid(ctx context.Context, tranID, status string) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()

	_, err := r.col.UpdateOne(ctx,
		bson.D{
			{Key: "tranId", Value: tranID},
			{Key: "status", Value: models.SubPayPending},
		},
		bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: status}}}},
	)
	return err
}

func (r *SubscriptionPayments) ListBySeller(ctx context.Context, sellerID string, limit int) ([]models.SubscriptionPayment, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	if limit <= 0 {
		limit = 20
	}

	cur, err := r.col.Find(ctx,
		bson.D{{Key: "sellerId", Value: sellerID}},
		options.Find().
			SetSort(bson.D{{Key: "createdAt", Value: -1}}).
			SetLimit(int64(limit)),
	)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)

	var rows []models.SubscriptionPayment
	if err := cur.All(ctx, &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// HasPaid reports whether the seller has at least one paid subscription payment.
func (r *SubscriptionPayments) HasPaid(ctx context.Context, sellerID string) (bool, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	n, err := r.col.CountDocuments(ctx,
		bson.D{
			{Key: "sellerId", Value: sellerID},
			{Key: "status", Value: models.SubPayPaid},
		},
		options.Count().SetLimit(1),
	)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}
