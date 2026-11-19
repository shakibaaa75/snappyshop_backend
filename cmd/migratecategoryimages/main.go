// migratecategoryimages fixes seller category rows that still hold a base64
// "data:image/..." picture inside MongoDB (saved before pictures went to R2).
//
//	-mode move   (default) decode each picture, run it through the same
//	             validation/WebP pipeline as normal uploads, store it in R2
//	             under catagory_image/<sellerId>/..., and save the https URL.
//	-mode clear  just empty the imageUrl (the seller can add a new picture).
//
// It is a dry run unless -apply is given. Safe to run more than once: rows
// that no longer start with "data:" are never touched.
//
//	set -a; source .env; set +a
//	go run ./cmd/migratecategoryimages                 # dry run
//	go run ./cmd/migratecategoryimages -apply          # move to R2
//	go run ./cmd/migratecategoryimages -apply -mode clear
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"

	"shopbd/backend/internal/database"
	"shopbd/backend/internal/imagesafe"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/storage"
	"shopbd/backend/internal/uploads"
)

func main() {
	apply := flag.Bool("apply", false, "write changes (default is a dry run)")
	mode := flag.String("mode", "move", "move | clear")
	flag.Parse()
	if *mode != "move" && *mode != "clear" {
		fmt.Println("mode must be move or clear")
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	client, err := database.Connect(ctx, os.Getenv("MONGODB_URI"))
	if err != nil {
		fmt.Println("mongo:", err)
		os.Exit(1)
	}
	defer client.Disconnect(context.Background())
	db := client.Database(os.Getenv("MONGODB_DB"))
	col := db.Collection(database.CategoriesCollection)
	upStore := uploads.NewStore(db)

	var r2 *storage.R2
	if *mode == "move" {
		r2, err = storage.New(storage.Config{
			AccountID:       os.Getenv("R2_ACCOUNT_ID"),
			Bucket:          os.Getenv("R2_BUCKET"),
			AccessKeyID:     os.Getenv("R2_ACCESS_KEY_ID"),
			SecretAccessKey: os.Getenv("R2_SECRET_ACCESS_KEY"),
			PublicBaseURL:   os.Getenv("R2_PUBLIC_URL"),
		})
		if err != nil {
			fmt.Println("r2 config:", err)
			os.Exit(1)
		}
	}

	cur, err := col.Find(ctx, bson.M{"imageUrl": bson.M{"$regex": "^data:"}})
	if err != nil {
		fmt.Println("find:", err)
		os.Exit(1)
	}
	var rows []struct {
		ID       string `bson:"_id"`
		SellerID string `bson:"sellerId"`
		Name     string `bson:"name"`
		ImageURL string `bson:"imageUrl"`
	}
	if err := cur.All(ctx, &rows); err != nil {
		fmt.Println("read:", err)
		os.Exit(1)
	}
	fmt.Printf("found %d categories with a base64 picture (mode=%s, apply=%v)\n", len(rows), *mode, *apply)

	var moved, cleared, failed int
	for _, row := range rows {
		label := fmt.Sprintf("%s (%s)", row.Name, row.ID)
		if !*apply {
			fmt.Printf("  would %s: %s, %d bytes\n", *mode, label, len(row.ImageURL))
			continue
		}
		newURL := ""
		if *mode == "move" {
			u, err := moveOne(ctx, r2, upStore, row.SellerID, row.ImageURL)
			if err != nil {
				// Leave the row untouched so it can be retried or cleared later.
				fmt.Printf("  FAILED %s: %v\n", label, err)
				failed++
				continue
			}
			newURL = u
		}
		// Match on the old value so a seller edit made meanwhile is not overwritten.
		res, err := col.UpdateOne(ctx,
			bson.M{"_id": row.ID, "imageUrl": row.ImageURL},
			bson.M{"$set": bson.M{"imageUrl": newURL, "updatedAt": time.Now().UTC()}})
		if err != nil || res.MatchedCount == 0 {
			fmt.Printf("  FAILED to save %s: %v\n", label, err)
			failed++
			continue
		}
		if *mode == "move" {
			moved++
			fmt.Printf("  moved %s -> %s\n", label, newURL)
		} else {
			cleared++
			fmt.Printf("  cleared %s\n", label)
		}
	}
	fmt.Printf("done: moved=%d cleared=%d failed=%d\n", moved, cleared, failed)
	if !*apply && len(rows) > 0 {
		fmt.Println("dry run only. Re-run with -apply to make the changes.")
	}
	if failed > 0 {
		os.Exit(1)
	}
}

// moveOne decodes one data: URL, re-encodes it with the same checks as a
// normal upload, stores it in R2 and records it in the uploads collection.
func moveOne(ctx context.Context, r2 *storage.R2, st *uploads.Store, sellerID, dataURL string) (string, error) {
	comma := strings.IndexByte(dataURL, ',')
	if comma < 0 || !strings.Contains(dataURL[:comma], ";base64") {
		return "", fmt.Errorf("not a base64 data URL")
	}
	raw, err := base64.StdEncoding.DecodeString(dataURL[comma+1:])
	if err != nil {
		return "", fmt.Errorf("bad base64: %w", err)
	}
	res, err := imagesafe.Process(ctx, raw)
	if err != nil {
		return "", fmt.Errorf("image rejected: %w", err)
	}
	kind := storage.Kinds["category"]
	key, err := storage.BuildKey(kind, sellerID, "webp")
	if err != nil {
		return "", err
	}
	if err := r2.PutObject(ctx, key, "image/webp", res.WebP); err != nil {
		return "", fmt.Errorf("r2 put: %w", err)
	}
	var id [16]byte
	_, _ = rand.Read(id[:])
	sum := sha256.Sum256(res.WebP)
	now := time.Now().UTC()
	rec := &uploads.Upload{
		ID: hex.EncodeToString(id[:]), OwnerID: sellerID, Role: string(models.RoleSeller), Kind: "category",
		Key: key, Status: uploads.StatusApproved, Bytes: int64(len(res.WebP)),
		Width: res.Width, Height: res.Height, SHA256: hex.EncodeToString(sum[:]),
		CreatedAt: now, UpdatedAt: now,
	}
	if err := st.Insert(ctx, rec); err != nil {
		_ = r2.DeleteObject(ctx, key) // never leave an unrecorded object behind
		return "", fmt.Errorf("record upload: %w", err)
	}
	return r2.PublicURL(key), nil
}
