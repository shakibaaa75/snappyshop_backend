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

// CustomDomains implements repository.CustomDomainRepository.
type CustomDomains struct{ col *mongo.Collection }

func NewCustomDomains(db *mongo.Database) *CustomDomains {
	return &CustomDomains{col: db.Collection(database.CustomDomainsCollection)}
}

func (r *CustomDomains) GetBySeller(ctx context.Context, sellerID string) (*models.CustomDomain, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	var out models.CustomDomain
	err := r.col.FindOne(ctx, bson.D{{Key: "_id", Value: sellerID}}).Decode(&out)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, repository.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// GetVerifiedByDomain filters on status as well, so the partial unique index
// (domain, only where status is "verified") answers the query.
func (r *CustomDomains) GetVerifiedByDomain(ctx context.Context, domain string) (*models.CustomDomain, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	var out models.CustomDomain
	err := r.col.FindOne(ctx, bson.D{
		{Key: "domain", Value: domain},
		{Key: "status", Value: models.DomainVerified},
	}).Decode(&out)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, repository.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (r *CustomDomains) Save(ctx context.Context, d *models.CustomDomain) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	_, err := r.col.ReplaceOne(ctx,
		bson.D{{Key: "_id", Value: d.SellerID}},
		d,
		options.Replace().SetUpsert(true),
	)
	if mongo.IsDuplicateKeyError(err) {
		return repository.ErrDomainTaken
	}
	return err
}

func (r *CustomDomains) Delete(ctx context.Context, sellerID string) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	_, err := r.col.DeleteOne(ctx, bson.D{{Key: "_id", Value: sellerID}})
	return err
}

var _ repository.CustomDomainRepository = (*CustomDomains)(nil)
