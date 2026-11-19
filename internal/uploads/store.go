// Package uploads keeps one MongoDB record per processed image: who owns it,
// which object key it lives at, and where it is in the pipeline.
// Only generated keys and minimal metadata are stored, never original
// filenames or client-supplied MIME types.
package uploads

import (
	"context"
	"errors"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const Collection = "uploads"

// Status of an upload.
const (
	StatusPending    = "pending"
	StatusProcessing = "processing"
	StatusApproved   = "approved" // sanitized, stored and servable
	StatusRejected   = "rejected" // failed validation; never stored in R2
)

var ErrNotFound = errors.New("uploads: not found")

// Upload is stored in the "uploads" collection.
type Upload struct {
	ID        string    `bson:"_id"`
	OwnerID   string    `bson:"ownerId"`
	Role      string    `bson:"role"`
	Kind      string    `bson:"kind"`
	ProductID string    `bson:"productId,omitempty"`
	Key       string    `bson:"key,omitempty"` // set once stored
	Status    string    `bson:"status"`
	Reason    string    `bson:"reason,omitempty"` // safe reject reason
	Bytes     int64     `bson:"bytes,omitempty"`  // size of the stored WebP
	Width     int       `bson:"width,omitempty"`
	Height    int       `bson:"height,omitempty"`
	SHA256    string    `bson:"sha256,omitempty"` // of the stored WebP
	CreatedAt time.Time `bson:"createdAt"`
	UpdatedAt time.Time `bson:"updatedAt"`
}

type Store struct{ col *mongo.Collection }

func NewStore(db *mongo.Database) *Store { return &Store{col: db.Collection(Collection)} }

// EnsureIndexes creates the indexes the queries below rely on.
func (s *Store) EnsureIndexes(ctx context.Context) error {
	_, err := s.col.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "ownerId", Value: 1}, {Key: "status", Value: 1}}, Options: options.Index().SetName("idx_uploads_owner_status")},
		{Keys: bson.D{{Key: "ownerId", Value: 1}, {Key: "createdAt", Value: -1}}, Options: options.Index().SetName("idx_uploads_owner_created")},
		{Keys: bson.D{{Key: "key", Value: 1}}, Options: options.Index().SetName("uniq_uploads_key").SetUnique(true).
			SetPartialFilterExpression(bson.D{{Key: "key", Value: bson.D{{Key: "$type", Value: "string"}}}})},
		{Keys: bson.D{{Key: "status", Value: 1}, {Key: "updatedAt", Value: 1}}, Options: options.Index().SetName("idx_uploads_status_updated")},
	})
	return err
}

func (s *Store) Insert(ctx context.Context, u *Upload) error {
	_, err := s.col.InsertOne(ctx, u)
	return err
}

// Finish moves a record to its final state. Only a record still in
// "processing" can be finished, so a stale worker cannot overwrite a result.
func (s *Store) Finish(ctx context.Context, id, status, reason string, set bson.M) error {
	if set == nil {
		set = bson.M{}
	}
	set["status"], set["reason"], set["updatedAt"] = status, reason, time.Now().UTC()
	res, err := s.col.UpdateOne(ctx, bson.M{"_id": id, "status": StatusProcessing}, bson.M{"$set": set})
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		return ErrNotFound
	}
	return nil
}

// GetOwned returns the record only if it belongs to ownerID. A record that
// exists but belongs to someone else is indistinguishable from a missing one.
func (s *Store) GetOwned(ctx context.Context, ownerID, id string) (*Upload, error) {
	var u Upload
	err := s.col.FindOne(ctx, bson.M{"_id": id, "ownerId": ownerID}).Decode(&u)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, ErrNotFound
	}
	return &u, err
}

// GetByKeyOwned finds an approved upload by object key, owner-scoped.
func (s *Store) GetByKeyOwned(ctx context.Context, ownerID, key string) (*Upload, error) {
	var u Upload
	err := s.col.FindOne(ctx, bson.M{"key": key, "ownerId": ownerID, "status": StatusApproved}).Decode(&u)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, ErrNotFound
	}
	return &u, err
}

// Delete removes a record, scoped to its owner.
func (s *Store) Delete(ctx context.Context, ownerID, id string) error {
	res, err := s.col.DeleteOne(ctx, bson.M{"_id": id, "ownerId": ownerID})
	if err != nil {
		return err
	}
	if res.DeletedCount == 0 {
		return ErrNotFound
	}
	return nil
}

// Usage returns how many bytes and files an owner has stored, and how many
// uploads they started since `since` (for abuse limits).
func (s *Store) Usage(ctx context.Context, ownerID string, since time.Time) (bytes int64, files int, recent int64, err error) {
	cur, err := s.col.Aggregate(ctx, mongo.Pipeline{
		{{Key: "$match", Value: bson.M{"ownerId": ownerID, "status": StatusApproved}}},
		{{Key: "$group", Value: bson.M{"_id": nil, "bytes": bson.M{"$sum": "$bytes"}, "n": bson.M{"$sum": 1}}}},
	})
	if err != nil {
		return 0, 0, 0, err
	}
	var rows []struct {
		Bytes int64 `bson:"bytes"`
		N     int   `bson:"n"`
	}
	if err := cur.All(ctx, &rows); err != nil {
		return 0, 0, 0, err
	}
	if len(rows) > 0 {
		bytes, files = rows[0].Bytes, rows[0].N
	}
	recent, err = s.col.CountDocuments(ctx, bson.M{"ownerId": ownerID, "createdAt": bson.M{"$gte": since}})
	return
}

// RejectedSince counts an owner's rejected uploads, a signal of probing.
func (s *Store) RejectedSince(ctx context.Context, ownerID string, since time.Time) (int64, error) {
	return s.col.CountDocuments(ctx, bson.M{"ownerId": ownerID, "status": StatusRejected, "createdAt": bson.M{"$gte": since}})
}

// Stale lists records stuck in a state longer than `olderThan`.
func (s *Store) Stale(ctx context.Context, status string, olderThan time.Time, limit int64) ([]Upload, error) {
	cur, err := s.col.Find(ctx, bson.M{"status": status, "updatedAt": bson.M{"$lt": olderThan}}, options.Find().SetLimit(limit))
	if err != nil {
		return nil, err
	}
	var out []Upload
	return out, cur.All(ctx, &out)
}

// DeleteByID removes a record regardless of owner; cleanup job only.
func (s *Store) DeleteByID(ctx context.Context, id string) error {
	_, err := s.col.DeleteOne(ctx, bson.M{"_id": id})
	return err
}

// StaleUnattached lists approved uploads older than `olderThan` whose key is
// not in `used`. The caller decides what "used" means (see Cleanup).
func (s *Store) Approved(ctx context.Context, olderThan time.Time, limit int64) ([]Upload, error) {
	cur, err := s.col.Find(ctx, bson.M{"status": StatusApproved, "createdAt": bson.M{"$lt": olderThan}}, options.Find().SetLimit(limit))
	if err != nil {
		return nil, err
	}
	var out []Upload
	return out, cur.All(ctx, &out)
}
