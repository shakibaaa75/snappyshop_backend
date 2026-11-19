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

// PlanCatalog implements repository.PlanCatalogRepository.
type PlanCatalog struct{ col *mongo.Collection }

func NewPlanCatalog(db *mongo.Database) *PlanCatalog {
	return &PlanCatalog{col: db.Collection(database.PlanCatalogCollection)}
}

func (r *PlanCatalog) Get(ctx context.Context) (*models.PlanCatalogDoc, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	var d models.PlanCatalogDoc
	err := r.col.FindOne(ctx, bson.D{{Key: "_id", Value: models.PlanCatalogID}}).Decode(&d)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, repository.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

func (r *PlanCatalog) Save(ctx context.Context, doc *models.PlanCatalogDoc) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	doc.ID = models.PlanCatalogID
	_, err := r.col.ReplaceOne(ctx, bson.D{{Key: "_id", Value: models.PlanCatalogID}}, doc,
		options.Replace().SetUpsert(true))
	return err
}
