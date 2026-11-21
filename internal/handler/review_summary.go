package handler

import (
	"context"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"shopbd/backend/internal/database"
	"time"
)

// Product cards use a small denormalized summary. Public review endpoints always
// read current order evidence. A cold start repairs changes made during downtime.
func (h Reviews) RunSummaryWorker(ctx context.Context) {
	var since time.Time
	for ctx.Err() == nil {
		started := time.Now().UTC()
		if err := h.refreshChanged(ctx, since); err == nil {
			since = started.Add(-2 * time.Second)
		} else if ctx.Err() == nil {
			h.Log.Warn("review summary refresh failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(20 * time.Second):
		}
	}
}
func (h Reviews) refreshChanged(ctx context.Context, since time.Time) error {
	products := map[string]bool{}
	scan := func(col *mongo.Collection, filter bson.M, field string, visit func(string) error) error {
		cur, err := col.Find(ctx, filter, options.Find().SetProjection(bson.M{field: 1}))
		if err != nil {
			return err
		}
		defer cur.Close(ctx)
		for cur.Next(ctx) {
			value, ok := cur.Current.Lookup(field).StringValueOK()
			if ok {
				if err := visit(value); err != nil {
					return err
				}
			}
		}
		return cur.Err()
	}
	remember := func(p string) error { products[p] = true; return nil }
	filter := bson.M{}
	if !since.IsZero() {
		filter["updated"] = bson.M{"$gte": since}
	}
	if err := scan(h.reviews(), filter, "product", remember); err != nil {
		return err
	}
	if !since.IsZero() {
		err := scan(h.orders(), bson.M{"updatedAt": bson.M{"$gte": since}, "buyerId": bson.M{"$exists": true}}, "_id", func(id string) error {
			return scan(h.reviews(), bson.M{"order": id}, "product", remember)
		})
		if err != nil {
			return err
		}
	}
	for product := range products {
		if err := h.refreshProduct(ctx, product); err != nil {
			return err
		}
	}
	return nil
}
func (h Reviews) refreshProduct(ctx context.Context, product string) error {
	p := append(reviewPipeline(bson.M{"product": product}), bson.M{"$project": bson.M{"status": 1, "recorded": 1, "revision": 1, "rating": 1}})
	cur, err := h.reviews().Aggregate(ctx, p)
	if err != nil {
		return err
	}
	defer cur.Close(ctx)
	sum, count := 0, 0
	for cur.Next(ctx) {
		var row struct {
			ID               string `bson:"_id"`
			Status, Recorded string
			Revision, Rating int
		}
		if err := cur.Decode(&row); err != nil {
			return err
		}
		if row.Status == "Published" {
			count++
			sum += row.Rating
		}
		if row.Status == row.Recorded {
			continue
		}
		set := bson.M{"recorded": row.Status}
		if row.Status == "Published" {
			set["everPublished"] = true
		}
		filter := bson.M{"_id": row.ID, "revision": row.Revision, "recorded": row.Recorded}
		if row.Recorded == "" {
			filter["recorded"] = nil
		}
		_, err = h.reviews().UpdateOne(ctx, filter, bson.M{"$set": set, "$push": bson.M{"audit": bson.M{"action": "status", "detail": row.Status, "at": time.Now().UTC(), "actor": "system"}}})
		if err != nil {
			return err
		}
	}
	if err := cur.Err(); err != nil {
		return err
	}
	avg := 0.0
	if count > 0 {
		avg = float64(sum) / float64(count)
	}
	_, err = h.DB.Collection(database.ProductsCollection).UpdateOne(ctx, bson.M{"_id": product}, bson.M{"$set": bson.M{"reviewAverage": avg, "reviewCount": count}})
	return err
}

func (h Reviews) refreshAfterChange(ctx context.Context, product string) {
	if err := h.refreshProduct(ctx, product); err != nil {
		h.Log.Warn("review summary queued for retry", "product", product, "error", err)
	}
}
func (h Reviews) RefreshOrder(ctx context.Context, id string) {
	products, err := h.reviews().Distinct(ctx, "product", bson.M{"order": id})
	if err != nil {
		h.Log.Warn("order review refresh queued for retry", "error", err)
		return
	}
	for _, value := range products {
		if product, ok := value.(string); ok {
			h.refreshAfterChange(ctx, product)
		}
	}
}
