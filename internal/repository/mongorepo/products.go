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

// Products implements repository.ProductRepository.
type Products struct{ col *mongo.Collection }

func NewProducts(db *mongo.Database) *Products {
	return &Products{col: db.Collection(database.ProductsCollection)}
}

func (r *Products) Create(ctx context.Context, p *models.Product) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	if _, err := r.col.InsertOne(ctx, p); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return repository.ErrProductSlug
		}
		return err
	}
	return nil
}

func (r *Products) FindByID(ctx context.Context, sellerID, id string) (*models.Product, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	var p models.Product
	err := r.col.FindOne(ctx, bson.D{
		{Key: "_id", Value: id},
		{Key: "sellerId", Value: sellerID},
	}).Decode(&p)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, repository.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (r *Products) FindBySlug(ctx context.Context, sellerID, slug string) (*models.Product, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	var p models.Product
	err := r.col.FindOne(ctx, bson.D{
		{Key: "sellerId", Value: sellerID},
		{Key: "slug", Value: slug},
	}).Decode(&p)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, repository.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (r *Products) List(ctx context.Context, sellerID string, limit int) ([]*models.Product, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	opts := options.Find().
		SetSort(bson.D{{Key: "createdAt", Value: -1}}).
		SetLimit(int64(limit))
	cur, err := r.col.Find(ctx, bson.D{{Key: "sellerId", Value: sellerID}}, opts)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var out []*models.Product
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ListPublic returns only active products for the seller. Used by the
// storefront.
func (r *Products) ListPublic(ctx context.Context, sellerID string, limit int) ([]*models.Product, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	opts := options.Find().
		SetSort(bson.D{{Key: "createdAt", Value: -1}}).
		SetLimit(int64(limit))
	cur, err := r.col.Find(ctx, bson.D{
		{Key: "sellerId", Value: sellerID},
		{Key: "isActive", Value: true},
	}, opts)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var out []*models.Product
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *Products) Update(ctx context.Context, sellerID, id string, p *models.Product) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	p.UpdatedAt = time.Now().UTC()

	res, err := r.col.UpdateOne(ctx,
		bson.D{{Key: "_id", Value: id}, {Key: "sellerId", Value: sellerID}},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "name", Value: p.Name},
			{Key: "slug", Value: p.Slug},
			{Key: "description", Value: p.Description},
			// Seller's own storefront categories (all of them, not just the primary).
			{Key: "categoryId", Value: p.CategoryID},
			{Key: "categoryName", Value: p.CategoryName},
			{Key: "categoryIds", Value: nonNilStrings(p.CategoryIDs)},
			{Key: "categoryNames", Value: nonNilStrings(p.CategoryNames)},
			// Platform-wide type + category. Without these the edit form opens
			// empty and the product keeps its old type forever.
			{Key: "mainId", Value: p.MainID},
			{Key: "subId", Value: p.SubID},
			{Key: "subName", Value: p.SubName},
			{Key: "rawSubCategory", Value: p.RawSubCategory},
			{Key: "categoryStatus", Value: p.CategoryStatus},
			{Key: "price", Value: p.Price},
			{Key: "comparePrice", Value: p.ComparePrice},
			{Key: "stock", Value: p.Stock},
			{Key: "sku", Value: p.SKU},
			{Key: "mainImageUrl", Value: p.MainImageURL},
			{Key: "imageUrls", Value: p.ImageURLs},
			{Key: "emoji", Value: p.Emoji},
			{Key: "hue", Value: p.Hue},
			{Key: "options", Value: p.Options},
			{Key: "combinations", Value: p.Combinations},
			{Key: "variantImageMode", Value: p.VariantImageMode},
			{Key: "attributes", Value: p.Attributes},
			{Key: "isNewArrival", Value: p.IsNewArrival},
			{Key: "isFeatured", Value: p.IsFeatured},
			{Key: "isBestSeller", Value: p.IsBestSeller},
			{Key: "tags", Value: p.Tags},
			{Key: "isActive", Value: p.IsActive},
			{Key: "updatedAt", Value: p.UpdatedAt},
		}}},
	)
	if err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return repository.ErrProductSlug
		}
		return err
	}
	if res.MatchedCount == 0 {
		return repository.ErrNotFound
	}
	return nil
}

func (r *Products) Delete(ctx context.Context, sellerID, id string) error {
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

// nonNilStrings makes sure an empty list is stored as [] and not null.
func nonNilStrings(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}
