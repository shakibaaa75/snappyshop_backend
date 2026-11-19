// backfillvariantprices gives every product variant its own stored price.
//
// Until now a variant saved without a price silently used the product's price
// at checkout. Checkout no longer does that: a variant with no price of its own
// cannot be ordered. Variants saved before this change may still have none, so
// run this once before (or right after) deploying, or those variants show up as
// "can't be ordered right now".
//
// For each such variant it writes the product's CURRENT price into the variant,
// which is exactly what buyers were being charged until now. Nothing else on
// the product changes (not even updatedAt).
//
// It is a dry run unless -apply is given, and safe to run more than once:
// variants that already have a price are never touched, and a product the
// seller edited while the command ran is skipped (re-run to pick it up).
//
// It reads MONGODB_URI and MONGODB_DB from the environment or from a .env file
// in the directory you run it from, like the API server does.
//
//	go run ./cmd/backfillvariantprices           # dry run
//	go run ./cmd/backfillvariantprices -apply
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/joho/godotenv"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"

	"shopbd/backend/internal/database"
)

// needsPrice matches a variant whose price is missing, zero or negative.
var needsPrice = bson.M{"$elemMatch": bson.M{"price": bson.M{"$not": bson.M{"$gt": 0}}}}

type row struct {
	ID           string    `bson:"_id"`
	SellerID     string    `bson:"sellerId"`
	Name         string    `bson:"name"`
	Price        float64   `bson:"price"`
	UpdatedAt    time.Time `bson:"updatedAt"`
	Combinations []struct {
		ID    string  `bson:"id"`
		Price float64 `bson:"price"`
	} `bson:"combinations"`
}

func main() {
	_ = godotenv.Load() // optional: same as the API server
	apply := flag.Bool("apply", false, "write changes (default is a dry run)")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	if os.Getenv("MONGODB_URI") == "" || os.Getenv("MONGODB_DB") == "" {
		fmt.Println("MONGODB_URI and MONGODB_DB must be set (in the environment or a .env file in this directory)")
		os.Exit(2)
	}
	client, err := database.Connect(ctx, os.Getenv("MONGODB_URI"))
	if err != nil {
		fmt.Println("mongo:", err)
		os.Exit(1)
	}
	defer client.Disconnect(context.Background())
	col := client.Database(os.Getenv("MONGODB_DB")).Collection(database.ProductsCollection)

	// Products whose own price is not positive cannot lend it to a variant.
	// Report them instead of writing a zero.
	unfixable, err := col.CountDocuments(ctx, bson.M{
		"combinations": needsPrice,
		"price":        bson.M{"$not": bson.M{"$gt": 0}},
	})
	if err != nil {
		fmt.Println("count:", err)
		os.Exit(1)
	}

	cur, err := col.Find(ctx, bson.M{"combinations": needsPrice, "price": bson.M{"$gt": 0}})
	if err != nil {
		fmt.Println("find:", err)
		os.Exit(1)
	}
	var rows []row
	if err := cur.All(ctx, &rows); err != nil {
		fmt.Println("read:", err)
		os.Exit(1)
	}
	fmt.Printf("found %d products with unpriced variants (apply=%v)\n", len(rows), *apply)
	if unfixable > 0 {
		fmt.Printf("WARNING: %d more have variants without a price AND no product price; fix those by hand\n", unfixable)
	}

	var fixed, failed int
	for _, r := range rows {
		missing := 0
		for _, c := range r.Combinations {
			if !(c.Price > 0) {
				missing++
			}
		}
		label := fmt.Sprintf("%s (%s, seller %s)", r.Name, r.ID, r.SellerID)
		if !*apply {
			fmt.Printf("  would set %d variant price(s) to %.2f on %s\n", missing, r.Price, label)
			continue
		}
		// Match on updatedAt so a seller edit made meanwhile is not overwritten.
		res, err := col.UpdateOne(ctx,
			bson.M{"_id": r.ID, "updatedAt": r.UpdatedAt},
			mongo.Pipeline{{{Key: "$set", Value: bson.M{
				"combinations": bson.M{"$map": bson.M{
					"input": "$combinations",
					"as":    "c",
					"in": bson.M{"$cond": bson.A{
						bson.M{"$gt": bson.A{bson.M{"$ifNull": bson.A{"$$c.price", 0}}, 0}},
						"$$c",
						bson.M{"$mergeObjects": bson.A{"$$c", bson.M{"price": "$price"}}},
					}},
				}},
			}}}})
		if err != nil || res.MatchedCount == 0 {
			fmt.Printf("  FAILED or changed meanwhile, re-run: %s: %v\n", label, err)
			failed++
			continue
		}
		fixed++
		fmt.Printf("  set %d variant price(s) to %.2f on %s\n", missing, r.Price, label)
	}
	fmt.Printf("done: fixed=%d failed=%d\n", fixed, failed)
	if !*apply && len(rows) > 0 {
		fmt.Println("dry run only. Re-run with -apply to make the changes.")
	}
	if failed > 0 {
		os.Exit(1)
	}
}
