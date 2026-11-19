package mongorepo

import (
	"context"
	"errors"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"shopbd/backend/internal/database"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/repository"
)

// Categories implements repository.CategoryRepository.
type Categories struct{ col *mongo.Collection }

func NewCategories(db *mongo.Database) *Categories {
	return &Categories{col: db.Collection(database.CategoriesCollection)}
}

func (r *Categories) Create(ctx context.Context, c *models.Category) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	if _, err := r.col.InsertOne(ctx, c); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return repository.ErrSlugTaken
		}
		return err
	}
	return nil
}

func (r *Categories) List(ctx context.Context, sellerID string) ([]*models.Category, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	opts := options.Find().SetSort(bson.D{
		{Key: "position", Value: 1},
		{Key: "createdAt", Value: 1},
	})
	cur, err := r.col.Find(ctx, bson.D{{Key: "sellerId", Value: sellerID}}, opts)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var out []*models.Category
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *Categories) FindByID(ctx context.Context, sellerID, id string) (*models.Category, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	var c models.Category
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

func (r *Categories) Update(ctx context.Context, sellerID, id string, c *models.Category) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	res, err := r.col.UpdateOne(ctx,
		bson.D{{Key: "_id", Value: id}, {Key: "sellerId", Value: sellerID}},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "name", Value: c.Name},
			{Key: "slug", Value: c.Slug},
			{Key: "emoji", Value: c.Emoji},
			{Key: "imageUrl", Value: c.ImageURL}, // <-- added
			{Key: "position", Value: c.Position},
			{Key: "updatedAt", Value: c.UpdatedAt},
		}}},
	)
	if err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return repository.ErrSlugTaken
		}
		return err
	}
	if res.MatchedCount == 0 {
		return repository.ErrNotFound
	}
	return nil
}

func (r *Categories) Delete(ctx context.Context, sellerID, id string) error {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
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
