package mongorepo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"shopbd/backend/internal/database"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/repository"
)

// StoreDesigns implements repository.StoreDesignRepository.
type StoreDesigns struct{ col *mongo.Collection }

// storeDesignDoc mirrors the stored document. Draft and Published are read as
// raw BSON because the driver would otherwise decode a nested design into
// bson.D (a list of key/value pairs), which is not a usable map and loses the
// design when it is turned back into JSON.
type storeDesignDoc struct {
	ID            string     `bson:"_id"`
	SellerID      string     `bson:"sellerId"`
	Slug          string     `bson:"slug"`
	PreviousSlugs []string   `bson:"previousSlugs,omitempty"`
	SlugChangedAt *time.Time `bson:"slugChangedAt,omitempty"`
	StoreName     string     `bson:"storeName,omitempty"`
	NameChanges   int        `bson:"nameChangeCount,omitempty"`
	Draft         bson.Raw   `bson:"draft"`
	Published     bson.Raw   `bson:"published,omitempty"`
	IsPublished   bool       `bson:"isPublished"`
	CreatedAt     time.Time  `bson:"createdAt"`
	UpdatedAt     time.Time  `bson:"updatedAt"`
}

func rawToMap(raw bson.Raw) (map[string]any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	js, err := bson.MarshalExtJSON(raw, false, false)
	if err != nil {
		return nil, fmt.Errorf("store design: encode: %w", err)
	}
	var m map[string]any
	if err := json.Unmarshal(js, &m); err != nil {
		return nil, fmt.Errorf("store design: decode: %w", err)
	}
	return m, nil
}

func (d *storeDesignDoc) toModel() (*models.StoreDesign, error) {
	out := &models.StoreDesign{
		ID:              d.ID,
		SellerID:        d.SellerID,
		Slug:            d.Slug,
		PreviousSlugs:   d.PreviousSlugs,
		SlugChangedAt:   d.SlugChangedAt,
		StoreName:       d.StoreName,
		NameChangeCount: d.NameChanges,
		IsPublished:     d.IsPublished,
		CreatedAt:       d.CreatedAt,
		UpdatedAt:       d.UpdatedAt,
	}
	draft, err := rawToMap(d.Draft)
	if err != nil {
		return nil, err
	}
	if draft != nil {
		out.Draft = draft
	}
	published, err := rawToMap(d.Published)
	if err != nil {
		return nil, err
	}
	if published != nil {
		out.Published = published
	}
	// Documents created before the name was stored at document level: fall
	// back to the name inside the design payload.
	if out.StoreName == "" {
		if m, ok := draft["storeName"].(string); ok {
			out.StoreName = strings.TrimSpace(m)
		}
	}
	return out, nil
}

func NewStoreDesigns(db *mongo.Database) *StoreDesigns {
	return &StoreDesigns{col: db.Collection(database.StoreDesignsCollection)}
}

func (r *StoreDesigns) GetBySeller(ctx context.Context, sellerID string) (*models.StoreDesign, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	var d storeDesignDoc
	// One document per seller is the rule. If stale duplicates ever exist,
	// always read the most recently updated one (the one with the latest slug).
	err := r.col.FindOne(ctx, bson.D{{Key: "sellerId", Value: sellerID}},
		options.FindOne().SetSort(bson.D{{Key: "updatedAt", Value: -1}}),
	).Decode(&d)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, repository.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return d.toModel()
}

// sellerStoreHidden reports whether the seller's account status hides their
// store from the public (suspended or banned). A missing user is not hidden,
// so stores created without a user document keep working.
func (r *StoreDesigns) sellerStoreHidden(ctx context.Context, sellerID string) (bool, error) {
	var u struct {
		Status models.Status `bson:"status"`
	}
	err := r.col.Database().Collection(database.UsersCollection).FindOne(ctx,
		bson.D{{Key: "_id", Value: sellerID}},
		options.FindOne().SetProjection(bson.D{{Key: "status", Value: 1}}),
	).Decode(&u)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return !u.Status.StoreVisible(), nil
}

func (r *StoreDesigns) GetPublishedBySlug(ctx context.Context, slug string) (*models.StoreDesign, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()

	// Permanently invalid: any slug that was retired must never resolve,
	// even if something still pointed at it.
	retired, err := r.col.CountDocuments(ctx,
		bson.D{{Key: "previousSlugs", Value: slug}},
		options.Count().SetLimit(1),
	)
	if err != nil {
		return nil, err
	}
	if retired > 0 {
		return nil, repository.ErrNotFound
	}

	// Only the *current* slug of a published store is valid.
	var d storeDesignDoc
	err = r.col.FindOne(ctx, bson.D{
		{Key: "slug", Value: slug},
		{Key: "isPublished", Value: true},
	}).Decode(&d)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, repository.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	// A suspended or banned seller's store is not public, so the storefront,
	// its products, categories, checkout and tracking all answer "not found".
	hidden, err := r.sellerStoreHidden(ctx, d.SellerID)
	if err != nil {
		return nil, err
	}
	if hidden {
		return nil, repository.ErrNotFound
	}
	return d.toModel()
}

func (r *StoreDesigns) SlugTaken(ctx context.Context, slug, exceptSellerID string) (bool, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	// Current slug of someone else, OR any retired slug (including this seller's).
	filter := bson.D{
		{Key: "$or", Value: bson.A{
			bson.D{
				{Key: "slug", Value: slug},
				{Key: "sellerId", Value: bson.D{{Key: "$ne", Value: exceptSellerID}}},
			},
			bson.D{{Key: "previousSlugs", Value: slug}},
		}},
	}
	n, err := r.col.CountDocuments(ctx, filter, options.Count().SetLimit(1))
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (r *StoreDesigns) UpsertDraft(ctx context.Context, p repository.UpsertDraftParams) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	now := time.Now().UTC()

	set := bson.D{
		{Key: "slug", Value: p.Slug},
		{Key: "draft", Value: p.Draft},
		{Key: "updatedAt", Value: now},
	}
	if p.StoreName != "" {
		set = append(set, bson.E{Key: "storeName", Value: p.StoreName})
	}

	update := bson.D{
		{Key: "$set", Value: set},
		{Key: "$setOnInsert", Value: bson.D{
			{Key: "_id", Value: models.NewID()},
			{Key: "sellerId", Value: p.SellerID},
			{Key: "isPublished", Value: false},
			{Key: "createdAt", Value: now},
		}},
	}

	if p.SlugChanged && p.OldSlug != "" {
		set = append(set,
			bson.E{Key: "slugChangedAt", Value: p.ChangedAt.UTC()},
			// Keep published payload's storeSlug in sync so nothing still
			// advertises the retired address.
			bson.E{Key: "published.storeSlug", Value: p.Slug},
		)
		update = bson.D{
			{Key: "$set", Value: set},
			{Key: "$addToSet", Value: bson.D{{Key: "previousSlugs", Value: p.OldSlug}}},
			{Key: "$setOnInsert", Value: bson.D{
				{Key: "_id", Value: models.NewID()},
				{Key: "sellerId", Value: p.SellerID},
				{Key: "isPublished", Value: false},
				{Key: "createdAt", Value: now},
			}},
		}
	}

	_, err := r.col.UpdateOne(ctx,
		bson.D{{Key: "sellerId", Value: p.SellerID}},
		update,
		options.Update().SetUpsert(true),
	)
	if mongo.IsDuplicateKeyError(err) {
		return repository.ErrSlugTaken
	}
	return err
}

func (r *StoreDesigns) Publish(ctx context.Context, sellerID string, draft any) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	now := time.Now().UTC()

	set := bson.D{
		{Key: "published", Value: draft},
		{Key: "isPublished", Value: true},
		{Key: "updatedAt", Value: now},
	}
	// If the design carries a storeSlug, keep the document-level slug aligned
	// so public lookup and the published payload never disagree.
	if m, ok := draft.(map[string]any); ok {
		if s, ok := m["storeSlug"].(string); ok {
			s = strings.ToLower(strings.TrimSpace(s))
			if s != "" {
				set = append(set, bson.E{Key: "slug", Value: s})
			}
		}
	}

	res, err := r.col.UpdateOne(ctx,
		bson.D{{Key: "sellerId", Value: sellerID}},
		bson.D{{Key: "$set", Value: set}},
	)
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		return repository.ErrNotFound
	}
	return nil
}

// ApplyIdentityChange renames the shop and/or moves it to a new URL in ONE
// conditional update. The filter re-checks the preconditions (current URL,
// remaining renames, 30-day lock) so two concurrent requests can never both
// succeed. Afterwards the references stored in other collections are moved to
// the new URL as well.
func (r *StoreDesigns) ApplyIdentityChange(ctx context.Context, p repository.IdentityChangeParams) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()

	slugChanged := p.NewSlug != p.OldSlug

	filter := bson.D{
		{Key: "sellerId", Value: p.SellerID},
		{Key: "slug", Value: p.OldSlug},
		{Key: "$or", Value: bson.A{
			bson.D{{Key: "slugChangedAt", Value: bson.D{{Key: "$exists", Value: false}}}},
			bson.D{{Key: "slugChangedAt", Value: bson.D{{Key: "$lte", Value: p.LockedBefore.UTC()}}}},
		}},
	}
	if p.CountName {
		// Matches documents where the counter is missing (0) or below the cap.
		filter = append(filter, bson.E{Key: "nameChangeCount", Value: bson.D{
			{Key: "$not", Value: bson.D{{Key: "$gte", Value: p.MaxNameChanges}}},
		}})
	}
	if p.Published {
		filter = append(filter, bson.E{Key: "isPublished", Value: true})
	}

	set := bson.D{
		{Key: "slug", Value: p.NewSlug},
		{Key: "slugChangedAt", Value: p.ChangedAt.UTC()},
		{Key: "updatedAt", Value: p.ChangedAt.UTC()},
		{Key: "draft.storeSlug", Value: p.NewSlug},
	}
	if p.NewName != "" {
		set = append(set,
			bson.E{Key: "storeName", Value: p.NewName},
			bson.E{Key: "draft.storeName", Value: p.NewName},
		)
	}
	if p.Published {
		set = append(set, bson.E{Key: "published.storeSlug", Value: p.NewSlug})
		if p.NewName != "" {
			set = append(set, bson.E{Key: "published.storeName", Value: p.NewName})
		}
	}

	update := bson.D{{Key: "$set", Value: set}}
	if slugChanged {
		update = append(update, bson.E{Key: "$addToSet", Value: bson.D{
			{Key: "previousSlugs", Value: p.OldSlug},
		}})
	}
	if p.CountName {
		update = append(update, bson.E{Key: "$inc", Value: bson.D{{Key: "nameChangeCount", Value: 1}}})
	}

	res, err := r.col.UpdateOne(ctx, filter, update)
	if mongo.IsDuplicateKeyError(err) {
		return repository.ErrSlugTaken
	}
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		return repository.ErrIdentityConflict
	}

	if slugChanged {
		// Remember the change for the admin "slug history". Best effort: the
		// change itself has already been applied, and BackfillLegacy rebuilds
		// any row that could not be written from previousSlugs on next start.
		source := p.Source
		if source == "" {
			source = models.SlugChangeAddress
		}
		changedAt := p.ChangedAt.UTC()
		_ = recordSlugChange(ctx, r.col.Database().Collection(database.StoreSlugHistoryCollection), &models.StoreSlugChange{
			SellerID:  p.SellerID,
			OldSlug:   p.OldSlug,
			NewSlug:   p.NewSlug,
			StoreName: p.NewName,
			Source:    source,
			ChangedAt: &changedAt,
		})

		// Payment records keep the store address for the gateway redirect.
		// Move them to the new address so nothing still points at the old one.
		pay := r.col.Database().Collection(database.SubscriptionPaymentsCollection)
		if _, err := pay.UpdateMany(ctx,
			bson.D{{Key: "sellerId", Value: p.SellerID}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "slug", Value: p.NewSlug}}}},
		); err != nil {
			return err
		}
	}
	return nil
}
