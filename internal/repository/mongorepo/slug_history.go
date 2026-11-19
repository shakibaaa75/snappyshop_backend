package mongorepo

import (
	"context"
	"regexp"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"shopbd/backend/internal/database"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/repository"
)

// SlugHistory implements repository.SlugHistoryRepository.
type SlugHistory struct{ db *mongo.Database }

func NewSlugHistory(db *mongo.Database) *SlugHistory { return &SlugHistory{db: db} }

func (r *SlugHistory) col() *mongo.Collection {
	return r.db.Collection(database.StoreSlugHistoryCollection)
}

// Record appends one change. A repeat of the same seller/old/new pair is
// ignored, so recording is safe to retry.
func (r *SlugHistory) Record(ctx context.Context, c *models.StoreSlugChange) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	return recordSlugChange(ctx, r.col(), c)
}

// recordSlugChange upserts on (sellerId, oldSlug, newSlug); $setOnInsert keeps
// the first record if the same change is written twice.
func recordSlugChange(ctx context.Context, col *mongo.Collection, c *models.StoreSlugChange) error {
	if c.ID == "" {
		c.ID = models.NewID()
	}
	if c.CreatedAt.IsZero() {
		c.CreatedAt = time.Now().UTC()
	}
	set := bson.D{
		{Key: "_id", Value: c.ID},
		{Key: "source", Value: c.Source},
		{Key: "createdAt", Value: c.CreatedAt},
	}
	if c.StoreName != "" {
		set = append(set, bson.E{Key: "storeName", Value: c.StoreName})
	}
	if c.ChangedAt != nil {
		set = append(set, bson.E{Key: "changedAt", Value: c.ChangedAt.UTC()})
	}
	_, err := col.UpdateOne(ctx,
		bson.D{
			{Key: "sellerId", Value: c.SellerID},
			{Key: "oldSlug", Value: c.OldSlug},
			{Key: "newSlug", Value: c.NewSlug},
		},
		bson.D{{Key: "$setOnInsert", Value: set}},
		options.Update().SetUpsert(true),
	)
	return err
}

func (r *SlugHistory) ListBySeller(ctx context.Context, sellerID string, limit int) ([]*models.StoreSlugChange, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	cur, err := r.col().Find(ctx, bson.D{{Key: "sellerId", Value: sellerID}},
		options.Find().SetSort(historySort()).SetLimit(int64(limit)))
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var out []*models.StoreSlugChange
	for cur.Next(ctx) {
		var c models.StoreSlugChange
		if err := cur.Decode(&c); err != nil {
			return nil, err
		}
		out = append(out, &c)
	}
	return out, cur.Err()
}

// historySort is newest first; legacy rows without a date sort last.
func historySort() bson.D {
	return bson.D{{Key: "changedAt", Value: -1}, {Key: "createdAt", Value: -1}}
}

func (r *SlugHistory) List(ctx context.Context, f repository.SlugHistoryFilter) ([]*repository.SlugChangeItem, int64, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()

	q := bson.D{}
	if f.SellerID != "" {
		q = append(q, bson.E{Key: "sellerId", Value: f.SellerID})
	}
	if s := strings.TrimSpace(f.Search); s != "" {
		rx := bson.D{{Key: "$regex", Value: regexp.QuoteMeta(s)}, {Key: "$options", Value: "i"}}
		or := bson.A{
			bson.D{{Key: "oldSlug", Value: rx}},
			bson.D{{Key: "newSlug", Value: rx}},
		}
		// Also match by seller name / email.
		ucur, err := r.db.Collection(database.UsersCollection).Find(ctx,
			bson.D{
				{Key: "role", Value: models.RoleSeller},
				{Key: "$or", Value: bson.A{
					bson.D{{Key: "name", Value: rx}},
					bson.D{{Key: "email", Value: rx}},
				}},
			},
			options.Find().SetProjection(bson.D{{Key: "_id", Value: 1}}).SetLimit(500),
		)
		if err != nil {
			return nil, 0, err
		}
		var ids []string
		for ucur.Next(ctx) {
			var u struct {
				ID string `bson:"_id"`
			}
			if ucur.Decode(&u) == nil {
				ids = append(ids, u.ID)
			}
		}
		_ = ucur.Close(ctx)
		if len(ids) > 0 {
			or = append(or, bson.D{{Key: "sellerId", Value: bson.D{{Key: "$in", Value: ids}}}})
		}
		q = append(q, bson.E{Key: "$or", Value: or})
	}

	total, err := r.col().CountDocuments(ctx, q)
	if err != nil {
		return nil, 0, err
	}
	limit := f.Limit
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	skip := f.Skip
	if skip < 0 {
		skip = 0
	}
	cur, err := r.col().Find(ctx, q,
		options.Find().SetSort(historySort()).SetLimit(int64(limit)).SetSkip(int64(skip)))
	if err != nil {
		return nil, 0, err
	}
	defer cur.Close(ctx)

	var changes []*models.StoreSlugChange
	idSet := map[string]struct{}{}
	for cur.Next(ctx) {
		var c models.StoreSlugChange
		if err := cur.Decode(&c); err != nil {
			return nil, 0, err
		}
		changes = append(changes, &c)
		idSet[c.SellerID] = struct{}{}
	}
	if err := cur.Err(); err != nil {
		return nil, 0, err
	}
	if len(changes) == 0 {
		return nil, total, nil
	}

	// Seller details and current address: two batched queries, not one per row.
	ids := make([]string, 0, len(idSet))
	for id := range idSet {
		ids = append(ids, id)
	}
	users := map[string]models.User{}
	uc, err := r.db.Collection(database.UsersCollection).Find(ctx,
		bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: ids}}}})
	if err != nil {
		return nil, 0, err
	}
	for uc.Next(ctx) {
		var u models.User
		if uc.Decode(&u) == nil {
			users[u.ID] = u
		}
	}
	_ = uc.Close(ctx)

	current := map[string]string{}
	sc, err := r.db.Collection(database.StoreDesignsCollection).Find(ctx,
		bson.D{{Key: "sellerId", Value: bson.D{{Key: "$in", Value: ids}}}},
		options.Find().
			SetProjection(bson.D{{Key: "sellerId", Value: 1}, {Key: "slug", Value: 1}}).
			SetSort(bson.D{{Key: "updatedAt", Value: 1}}), // latest document wins
	)
	if err != nil {
		return nil, 0, err
	}
	for sc.Next(ctx) {
		var d struct {
			SellerID string `bson:"sellerId"`
			Slug     string `bson:"slug"`
		}
		if sc.Decode(&d) == nil {
			current[d.SellerID] = d.Slug
		}
	}
	_ = sc.Close(ctx)

	out := make([]*repository.SlugChangeItem, 0, len(changes))
	for _, c := range changes {
		u := users[c.SellerID]
		out = append(out, &repository.SlugChangeItem{
			Change:       c,
			SellerName:   u.Name,
			SellerEmail:  u.Email,
			SellerStatus: u.Status,
			CurrentSlug:  current[c.SellerID],
		})
	}
	return out, total, nil
}

// BackfillLegacy turns each store's previousSlugs list into history rows so
// changes made before history was recorded still show up. The list is ordered
// oldest first, so the chain is a->b, b->c, ..., last->current. Only the last
// hop has a known date (slugChangedAt); earlier hops are stored without one.
func (r *SlugHistory) BackfillLegacy(ctx context.Context) (int, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()

	stores := r.db.Collection(database.StoreDesignsCollection)
	cur, err := stores.Find(ctx,
		bson.D{{Key: "previousSlugs.0", Value: bson.D{{Key: "$exists", Value: true}}}},
		options.Find().SetProjection(bson.D{
			{Key: "sellerId", Value: 1}, {Key: "slug", Value: 1},
			{Key: "previousSlugs", Value: 1}, {Key: "slugChangedAt", Value: 1},
		}),
	)
	if err != nil {
		return 0, err
	}
	defer cur.Close(ctx)

	added := 0
	for cur.Next(ctx) {
		var d struct {
			SellerID      string     `bson:"sellerId"`
			Slug          string     `bson:"slug"`
			PreviousSlugs []string   `bson:"previousSlugs"`
			SlugChangedAt *time.Time `bson:"slugChangedAt"`
		}
		if err := cur.Decode(&d); err != nil {
			return added, err
		}
		chain := append(append([]string{}, d.PreviousSlugs...), d.Slug)
		for i := 0; i+1 < len(chain); i++ {
			if chain[i] == "" || chain[i+1] == "" || chain[i] == chain[i+1] {
				continue
			}
			// Skip when this hop (or a real record of it) already exists.
			n, err := r.col().CountDocuments(ctx, bson.D{
				{Key: "sellerId", Value: d.SellerID},
				{Key: "oldSlug", Value: chain[i]},
				{Key: "newSlug", Value: chain[i+1]},
			}, options.Count().SetLimit(1))
			if err != nil {
				return added, err
			}
			if n > 0 {
				continue
			}
			c := &models.StoreSlugChange{
				SellerID: d.SellerID, OldSlug: chain[i], NewSlug: chain[i+1],
				Source: models.SlugChangeLegacy,
			}
			if i+2 == len(chain) && d.SlugChangedAt != nil && !d.SlugChangedAt.IsZero() {
				t := d.SlugChangedAt.UTC()
				c.ChangedAt = &t
			}
			if err := recordSlugChange(ctx, r.col(), c); err != nil {
				return added, err
			}
			added++
		}
	}
	return added, cur.Err()
}

var _ repository.SlugHistoryRepository = (*SlugHistory)(nil)
