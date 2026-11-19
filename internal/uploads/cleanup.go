package uploads

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Deleter removes objects from the bucket (satisfied by *storage.R2).
type Deleter interface {
	DeleteObject(ctx context.Context, key string) error
}

// Cleaner removes leftovers:
//   - uploads stuck in "processing" (crash between R2 write and DB update):
//     the object and the record are removed;
//   - old "rejected" records (kept 30 days for abuse review);
//   - orphans: approved product images no product or order line references.
//     Orphan deletion only runs when DeleteOrphans is set; otherwise the
//     orphans are only counted in the log, because order history and other
//     areas may still point at an image.
type Cleaner struct {
	Store         *Store
	R2            Deleter
	DB            *mongo.Database
	Log           *slog.Logger
	PublicBase    string // R2 public base URL, to rebuild an image's URL from its key
	DeleteOrphans bool
	OrphanAge     time.Duration // default 14 days
}

func (c *Cleaner) Run(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		c.once(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (c *Cleaner) once(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	now := time.Now().UTC()

	stale, err := c.Store.Stale(ctx, StatusProcessing, now.Add(-15*time.Minute), 200)
	if err != nil {
		c.Log.Error("uploads cleanup: list processing", "error", err.Error())
		return
	}
	for _, u := range stale {
		if u.Key != "" {
			if err := c.R2.DeleteObject(ctx, u.Key); err != nil {
				c.Log.Error("uploads cleanup: delete object", "error", err.Error())
				continue
			}
		}
		_ = c.Store.DeleteByID(ctx, u.ID)
	}
	old, _ := c.Store.Stale(ctx, StatusRejected, now.Add(-30*24*time.Hour), 500)
	for _, u := range old {
		_ = c.Store.DeleteByID(ctx, u.ID)
	}

	age := c.OrphanAge
	if age <= 0 {
		age = 14 * 24 * time.Hour
	}
	cands, err := c.Store.Approved(ctx, now.Add(-age), 200)
	if err != nil || c.DB == nil {
		return
	}
	orphans := 0
	for _, u := range cands {
		if u.Kind != "product" {
			continue // other kinds are referenced from designs/looks we cannot scan safely
		}
		url := strings.TrimRight(c.PublicBase, "/") + "/" + u.Key
		used, err := c.referenced(ctx, url)
		if err != nil || used {
			continue
		}
		orphans++
		if c.DeleteOrphans {
			if err := c.R2.DeleteObject(ctx, u.Key); err == nil {
				_ = c.Store.DeleteByID(ctx, u.ID)
			}
		}
	}
	if orphans > 0 {
		c.Log.Info("uploads cleanup: orphaned product images", "count", orphans, "deleted", c.DeleteOrphans)
	}
}

func (c *Cleaner) referenced(ctx context.Context, url string) (bool, error) {
	n, err := c.DB.Collection("products").CountDocuments(ctx, bson.M{"$or": bson.A{
		bson.M{"mainImageUrl": url}, bson.M{"imageUrls": url}, bson.M{"combinations.imageUrls": url},
	}}, countOne)
	if err != nil || n > 0 {
		return n > 0, err
	}
	n, err = c.DB.Collection("orders").CountDocuments(ctx, bson.M{"lines.imageUrl": url}, countOne)
	return n > 0, err
}

var countOne = options.Count().SetLimit(1)
