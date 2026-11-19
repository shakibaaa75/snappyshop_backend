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

// emailCollation makes email matching ignore upper/lower case. The index
// "idx_orders_email_created" is built with the same collation, so it is used.
func emailCollation() *options.Collation {
	return &options.Collation{Locale: "en", Strength: 2}
}

// BuyerOrders implements repository.BuyerOrderRepository on the orders collection.
type BuyerOrders struct{ db *mongo.Database }

func NewBuyerOrders(db *mongo.Database) *BuyerOrders { return &BuyerOrders{db: db} }

func (r *BuyerOrders) col() *mongo.Collection { return r.db.Collection(database.OrdersCollection) }

func (r *BuyerOrders) ListByEmail(ctx context.Context, email string, limit int, before time.Time) ([]*models.Order, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	filter := bson.D{{Key: "customerEmail", Value: email}}
	if !before.IsZero() {
		filter = append(filter, bson.E{Key: "createdAt", Value: bson.D{{Key: "$lt", Value: before}}})
	}
	cur, err := r.col().Find(ctx, filter, options.Find().
		SetCollation(emailCollation()).
		SetSort(bson.D{{Key: "createdAt", Value: -1}}).
		SetLimit(int64(limit)))
	if err != nil {
		return nil, err
	}
	var out []*models.Order
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *BuyerOrders) FindByEmailAndID(ctx context.Context, email, orderID string) (*models.Order, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	var o models.Order
	err := r.col().FindOne(ctx,
		bson.D{{Key: "_id", Value: orderID}, {Key: "customerEmail", Value: email}},
		options.FindOne().SetCollation(emailCollation())).Decode(&o)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, repository.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &o, nil
}

// OrderIssues implements repository.OrderIssueRepository.
type OrderIssues struct{ db *mongo.Database }

func NewOrderIssues(db *mongo.Database) *OrderIssues { return &OrderIssues{db: db} }

func (r *OrderIssues) col() *mongo.Collection { return r.db.Collection(database.OrderIssuesCollection) }

func (r *OrderIssues) Create(ctx context.Context, i *models.OrderIssue) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	// A partial unique index allows only one open issue per order.
	if _, err := r.col().InsertOne(ctx, i); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return repository.ErrIssueOpen
		}
		return err
	}
	return nil
}

func (r *OrderIssues) ListByBuyer(ctx context.Context, buyerID string, limit int) ([]*models.OrderIssue, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	cur, err := r.col().Find(ctx, bson.D{{Key: "buyerId", Value: buyerID}},
		options.Find().SetSort(bson.D{{Key: "createdAt", Value: -1}}).SetLimit(int64(limit)))
	if err != nil {
		return nil, err
	}
	var out []*models.OrderIssue
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *OrderIssues) Withdraw(ctx context.Context, buyerID, issueID string, at time.Time) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	res, err := r.col().UpdateOne(ctx,
		bson.D{{Key: "_id", Value: issueID}, {Key: "buyerId", Value: buyerID}, {Key: "status", Value: models.IssueOpen}},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "status", Value: models.IssueWithdrawn},
			{Key: "updatedAt", Value: at},
			{Key: "closedAt", Value: at},
		}}})
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		return repository.ErrNotFound
	}
	return nil
}
