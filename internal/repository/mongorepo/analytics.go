package mongorepo

import (
	"context"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"shopbd/backend/internal/database"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/repository"
)

// Analytics implements repository.AnalyticsRepository.
type Analytics struct{ col *mongo.Collection }

func NewAnalytics(db *mongo.Database) *Analytics {
	return &Analytics{col: db.Collection(database.AnalyticsEventsCollection)}
}

// EnsureIndexes creates the unique index (plus a query index) that stops the same device from
// counting twice in one window. Call it once at startup (see main.go) and
// LOG the returned error: if old rows contain duplicate "dedupe" values the
// build fails. Insert below still dedupes without the index.
//
// The index is partial: only documents that have a "dedupe" field are
// checked, so search events and old events (no dedupe) are unaffected.
func (r *Analytics) EnsureIndexes(ctx context.Context) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	_, err := r.col.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{
			Keys: bson.D{{Key: "dedupe", Value: 1}},
			Options: options.Index().
				SetName("uniq_dedupe").
				SetUnique(true).
				SetPartialFilterExpression(bson.D{
					{Key: "dedupe", Value: bson.D{{Key: "$exists", Value: true}}},
				}),
		},
		{
			// Every overview query filters by seller, kind and a createdAt range.
			Keys:    bson.D{{Key: "sellerId", Value: 1}, {Key: "kind", Value: 1}, {Key: "createdAt", Value: -1}},
			Options: options.Index().SetName("idx_analytics_seller_kind_created"),
		},
	})
	if err != nil {
		return fmt.Errorf("analytics: create indexes: %w", err)
	}
	return nil
}

// Insert stores an event. Events with a Dedupe key are upserted: the row is
// created only if no row with that key exists, so refreshes inside the same
// window never add a second row (with or without the unique index).
func (r *Analytics) Insert(ctx context.Context, e *models.AnalyticsEvent) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()

	if e.Dedupe == "" {
		_, err := r.col.InsertOne(ctx, e)
		return err
	}
	_, err := r.col.UpdateOne(ctx,
		bson.D{{Key: "dedupe", Value: e.Dedupe}},
		bson.D{{Key: "$setOnInsert", Value: e}},
		options.Update().SetUpsert(true),
	)
	if mongo.IsDuplicateKeyError(err) {
		// Lost a race with a concurrent insert: already counted.
		return nil
	}
	return err
}

func (r *Analytics) CountByKind(ctx context.Context, sellerID, kind string, since time.Time) (int, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	n, err := r.col.CountDocuments(ctx, bson.D{
		{Key: "sellerId", Value: sellerID},
		{Key: "kind", Value: kind},
		{Key: "createdAt", Value: bson.D{{Key: "$gte", Value: since}}},
	})
	return int(n), err
}

func (r *Analytics) ProductCounts(ctx context.Context, sellerID, kind string, since time.Time, limit int) ([]repository.ProductCount, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	if limit <= 0 {
		limit = 20
	}
	pipeline := mongo.Pipeline{
		{{Key: "$match", Value: bson.D{
			{Key: "sellerId", Value: sellerID},
			{Key: "kind", Value: kind},
			{Key: "productId", Value: bson.D{{Key: "$ne", Value: ""}}},
			{Key: "createdAt", Value: bson.D{{Key: "$gte", Value: since}}},
		}}},
		{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: "$productId"},
			{Key: "count", Value: bson.D{{Key: "$sum", Value: 1}}},
		}}},
		{{Key: "$sort", Value: bson.D{{Key: "count", Value: -1}}}},
		{{Key: "$limit", Value: limit}},
	}
	cur, err := r.col.Aggregate(ctx, pipeline)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)

	var out []repository.ProductCount
	for cur.Next(ctx) {
		var row struct {
			ID    string `bson:"_id"`
			Count int    `bson:"count"`
		}
		if err := cur.Decode(&row); err != nil {
			return nil, err
		}
		out = append(out, repository.ProductCount{ProductID: row.ID, Count: row.Count})
	}
	return out, cur.Err()
}

func (r *Analytics) TermCounts(ctx context.Context, sellerID string, since time.Time, limit int) ([]repository.TermCount, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	if limit <= 0 {
		limit = 20
	}
	pipeline := mongo.Pipeline{
		{{Key: "$match", Value: bson.D{
			{Key: "sellerId", Value: sellerID},
			{Key: "kind", Value: models.AnalyticsEventSearch},
			{Key: "term", Value: bson.D{{Key: "$ne", Value: ""}}},
			{Key: "createdAt", Value: bson.D{{Key: "$gte", Value: since}}},
		}}},
		{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: "$term"},
			{Key: "count", Value: bson.D{{Key: "$sum", Value: 1}}},
		}}},
		{{Key: "$sort", Value: bson.D{{Key: "count", Value: -1}}}},
		{{Key: "$limit", Value: limit}},
	}
	cur, err := r.col.Aggregate(ctx, pipeline)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)

	var out []repository.TermCount
	for cur.Next(ctx) {
		var row struct {
			ID    string `bson:"_id"`
			Count int    `bson:"count"`
		}
		if err := cur.Decode(&row); err != nil {
			return nil, err
		}
		out = append(out, repository.TermCount{Term: row.ID, Count: row.Count})
	}
	return out, cur.Err()
}

func (r *Analytics) SourceCounts(ctx context.Context, sellerID string, since time.Time, limit int) ([]repository.SourceCount, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	if limit <= 0 {
		limit = 20
	}
	pipeline := mongo.Pipeline{
		{{Key: "$match", Value: bson.D{
			{Key: "sellerId", Value: sellerID},
			{Key: "kind", Value: models.AnalyticsEventTraffic},
			{Key: "source", Value: bson.D{{Key: "$ne", Value: ""}}},
			{Key: "createdAt", Value: bson.D{{Key: "$gte", Value: since}}},
		}}},
		{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: "$source"},
			{Key: "count", Value: bson.D{{Key: "$sum", Value: 1}}},
		}}},
		{{Key: "$sort", Value: bson.D{{Key: "count", Value: -1}}}},
		{{Key: "$limit", Value: limit}},
	}
	cur, err := r.col.Aggregate(ctx, pipeline)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)

	var out []repository.SourceCount
	for cur.Next(ctx) {
		var row struct {
			ID    string `bson:"_id"`
			Count int    `bson:"count"`
		}
		if err := cur.Decode(&row); err != nil {
			return nil, err
		}
		out = append(out, repository.SourceCount{Source: row.ID, Count: row.Count})
	}
	return out, cur.Err()
}

func (r *Analytics) ProductCountsInRange(ctx context.Context, sellerID, kind string, from, to time.Time) (map[string]int, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	pipeline := mongo.Pipeline{
		{{Key: "$match", Value: bson.D{
			{Key: "sellerId", Value: sellerID},
			{Key: "kind", Value: kind},
			{Key: "productId", Value: bson.D{{Key: "$ne", Value: ""}}},
			{Key: "createdAt", Value: bson.D{
				{Key: "$gte", Value: from},
				{Key: "$lt", Value: to},
			}},
		}}},
		{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: "$productId"},
			{Key: "count", Value: bson.D{{Key: "$sum", Value: 1}}},
		}}},
	}
	cur, err := r.col.Aggregate(ctx, pipeline, options.Aggregate())
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)

	out := make(map[string]int)
	for cur.Next(ctx) {
		var row struct {
			ID    string `bson:"_id"`
			Count int    `bson:"count"`
		}
		if err := cur.Decode(&row); err != nil {
			return nil, err
		}
		out[row.ID] = row.Count
	}
	return out, cur.Err()
}

func (r *Analytics) DailyCounts(ctx context.Context, sellerID, kind string, from, to time.Time) (map[string]int, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()

	pipeline := mongo.Pipeline{
		{{Key: "$match", Value: bson.D{
			{Key: "sellerId", Value: sellerID},
			{Key: "kind", Value: kind},
			{Key: "createdAt", Value: bson.D{
				{Key: "$gte", Value: from},
				{Key: "$lt", Value: to},
			}},
		}}},
		{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: bson.D{
				{Key: "$dateToString", Value: bson.D{
					{Key: "format", Value: "%Y-%m-%d"},
					{Key: "date", Value: "$createdAt"},
					{Key: "timezone", Value: "UTC"},
				}},
			}},
			{Key: "count", Value: bson.D{{Key: "$sum", Value: 1}}},
		}}},
	}

	cur, err := r.col.Aggregate(ctx, pipeline)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)

	out := make(map[string]int)
	for cur.Next(ctx) {
		var row struct {
			ID    string `bson:"_id"`
			Count int    `bson:"count"`
		}
		if err := cur.Decode(&row); err != nil {
			return nil, err
		}
		out[row.ID] = row.Count
	}
	return out, cur.Err()
}

// SourceBreakdown groups events by (source, kind). Visitors are counted as the
// number of distinct visitor hashes inside each group; events without a hash
// (stored before visitor hashing existed) are simply not counted as visitors.
func (r *Analytics) SourceBreakdown(ctx context.Context, sellerID string, since time.Time) ([]repository.SourceKindStat, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	pipeline := mongo.Pipeline{
		{{Key: "$match", Value: bson.D{
			{Key: "sellerId", Value: sellerID},
			{Key: "kind", Value: bson.D{{Key: "$in", Value: bson.A{
				models.AnalyticsEventTraffic,
				models.AnalyticsEventProductView,
				models.AnalyticsEventClick,
				models.AnalyticsEventAddToCart,
				models.AnalyticsEventPurchase,
			}}}},
			{Key: "source", Value: bson.D{{Key: "$exists", Value: true}, {Key: "$ne", Value: ""}}},
			{Key: "createdAt", Value: bson.D{{Key: "$gte", Value: since}}},
		}}},
		// First collapse to one row per (source, kind, visitor) ...
		{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: bson.D{
				{Key: "s", Value: "$source"},
				{Key: "k", Value: "$kind"},
				{Key: "v", Value: "$visitor"},
			}},
			{Key: "count", Value: bson.D{{Key: "$sum", Value: 1}}},
			{Key: "value", Value: bson.D{{Key: "$sum", Value: bson.D{{Key: "$ifNull", Value: bson.A{"$value", 0}}}}}},
		}}},
		// ... then roll up to (source, kind). Rows per visitor => distinct visitors.
		{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: bson.D{
				{Key: "s", Value: "$_id.s"},
				{Key: "k", Value: "$_id.k"},
			}},
			{Key: "count", Value: bson.D{{Key: "$sum", Value: "$count"}}},
			{Key: "value", Value: bson.D{{Key: "$sum", Value: "$value"}}},
			{Key: "visitors", Value: bson.D{{Key: "$sum", Value: bson.D{
				{Key: "$cond", Value: bson.A{bson.D{{Key: "$ifNull", Value: bson.A{"$_id.v", false}}}, 1, 0}},
			}}}},
		}}},
	}
	cur, err := r.col.Aggregate(ctx, pipeline)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)

	var out []repository.SourceKindStat
	for cur.Next(ctx) {
		var row struct {
			ID struct {
				Source string `bson:"s"`
				Kind   string `bson:"k"`
			} `bson:"_id"`
			Count    int     `bson:"count"`
			Value    float64 `bson:"value"`
			Visitors int     `bson:"visitors"`
		}
		if err := cur.Decode(&row); err != nil {
			return nil, err
		}
		out = append(out, repository.SourceKindStat{
			Source: row.ID.Source, Kind: row.ID.Kind,
			Count: row.Count, Visitors: row.Visitors, Value: row.Value,
		})
	}
	return out, cur.Err()
}

// DailySourceCounts groups events in [from, to) by (UTC day, source, kind),
// restricted to the given sources. It powers the per-platform daily charts.
// It implements repository.DailySourceCounter.
func (r *Analytics) DailySourceCounts(ctx context.Context, sellerID string, sources []string, from, to time.Time) ([]repository.DailySourceKindStat, error) {
	if len(sources) == 0 {
		return nil, nil
	}
	ctx, cancel := withTimeout(ctx)
	defer cancel()

	srcs := make(bson.A, 0, len(sources))
	for _, s := range sources {
		srcs = append(srcs, s)
	}
	pipeline := mongo.Pipeline{
		{{Key: "$match", Value: bson.D{
			{Key: "sellerId", Value: sellerID},
			{Key: "kind", Value: bson.D{{Key: "$in", Value: bson.A{
				models.AnalyticsEventTraffic,
				models.AnalyticsEventProductView,
				models.AnalyticsEventClick,
				models.AnalyticsEventAddToCart,
				models.AnalyticsEventPurchase,
			}}}},
			{Key: "source", Value: bson.D{{Key: "$in", Value: srcs}}},
			{Key: "createdAt", Value: bson.D{
				{Key: "$gte", Value: from},
				{Key: "$lt", Value: to},
			}},
		}}},
		{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: bson.D{
				{Key: "d", Value: bson.D{
					{Key: "$dateToString", Value: bson.D{
						{Key: "format", Value: "%Y-%m-%d"},
						{Key: "date", Value: "$createdAt"},
						{Key: "timezone", Value: "UTC"},
					}},
				}},
				{Key: "s", Value: "$source"},
				{Key: "k", Value: "$kind"},
			}},
			{Key: "count", Value: bson.D{{Key: "$sum", Value: 1}}},
			{Key: "value", Value: bson.D{{Key: "$sum", Value: bson.D{{Key: "$ifNull", Value: bson.A{"$value", 0}}}}}},
		}}},
	}
	cur, err := r.col.Aggregate(ctx, pipeline)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)

	var out []repository.DailySourceKindStat
	for cur.Next(ctx) {
		var row struct {
			ID struct {
				Date   string `bson:"d"`
				Source string `bson:"s"`
				Kind   string `bson:"k"`
			} `bson:"_id"`
			Count int     `bson:"count"`
			Value float64 `bson:"value"`
		}
		if err := cur.Decode(&row); err != nil {
			return nil, err
		}
		out = append(out, repository.DailySourceKindStat{
			Date: row.ID.Date, Source: row.ID.Source, Kind: row.ID.Kind,
			Count: row.Count, Value: row.Value,
		})
	}
	return out, cur.Err()
}

// UniqueVisitors counts distinct visitor hashes on store-visit events.
func (r *Analytics) UniqueVisitors(ctx context.Context, sellerID string, since time.Time) (int, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	pipeline := mongo.Pipeline{
		{{Key: "$match", Value: bson.D{
			{Key: "sellerId", Value: sellerID},
			{Key: "kind", Value: models.AnalyticsEventTraffic},
			{Key: "visitor", Value: bson.D{{Key: "$exists", Value: true}, {Key: "$ne", Value: ""}}},
			{Key: "createdAt", Value: bson.D{{Key: "$gte", Value: since}}},
		}}},
		{{Key: "$group", Value: bson.D{{Key: "_id", Value: "$visitor"}}}},
		{{Key: "$count", Value: "n"}},
	}
	cur, err := r.col.Aggregate(ctx, pipeline)
	if err != nil {
		return 0, err
	}
	defer cur.Close(ctx)
	if !cur.Next(ctx) {
		return 0, cur.Err()
	}
	var row struct {
		N int `bson:"n"`
	}
	if err := cur.Decode(&row); err != nil {
		return 0, err
	}
	return row.N, nil
}
