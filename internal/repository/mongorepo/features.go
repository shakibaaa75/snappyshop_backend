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

/* ------------------------------------------------------------------ */
/* Feature entitlements                                                */
/* ------------------------------------------------------------------ */

type FeatureEntitlements struct{ col *mongo.Collection }

func NewFeatureEntitlements(db *mongo.Database) *FeatureEntitlements {
	return &FeatureEntitlements{col: db.Collection(database.FeatureEntitlementsCollection)}
}

func (r *FeatureEntitlements) Get(ctx context.Context, sellerID, feature string) (*models.FeatureEntitlement, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()

	var e models.FeatureEntitlement
	err := r.col.FindOne(ctx, bson.D{
		{Key: "sellerId", Value: sellerID},
		{Key: "feature", Value: feature},
	}).Decode(&e)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, repository.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

func (r *FeatureEntitlements) Upsert(ctx context.Context, e *models.FeatureEntitlement) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()

	id := e.ID
	if id == "" {
		id = models.NewID()
	}
	set := bson.D{
		{Key: "status", Value: e.Status},
		{Key: "source", Value: e.Source},
		{Key: "amount", Value: e.Amount},
		{Key: "grantedBy", Value: e.GrantedBy},
		{Key: "note", Value: e.Note},
		{Key: "purchasedAt", Value: e.PurchasedAt},
		{Key: "updatedAt", Value: e.UpdatedAt},
	}
	update := bson.D{
		{Key: "$set", Value: set},
		// sellerId and feature come from the filter on insert.
		{Key: "$setOnInsert", Value: bson.D{
			{Key: "_id", Value: id},
			{Key: "createdAt", Value: e.CreatedAt},
		}},
	}
	filter := bson.D{
		{Key: "sellerId", Value: e.SellerID},
		{Key: "feature", Value: e.Feature},
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

func (r *FeatureEntitlements) ListBySeller(ctx context.Context, sellerID string) ([]models.FeatureEntitlement, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()

	cur, err := r.col.Find(ctx, bson.D{{Key: "sellerId", Value: sellerID}})
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)

	var rows []models.FeatureEntitlement
	if err := cur.All(ctx, &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

/* ------------------------------------------------------------------ */
/* Feature payments                                                    */
/* ------------------------------------------------------------------ */

type FeaturePayments struct{ col *mongo.Collection }

func NewFeaturePayments(db *mongo.Database) *FeaturePayments {
	return &FeaturePayments{col: db.Collection(database.FeaturePaymentsCollection)}
}

func (r *FeaturePayments) Create(ctx context.Context, p *models.FeaturePayment) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	_, err := r.col.InsertOne(ctx, p)
	return err
}

func (r *FeaturePayments) FindByTranID(ctx context.Context, tranID string) (*models.FeaturePayment, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()

	var p models.FeaturePayment
	err := r.col.FindOne(ctx, bson.D{{Key: "tranId", Value: tranID}}).Decode(&p)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, repository.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (r *FeaturePayments) MarkPaid(ctx context.Context, tranID, valID string, at time.Time) (bool, error) {
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

func (r *FeaturePayments) ResetPaid(ctx context.Context, tranID string) error {
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

func (r *FeaturePayments) MarkNotPaid(ctx context.Context, tranID, status string) error {
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

func (r *FeaturePayments) ListBySeller(ctx context.Context, sellerID, feature string, limit int) ([]models.FeaturePayment, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	if limit <= 0 {
		limit = 20
	}
	filter := bson.D{{Key: "sellerId", Value: sellerID}}
	if feature != "" {
		filter = append(filter, bson.E{Key: "feature", Value: feature})
	}
	cur, err := r.col.Find(ctx, filter,
		options.Find().
			SetSort(bson.D{{Key: "createdAt", Value: -1}}).
			SetLimit(int64(limit)),
	)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)

	var rows []models.FeaturePayment
	if err := cur.All(ctx, &rows); err != nil {
		return nil, err
	}
	return rows, nil
}
