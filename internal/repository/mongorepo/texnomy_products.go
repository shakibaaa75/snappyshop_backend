package mongorepo

import (
	"context"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"

	"shopbd/backend/internal/database"
	"shopbd/backend/internal/taxonomy"
)

// TaxonomyProducts implements taxonomy.ProductStore: it keeps products in
// step with admin category changes.
type TaxonomyProducts struct{ col *mongo.Collection }

var _ taxonomy.ProductStore = (*TaxonomyProducts)(nil)

func NewTaxonomyProducts(db *mongo.Database) *TaxonomyProducts {
	return &TaxonomyProducts{col: db.Collection(database.ProductsCollection)}
}

func (r *TaxonomyProducts) CountByCategory(ctx context.Context) (map[string]int, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	cur, err := r.col.Aggregate(ctx, mongo.Pipeline{
		{{Key: "$match", Value: bson.D{{Key: "subId", Value: bson.D{{Key: "$ne", Value: ""}}}}}},
		{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: "$subId"},
			{Key: "n", Value: bson.D{{Key: "$sum", Value: 1}}},
		}}},
	})
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var rows []struct {
		ID string `bson:"_id"`
		N  int    `bson:"n"`
	}
	if err := cur.All(ctx, &rows); err != nil {
		return nil, err
	}
	out := make(map[string]int, len(rows))
	for _, row := range rows {
		out[row.ID] = row.N
	}
	return out, nil
}

func (r *TaxonomyProducts) CountForCategory(ctx context.Context, id string) (int64, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	return r.col.CountDocuments(ctx, bson.D{{Key: "subId", Value: id}})
}

func (r *TaxonomyProducts) Reassign(ctx context.Context, fromID string, to taxonomy.Node) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, bulkTimeout)
	defer cancel()
	res, err := r.col.UpdateMany(ctx, bson.D{{Key: "subId", Value: fromID}}, bson.D{{Key: "$set", Value: bson.D{
		{Key: "mainId", Value: to.ParentID},
		{Key: "subId", Value: to.ID},
		{Key: "subName", Value: to.Name},
		{Key: "categoryStatus", Value: "mapped"},
	}}})
	if err != nil {
		return 0, err
	}
	return res.ModifiedCount, nil
}

func (r *TaxonomyProducts) SetName(ctx context.Context, id, name string) error {
	ctx, cancel := context.WithTimeout(ctx, bulkTimeout)
	defer cancel()
	_, err := r.col.UpdateMany(ctx, bson.D{{Key: "subId", Value: id}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "subName", Value: name}}}})
	return err
}

func (r *TaxonomyProducts) SetMain(ctx context.Context, id, mainID string) error {
	ctx, cancel := context.WithTimeout(ctx, bulkTimeout)
	defer cancel()
	_, err := r.col.UpdateMany(ctx, bson.D{{Key: "subId", Value: id}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "mainId", Value: mainID}}}})
	return err
}
