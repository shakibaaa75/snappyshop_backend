package mongorepo

import (
	"context"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"shopbd/backend/internal/taxonomy"
)

// TaxonomyCollection holds the platform-wide category tree (Main Types and
// their categories), one document per node.
const TaxonomyCollection = "taxonomy_nodes"

// bulkTimeout is longer than opTimeout because the first start inserts the
// whole seed (about 150 documents) in one go.
const bulkTimeout = 30 * time.Second

// TaxonomyNodes implements taxonomy.NodeStore on MongoDB.
type TaxonomyNodes struct{ col *mongo.Collection }

var _ taxonomy.NodeStore = (*TaxonomyNodes)(nil)

func NewTaxonomyNodes(db *mongo.Database) *TaxonomyNodes {
	return &TaxonomyNodes{col: db.Collection(TaxonomyCollection)}
}

// EnsureIndexes creates the indexes the admin screens and the public tree use.
// Safe to call on every start.
func (r *TaxonomyNodes) EnsureIndexes(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, bulkTimeout)
	defer cancel()
	_, err := r.col.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "parentId", Value: 1}, {Key: "sortOrder", Value: 1}}},
		{Keys: bson.D{{Key: "adminSeen", Value: 1}, {Key: "createdAt", Value: -1}}},
	})
	return err
}

func (r *TaxonomyNodes) ListNodes(ctx context.Context) ([]taxonomy.Node, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	cur, err := r.col.Find(ctx, bson.D{}, options.Find().SetSort(bson.D{
		{Key: "sortOrder", Value: 1}, {Key: "_id", Value: 1},
	}))
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var out []taxonomy.Node
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *TaxonomyNodes) CountNodes(ctx context.Context) (int64, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	return r.col.CountDocuments(ctx, bson.D{})
}

// InsertMissing inserts only nodes whose _id is not there yet ($setOnInsert),
// so it never overwrites an admin's renames or edits.
func (r *TaxonomyNodes) InsertMissing(ctx context.Context, nodes []taxonomy.Node) error {
	if len(nodes) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, bulkTimeout)
	defer cancel()
	models := make([]mongo.WriteModel, 0, len(nodes))
	for _, n := range nodes {
		raw, err := bson.Marshal(n)
		if err != nil {
			return err
		}
		var doc bson.M
		if err := bson.Unmarshal(raw, &doc); err != nil {
			return err
		}
		delete(doc, "_id") // _id comes from the filter
		models = append(models, mongo.NewUpdateOneModel().
			SetFilter(bson.D{{Key: "_id", Value: n.ID}}).
			SetUpdate(bson.D{{Key: "$setOnInsert", Value: doc}}).
			SetUpsert(true))
	}
	_, err := r.col.BulkWrite(ctx, models, options.BulkWrite().SetOrdered(false))
	return err
}

// InsertIfAbsent atomically inserts one node; false means _id already exists.
func (r *TaxonomyNodes) InsertIfAbsent(ctx context.Context, n taxonomy.Node) (bool, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	if _, err := r.col.InsertOne(ctx, n); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (r *TaxonomyNodes) UpdateNode(ctx context.Context, n taxonomy.Node) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	res, err := r.col.ReplaceOne(ctx, bson.D{{Key: "_id", Value: n.ID}}, n)
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		return taxonomy.ErrNotFound
	}
	return nil
}

func (r *TaxonomyNodes) DeleteNode(ctx context.Context, id string) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	_, err := r.col.DeleteOne(ctx, bson.D{{Key: "_id", Value: id}})
	return err
}

func (r *TaxonomyNodes) MarkSeen(ctx context.Context, ids []string) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	filter := bson.D{{Key: "adminSeen", Value: false}}
	if len(ids) > 0 {
		filter = bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: ids}}}}
	}
	_, err := r.col.UpdateMany(ctx, filter, bson.D{{Key: "$set", Value: bson.D{{Key: "adminSeen", Value: true}}}})
	return err
}
