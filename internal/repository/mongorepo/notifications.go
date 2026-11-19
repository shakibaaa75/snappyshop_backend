package mongorepo

import (
	"context"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"shopbd/backend/internal/database"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/repository"
)

// Notifications implements repository.NotificationRepository.
type Notifications struct{ db *mongo.Database }

func NewNotifications(db *mongo.Database) *Notifications { return &Notifications{db: db} }

func (r *Notifications) col() *mongo.Collection {
	return r.db.Collection(database.NotificationsCollection)
}

func (r *Notifications) reads() *mongo.Collection {
	return r.db.Collection(database.NotificationReadsCollection)
}

func (r *Notifications) Create(ctx context.Context, n *models.Notification) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	_, err := r.col().InsertOne(ctx, n)
	return err
}

func (r *Notifications) Delete(ctx context.Context, id string) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	res, err := r.col().DeleteOne(ctx, bson.D{{Key: "_id", Value: id}})
	if err != nil {
		return err
	}
	if res.DeletedCount == 0 {
		return repository.ErrNotFound
	}
	_, err = r.reads().DeleteMany(ctx, bson.D{{Key: "notificationId", Value: id}})
	return err
}

func (r *Notifications) ListAdmin(ctx context.Context, limit, skip int) ([]*models.Notification, int64, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	if skip < 0 {
		skip = 0
	}
	// Platform-made notifications (new order, ...) are not part of what an
	// admin sent, so the admin history leaves them out.
	filter := bson.D{{Key: "createdBy", Value: bson.D{{Key: "$ne", Value: "system"}}}}
	total, err := r.col().CountDocuments(ctx, filter)
	if err != nil {
		return nil, 0, err
	}
	cur, err := r.col().Find(ctx, filter, options.Find().
		SetSort(bson.D{{Key: "createdAt", Value: -1}}).
		SetLimit(int64(limit)).SetSkip(int64(skip)))
	if err != nil {
		return nil, 0, err
	}
	defer cur.Close(ctx)
	var out []*models.Notification
	if err := cur.All(ctx, &out); err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

func (r *Notifications) ReadCounts(ctx context.Context, ids []string) (map[string]int64, error) {
	out := map[string]int64{}
	if len(ids) == 0 {
		return out, nil
	}
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	cur, err := r.reads().Aggregate(ctx, mongo.Pipeline{
		{{Key: "$match", Value: bson.D{{Key: "notificationId", Value: bson.D{{Key: "$in", Value: ids}}}}}},
		{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: "$notificationId"},
			{Key: "n", Value: bson.D{{Key: "$sum", Value: 1}}},
		}}},
	})
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var rows []struct {
		ID string `bson:"_id"`
		N  int64  `bson:"n"`
	}
	if err := cur.All(ctx, &rows); err != nil {
		return nil, err
	}
	for _, row := range rows {
		out[row.ID] = row.N
	}
	return out, nil
}

func (r *Notifications) ListForSeller(ctx context.Context, sellerID string, since, now time.Time, limit int) ([]*models.Notification, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	filter := bson.D{{Key: "$and", Value: bson.A{
		bson.D{{Key: "createdAt", Value: bson.D{{Key: "$gte", Value: since}}}},
		// expiresAt missing/null = never expires
		bson.D{{Key: "$or", Value: bson.A{
			bson.D{{Key: "expiresAt", Value: nil}},
			bson.D{{Key: "expiresAt", Value: bson.D{{Key: "$gt", Value: now}}}},
		}}},
		bson.D{{Key: "$or", Value: bson.A{
			bson.D{{Key: "audience", Value: models.NotificationAudienceAll}},
			bson.D{{Key: "recipientIds", Value: sellerID}},
		}}},
	}}}
	cur, err := r.col().Find(ctx, filter, options.Find().
		SetSort(bson.D{{Key: "createdAt", Value: -1}}).
		SetLimit(int64(limit)))
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var out []*models.Notification
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *Notifications) ReadSet(ctx context.Context, userID string, ids []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(ids) == 0 {
		return out, nil
	}
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	cur, err := r.reads().Find(ctx, bson.D{
		{Key: "userId", Value: userID},
		{Key: "notificationId", Value: bson.D{{Key: "$in", Value: ids}}},
	})
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var rows []models.NotificationRead
	if err := cur.All(ctx, &rows); err != nil {
		return nil, err
	}
	for _, row := range rows {
		out[row.NotificationID] = true
	}
	return out, nil
}

func (r *Notifications) MarkRead(ctx context.Context, userID string, ids []string, at time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	ops := make([]mongo.WriteModel, 0, len(ids))
	for _, id := range ids {
		ops = append(ops, mongo.NewUpdateOneModel().
			SetFilter(bson.D{{Key: "_id", Value: userID + ":" + id}}).
			SetUpdate(bson.D{{Key: "$setOnInsert", Value: bson.D{
				{Key: "userId", Value: userID},
				{Key: "notificationId", Value: id},
				{Key: "readAt", Value: at},
			}}}).
			SetUpsert(true))
	}
	_, err := r.reads().BulkWrite(ctx, ops, options.BulkWrite().SetOrdered(false))
	return err
}

func (r *Notifications) ExistingSellerIDs(ctx context.Context, ids []string) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	cur, err := r.db.Collection(database.UsersCollection).Find(ctx,
		bson.D{
			{Key: "_id", Value: bson.D{{Key: "$in", Value: ids}}},
			{Key: "role", Value: string(models.RoleSeller)},
		},
		options.Find().SetProjection(bson.D{{Key: "_id", Value: 1}}))
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var rows []struct {
		ID string `bson:"_id"`
	}
	if err := cur.All(ctx, &rows); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.ID)
	}
	return out, nil
}

var _ repository.NotificationRepository = (*Notifications)(nil)
