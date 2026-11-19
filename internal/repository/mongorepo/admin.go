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

type AdminActivityLogs struct{ col *mongo.Collection }

func NewAdminActivityLogs(db *mongo.Database) *AdminActivityLogs {
	return &AdminActivityLogs{col: db.Collection(database.AdminActivityLogsCollection)}
}

func (r *AdminActivityLogs) Create(ctx context.Context, log *models.AdminActivityLog) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	_, err := r.col.InsertOne(ctx, log)
	return err
}

func (r *AdminActivityLogs) List(ctx context.Context, filter repository.AdminLogFilter) ([]*models.AdminActivityLog, int64, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	q := bson.D{}
	if filter.AdminID != "" {
		q = append(q, bson.E{Key: "adminId", Value: filter.AdminID})
	}
	if filter.TargetType != "" {
		q = append(q, bson.E{Key: "targetType", Value: filter.TargetType})
	}
	if filter.TargetID != "" {
		q = append(q, bson.E{Key: "targetId", Value: filter.TargetID})
	}
	if filter.Action != "" {
		q = append(q, bson.E{Key: "action", Value: filter.Action})
	}
	total, err := r.col.CountDocuments(ctx, q)
	if err != nil {
		return nil, 0, err
	}
	limit := filter.Limit
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	skip := filter.Skip
	if skip < 0 {
		skip = 0
	}
	opts := options.Find().SetSort(bson.D{{Key: "createdAt", Value: -1}}).SetLimit(int64(limit)).SetSkip(int64(skip))
	cur, err := r.col.Find(ctx, q, opts)
	if err != nil {
		return nil, 0, err
	}
	defer cur.Close(ctx)
	var out []*models.AdminActivityLog
	for cur.Next(ctx) {
		var item models.AdminActivityLog
		if err := cur.Decode(&item); err != nil {
			return nil, 0, err
		}
		out = append(out, &item)
	}
	return out, total, cur.Err()
}

type SellerRestrictions struct{ col *mongo.Collection }

func NewSellerRestrictions(db *mongo.Database) *SellerRestrictions {
	return &SellerRestrictions{col: db.Collection(database.SellerRestrictionsCollection)}
}

func (r *SellerRestrictions) Create(ctx context.Context, item *models.SellerRestriction) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	_, err := r.col.InsertOne(ctx, item)
	return err
}

func (r *SellerRestrictions) DeactivateActive(ctx context.Context, sellerID string) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	_, err := r.col.UpdateMany(ctx,
		bson.D{{Key: "sellerId", Value: sellerID}, {Key: "active", Value: true}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "active", Value: false}}}},
	)
	return err
}

func (r *SellerRestrictions) ListBySeller(ctx context.Context, sellerID string, limit int) ([]*models.SellerRestriction, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	opts := options.Find().SetSort(bson.D{{Key: "createdAt", Value: -1}}).SetLimit(int64(limit))
	cur, err := r.col.Find(ctx, bson.D{{Key: "sellerId", Value: sellerID}}, opts)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var out []*models.SellerRestriction
	for cur.Next(ctx) {
		var item models.SellerRestriction
		if err := cur.Decode(&item); err != nil {
			return nil, err
		}
		out = append(out, &item)
	}
	return out, cur.Err()
}

func (r *SellerRestrictions) ActiveForSeller(ctx context.Context, sellerID string) (*models.SellerRestriction, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	var item models.SellerRestriction
	err := r.col.FindOne(ctx, bson.D{
		{Key: "sellerId", Value: sellerID},
		{Key: "active", Value: true},
		{Key: "kind", Value: bson.D{{Key: "$ne", Value: models.RestrictionRestore}}},
	}, options.FindOne().SetSort(bson.D{{Key: "createdAt", Value: -1}})).Decode(&item)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, repository.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &item, nil
}

func (r *SellerRestrictions) ExpireDue(ctx context.Context, now time.Time) (int64, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	res, err := r.col.UpdateMany(ctx,
		bson.D{
			{Key: "active", Value: true},
			{Key: "endsAt", Value: bson.D{{Key: "$ne", Value: nil}, {Key: "$lte", Value: now}}},
		},
		bson.D{{Key: "$set", Value: bson.D{{Key: "active", Value: false}}}},
	)
	if err != nil {
		return 0, err
	}
	return res.ModifiedCount, nil
}

type AdminNotes struct{ col *mongo.Collection }

func NewAdminNotes(db *mongo.Database) *AdminNotes {
	return &AdminNotes{col: db.Collection(database.AdminNotesCollection)}
}

func (r *AdminNotes) Create(ctx context.Context, n *models.AdminNote) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	_, err := r.col.InsertOne(ctx, n)
	return err
}

func (r *AdminNotes) List(ctx context.Context, targetType, targetID string, limit int) ([]*models.AdminNote, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	opts := options.Find().SetSort(bson.D{{Key: "createdAt", Value: -1}}).SetLimit(int64(limit))
	cur, err := r.col.Find(ctx, bson.D{
		{Key: "targetType", Value: targetType},
		{Key: "targetId", Value: targetID},
	}, opts)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var out []*models.AdminNote
	for cur.Next(ctx) {
		var item models.AdminNote
		if err := cur.Decode(&item); err != nil {
			return nil, err
		}
		out = append(out, &item)
	}
	return out, cur.Err()
}

func (r *AdminNotes) Update(ctx context.Context, id, body string) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	res, err := r.col.UpdateOne(ctx,
		bson.D{{Key: "_id", Value: id}},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "body", Value: body},
			{Key: "updatedAt", Value: time.Now().UTC()},
		}}},
	)
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		return repository.ErrNotFound
	}
	return nil
}

func (r *AdminNotes) Delete(ctx context.Context, id string) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	res, err := r.col.DeleteOne(ctx, bson.D{{Key: "_id", Value: id}})
	if err != nil {
		return err
	}
	if res.DeletedCount == 0 {
		return repository.ErrNotFound
	}
	return nil
}

type SellerVerifications struct{ col *mongo.Collection }

func NewSellerVerifications(db *mongo.Database) *SellerVerifications {
	return &SellerVerifications{col: db.Collection(database.SellerVerificationsCollection)}
}

func (r *SellerVerifications) Create(ctx context.Context, v *models.SellerVerification) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	_, err := r.col.InsertOne(ctx, v)
	return err
}

func (r *SellerVerifications) FindByID(ctx context.Context, id string) (*models.SellerVerification, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	var v models.SellerVerification
	err := r.col.FindOne(ctx, bson.D{{Key: "_id", Value: id}}).Decode(&v)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, repository.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &v, nil
}

func (r *SellerVerifications) LatestBySeller(ctx context.Context, sellerID string) (*models.SellerVerification, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	var v models.SellerVerification
	err := r.col.FindOne(ctx, bson.D{{Key: "sellerId", Value: sellerID}},
		options.FindOne().SetSort(bson.D{{Key: "createdAt", Value: -1}})).Decode(&v)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, repository.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &v, nil
}

func (r *SellerVerifications) List(ctx context.Context, status string, limit, skip int) ([]*models.SellerVerification, int64, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	q := bson.D{}
	if status != "" {
		q = append(q, bson.E{Key: "status", Value: status})
	}
	total, err := r.col.CountDocuments(ctx, q)
	if err != nil {
		return nil, 0, err
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	if skip < 0 {
		skip = 0
	}
	opts := options.Find().SetSort(bson.D{{Key: "submittedAt", Value: -1}}).SetLimit(int64(limit)).SetSkip(int64(skip))
	cur, err := r.col.Find(ctx, q, opts)
	if err != nil {
		return nil, 0, err
	}
	defer cur.Close(ctx)
	var out []*models.SellerVerification
	for cur.Next(ctx) {
		var item models.SellerVerification
		if err := cur.Decode(&item); err != nil {
			return nil, 0, err
		}
		out = append(out, &item)
	}
	return out, total, cur.Err()
}

func (r *SellerVerifications) UpdateStatus(ctx context.Context, id, status, reviewedBy, reason string, at time.Time) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	set := bson.D{
		{Key: "status", Value: status},
		{Key: "reviewedBy", Value: reviewedBy},
		{Key: "reviewedAt", Value: at},
		{Key: "updatedAt", Value: at},
	}
	if reason != "" {
		set = append(set, bson.E{Key: "rejectionReason", Value: reason})
	}
	res, err := r.col.UpdateOne(ctx, bson.D{{Key: "_id", Value: id}}, bson.D{{Key: "$set", Value: set}})
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		return repository.ErrNotFound
	}
	return nil
}

func (r *SellerVerifications) CountByStatus(ctx context.Context, status string) (int64, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	return r.col.CountDocuments(ctx, bson.D{{Key: "status", Value: status}})
}

type SubscriptionOverrides struct{ col *mongo.Collection }

func NewSubscriptionOverrides(db *mongo.Database) *SubscriptionOverrides {
	return &SubscriptionOverrides{col: db.Collection(database.SubscriptionOverridesCollection)}
}

func (r *SubscriptionOverrides) Create(ctx context.Context, o *models.SubscriptionOverride) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	_, err := r.col.InsertOne(ctx, o)
	return err
}

func (r *SubscriptionOverrides) ActiveForSeller(ctx context.Context, sellerID string, now time.Time) (*models.SubscriptionOverride, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	var o models.SubscriptionOverride
	err := r.col.FindOne(ctx, bson.D{
		{Key: "sellerId", Value: sellerID},
		{Key: "active", Value: true},
		{Key: "startsAt", Value: bson.D{{Key: "$lte", Value: now}}},
		{Key: "endsAt", Value: bson.D{{Key: "$gt", Value: now}}},
	}, options.FindOne().SetSort(bson.D{{Key: "createdAt", Value: -1}})).Decode(&o)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, repository.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &o, nil
}

func (r *SubscriptionOverrides) DeactivateActive(ctx context.Context, sellerID string) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	_, err := r.col.UpdateMany(ctx,
		bson.D{{Key: "sellerId", Value: sellerID}, {Key: "active", Value: true}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "active", Value: false}}}},
	)
	return err
}

func (r *SubscriptionOverrides) ListBySeller(ctx context.Context, sellerID string, limit int) ([]*models.SubscriptionOverride, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	opts := options.Find().SetSort(bson.D{{Key: "createdAt", Value: -1}}).SetLimit(int64(limit))
	cur, err := r.col.Find(ctx, bson.D{{Key: "sellerId", Value: sellerID}}, opts)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var out []*models.SubscriptionOverride
	for cur.Next(ctx) {
		var item models.SubscriptionOverride
		if err := cur.Decode(&item); err != nil {
			return nil, err
		}
		out = append(out, &item)
	}
	return out, cur.Err()
}

func (r *SubscriptionOverrides) CountActivePromotional(ctx context.Context, now time.Time) (int64, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	return r.col.CountDocuments(ctx, bson.D{
		{Key: "active", Value: true},
		{Key: "isPromotional", Value: true},
		{Key: "endsAt", Value: bson.D{{Key: "$gt", Value: now}}},
	})
}

func (r *SubscriptionOverrides) ExpireDue(ctx context.Context, now time.Time) (int64, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	res, err := r.col.UpdateMany(ctx,
		bson.D{
			{Key: "active", Value: true},
			{Key: "endsAt", Value: bson.D{{Key: "$lte", Value: now}}},
		},
		bson.D{{Key: "$set", Value: bson.D{{Key: "active", Value: false}}}},
	)
	if err != nil {
		return 0, err
	}
	return res.ModifiedCount, nil
}

type SellerReports struct{ col *mongo.Collection }

func NewSellerReports(db *mongo.Database) *SellerReports {
	return &SellerReports{col: db.Collection(database.SellerReportsCollection)}
}

func (r *SellerReports) Create(ctx context.Context, item *models.SellerReport) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	_, err := r.col.InsertOne(ctx, item)
	return err
}

func (r *SellerReports) FindByID(ctx context.Context, id string) (*models.SellerReport, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	var item models.SellerReport
	err := r.col.FindOne(ctx, bson.D{{Key: "_id", Value: id}}).Decode(&item)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, repository.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &item, nil
}

func (r *SellerReports) List(ctx context.Context, filter repository.ReportListFilter) ([]*models.SellerReport, int64, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	q := bson.D{}
	if filter.Status != "" {
		q = append(q, bson.E{Key: "status", Value: filter.Status})
	}
	if filter.SellerID != "" {
		q = append(q, bson.E{Key: "sellerId", Value: filter.SellerID})
	}
	total, err := r.col.CountDocuments(ctx, q)
	if err != nil {
		return nil, 0, err
	}
	limit := filter.Limit
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	skip := filter.Skip
	if skip < 0 {
		skip = 0
	}
	opts := options.Find().SetSort(bson.D{{Key: "createdAt", Value: -1}}).SetLimit(int64(limit)).SetSkip(int64(skip))
	cur, err := r.col.Find(ctx, q, opts)
	if err != nil {
		return nil, 0, err
	}
	defer cur.Close(ctx)
	var out []*models.SellerReport
	for cur.Next(ctx) {
		var item models.SellerReport
		if err := cur.Decode(&item); err != nil {
			return nil, 0, err
		}
		out = append(out, &item)
	}
	return out, total, cur.Err()
}

func (r *SellerReports) Update(ctx context.Context, item *models.SellerReport) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	res, err := r.col.ReplaceOne(ctx, bson.D{{Key: "_id", Value: item.ID}}, item)
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		return repository.ErrNotFound
	}
	return nil
}

func (r *SellerReports) CountByStatus(ctx context.Context, status string) (int64, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	return r.col.CountDocuments(ctx, bson.D{{Key: "status", Value: status}})
}

func (r *SellerReports) CountOpen(ctx context.Context) (int64, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	return r.col.CountDocuments(ctx, bson.D{
		{Key: "status", Value: bson.D{{Key: "$nin", Value: bson.A{models.ReportStatusResolved, models.ReportStatusDismissed}}}},
	})
}

type SellerPromotions struct{ col *mongo.Collection }

func NewSellerPromotions(db *mongo.Database) *SellerPromotions {
	return &SellerPromotions{col: db.Collection(database.SellerPromotionsCollection)}
}

func (r *SellerPromotions) Create(ctx context.Context, p *models.SellerPromotion) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	_, err := r.col.InsertOne(ctx, p)
	return err
}

func (r *SellerPromotions) DeactivateActive(ctx context.Context, sellerID, kind string) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	q := bson.D{{Key: "sellerId", Value: sellerID}, {Key: "active", Value: true}}
	if kind != "" {
		q = append(q, bson.E{Key: "kind", Value: kind})
	}
	_, err := r.col.UpdateMany(ctx, q, bson.D{{Key: "$set", Value: bson.D{{Key: "active", Value: false}}}})
	return err
}

func (r *SellerPromotions) ListBySeller(ctx context.Context, sellerID string, limit int) ([]*models.SellerPromotion, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	opts := options.Find().SetSort(bson.D{{Key: "createdAt", Value: -1}}).SetLimit(int64(limit))
	cur, err := r.col.Find(ctx, bson.D{{Key: "sellerId", Value: sellerID}}, opts)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var out []*models.SellerPromotion
	for cur.Next(ctx) {
		var item models.SellerPromotion
		if err := cur.Decode(&item); err != nil {
			return nil, err
		}
		out = append(out, &item)
	}
	return out, cur.Err()
}

func (r *SellerPromotions) ActiveFeatured(ctx context.Context, limit int) ([]*models.SellerPromotion, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	now := time.Now().UTC()
	opts := options.Find().SetSort(bson.D{{Key: "priority", Value: -1}, {Key: "createdAt", Value: -1}}).SetLimit(int64(limit))
	cur, err := r.col.Find(ctx, bson.D{
		{Key: "active", Value: true},
		{Key: "kind", Value: "feature"},
		{Key: "$or", Value: bson.A{
			bson.D{{Key: "endsAt", Value: nil}},
			bson.D{{Key: "endsAt", Value: bson.D{{Key: "$gt", Value: now}}}},
		}},
	}, opts)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var out []*models.SellerPromotion
	for cur.Next(ctx) {
		var item models.SellerPromotion
		if err := cur.Decode(&item); err != nil {
			return nil, err
		}
		out = append(out, &item)
	}
	return out, cur.Err()
}
