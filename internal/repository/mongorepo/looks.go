package mongorepo

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"shopbd/backend/internal/database"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/repository"
)

type Looks struct{ col *mongo.Collection }

func NewLooks(db *mongo.Database) *Looks {
	return &Looks{col: db.Collection(database.LooksCollection)}
}

func (r *Looks) Create(ctx context.Context, l *models.Look) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	if l.Items == nil {
		l.Items = []models.LookItem{}
	}
	_, err := r.col.InsertOne(ctx, l)
	return err
}

func (r *Looks) findOne(ctx context.Context, filter bson.D) (*models.Look, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	var l models.Look
	err := r.col.FindOne(ctx, filter).Decode(&l)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, repository.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &l, nil
}

func (r *Looks) FindByID(ctx context.Context, sellerID, id string) (*models.Look, error) {
	return r.findOne(ctx, bson.D{{Key: "_id", Value: id}, {Key: "sellerId", Value: sellerID}})
}

func (r *Looks) FindPublishedByID(ctx context.Context, id string) (*models.Look, error) {
	return r.findOne(ctx, bson.D{{Key: "_id", Value: id}, {Key: "status", Value: models.LookPublished}})
}

func (r *Looks) ListBySeller(ctx context.Context, sellerID string) ([]*models.Look, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	cur, err := r.col.Find(ctx, bson.D{{Key: "sellerId", Value: sellerID}},
		options.Find().SetSort(bson.D{{Key: "updatedAt", Value: -1}}).SetLimit(int64(models.MaxLooksPerSeller)))
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var out []*models.Look
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *Looks) CountBySeller(ctx context.Context, sellerID string) (int, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	n, err := r.col.CountDocuments(ctx, bson.D{{Key: "sellerId", Value: sellerID}})
	return int(n), err
}

func (r *Looks) UpdateMeta(ctx context.Context, sellerID, id, title, gender, modelImageURL string, at time.Time) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	set := bson.D{
		{Key: "title", Value: title},
		{Key: "modelImageUrl", Value: modelImageURL},
		{Key: "updatedAt", Value: at},
	}
	update := bson.D{}
	if gender == "" {
		update = append(update,
			bson.E{Key: "$set", Value: set},
			bson.E{Key: "$unset", Value: bson.D{{Key: "gender", Value: ""}}})
	} else {
		set = append(set, bson.E{Key: "gender", Value: gender})
		update = append(update, bson.E{Key: "$set", Value: set})
	}
	res, err := r.col.UpdateOne(ctx,
		bson.D{{Key: "_id", Value: id}, {Key: "sellerId", Value: sellerID}}, update)
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		return repository.ErrNotFound
	}
	return nil
}

func (r *Looks) SetStatus(ctx context.Context, sellerID, id, status string, at time.Time) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	set := bson.D{
		{Key: "status", Value: status},
		{Key: "updatedAt", Value: at},
	}
	if status == models.LookPublished {
		set = append(set, bson.E{Key: "publishedAt", Value: at})
	}
	res, err := r.col.UpdateOne(ctx,
		bson.D{{Key: "_id", Value: id}, {Key: "sellerId", Value: sellerID}},
		bson.D{{Key: "$set", Value: set}})
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		return repository.ErrNotFound
	}
	return nil
}

func (r *Looks) Delete(ctx context.Context, sellerID, id string) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	res, err := r.col.DeleteOne(ctx, bson.D{{Key: "_id", Value: id}, {Key: "sellerId", Value: sellerID}})
	if err != nil {
		return err
	}
	if res.DeletedCount == 0 {
		return repository.ErrNotFound
	}
	return nil
}

func (r *Looks) AddItem(ctx context.Context, sellerID, id, productID string, max int, at time.Time) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	// One atomic update: only matches while the product is NOT in the look and
	// the array has no element at index max-1 (so it holds fewer than max).
	filter := bson.D{
		{Key: "_id", Value: id},
		{Key: "sellerId", Value: sellerID},
		{Key: "items.productId", Value: bson.D{{Key: "$ne", Value: productID}}},
		{Key: "items." + strconv.Itoa(max-1), Value: bson.D{{Key: "$exists", Value: false}}},
	}
	res, err := r.col.UpdateOne(ctx, filter, bson.D{
		{Key: "$push", Value: bson.D{{Key: "items", Value: models.LookItem{ProductID: productID}}}},
		{Key: "$set", Value: bson.D{{Key: "updatedAt", Value: at}}},
	})
	if err != nil {
		return err
	}
	if res.MatchedCount == 1 {
		return nil
	}
	// Nothing matched: find out why, to give a precise error.
	l, err := r.FindByID(ctx, sellerID, id)
	if err != nil {
		return err
	}
	if l.HasProduct(productID) {
		return repository.ErrLookItemExists
	}
	return repository.ErrLookFull
}

func (r *Looks) RemoveItem(ctx context.Context, sellerID, id, productID string, at time.Time) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	res, err := r.col.UpdateOne(ctx,
		bson.D{{Key: "_id", Value: id}, {Key: "sellerId", Value: sellerID}},
		bson.D{
			{Key: "$pull", Value: bson.D{{Key: "items", Value: bson.D{{Key: "productId", Value: productID}}}}},
			{Key: "$set", Value: bson.D{{Key: "updatedAt", Value: at}}},
		})
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		return repository.ErrNotFound
	}
	if res.ModifiedCount == 0 {
		return repository.ErrLookItemMissing
	}
	return nil
}

func (r *Looks) ReplaceItem(ctx context.Context, sellerID, id, oldProductID, newProductID string, at time.Time) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	filter := bson.D{
		{Key: "_id", Value: id},
		{Key: "sellerId", Value: sellerID},
		{Key: "$and", Value: bson.A{
			bson.D{{Key: "items.productId", Value: oldProductID}},
			bson.D{{Key: "items.productId", Value: bson.D{{Key: "$ne", Value: newProductID}}}},
		}},
	}
	// arrayFilters pick the old element exactly, with no positional-operator ambiguity.
	res, err := r.col.UpdateOne(ctx, filter,
		bson.D{
			{Key: "$set", Value: bson.D{
				{Key: "items.$[el].productId", Value: newProductID},
				{Key: "updatedAt", Value: at},
			}},
		},
		options.Update().SetArrayFilters(options.ArrayFilters{
			Filters: []interface{}{bson.D{{Key: "el.productId", Value: oldProductID}}},
		}),
	)
	if err != nil {
		return err
	}
	if res.MatchedCount == 1 {
		return nil
	}
	l, err := r.FindByID(ctx, sellerID, id)
	if err != nil {
		return err
	}
	if !l.HasProduct(oldProductID) {
		return repository.ErrLookItemMissing
	}
	return repository.ErrLookItemExists
}

func (r *Looks) SetOrder(ctx context.Context, sellerID, id string, productIDs []string, at time.Time) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	items := make([]models.LookItem, len(productIDs))
	for i, p := range productIDs {
		items[i] = models.LookItem{ProductID: p}
	}
	// Applies only while the look holds exactly this set of products.
	filter := bson.D{
		{Key: "_id", Value: id},
		{Key: "sellerId", Value: sellerID},
		{Key: "items", Value: bson.D{{Key: "$size", Value: len(productIDs)}}},
		{Key: "items.productId", Value: bson.D{{Key: "$all", Value: productIDs}}},
	}
	res, err := r.col.UpdateOne(ctx, filter, bson.D{{Key: "$set", Value: bson.D{
		{Key: "items", Value: items},
		{Key: "updatedAt", Value: at},
	}}})
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		if _, err := r.FindByID(ctx, sellerID, id); err != nil {
			return err
		}
		return repository.ErrLookItemsChanged
	}
	return nil
}

/* ------------------------------------------------------------------ */
/* Product reads the looks service needs (bulk, scoped to the seller)  */
/* ------------------------------------------------------------------ */

// FindByIDs returns the seller's products with these IDs, in no particular
// order. IDs that do not exist (or belong to someone else) are simply absent.
func (r *Products) FindByIDs(ctx context.Context, sellerID string, ids []string) ([]*models.Product, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	cur, err := r.col.Find(ctx, bson.D{
		{Key: "sellerId", Value: sellerID},
		{Key: "_id", Value: bson.D{{Key: "$in", Value: ids}}},
	})
	if err != nil {
		return nil, fmt.Errorf("find products by ids: %w", err)
	}
	defer cur.Close(ctx)
	var out []*models.Product
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ListPublished returns published looks, newest first. sellerID and gender are
// optional filters ("" = any). Used by the public pages; the service still
// checks each seller's access and visibility.
func (r *Looks) ListPublished(ctx context.Context, sellerID, gender string, limit int) ([]*models.Look, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	filter := bson.D{{Key: "status", Value: models.LookPublished}}
	if sellerID != "" {
		filter = append(filter, bson.E{Key: "sellerId", Value: sellerID})
	}
	if gender != "" {
		filter = append(filter, bson.E{Key: "gender", Value: gender})
	}
	cur, err := r.col.Find(ctx, filter,
		options.Find().SetSort(bson.D{{Key: "publishedAt", Value: -1}}).SetLimit(int64(limit)))
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var out []*models.Look
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}
