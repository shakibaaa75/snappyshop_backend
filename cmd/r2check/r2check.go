// r2check uploads a tiny PNG to R2 with the same credentials and signing code
// the API uses, and prints R2's exact answer. Run it from the backend folder:
//
//	set -a; source .env; set +a; go run ./cmd/r2check
package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"time"

	"shopbd/backend/internal/storage"
)

// A 1x1 transparent PNG.
const tinyPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="

func main() {
	cfg := storage.Config{
		AccountID:       os.Getenv("R2_ACCOUNT_ID"),
		Bucket:          os.Getenv("R2_BUCKET"),
		AccessKeyID:     os.Getenv("R2_ACCESS_KEY_ID"),
		SecretAccessKey: os.Getenv("R2_SECRET_ACCESS_KEY"),
		PublicBaseURL:   os.Getenv("R2_PUBLIC_URL"),
	}
	fmt.Println("account:", cfg.AccountID)
	fmt.Println("bucket :", cfg.Bucket)
	fmt.Println("key id :", mask(cfg.AccessKeyID))
	fmt.Println("public :", cfg.PublicBaseURL)

	r2, err := storage.New(cfg)
	if err != nil {
		fmt.Println("CONFIG ERROR:", err)
		os.Exit(1)
	}

	key, err := storage.BuildKey(storage.Kind{Folder: "r2check"}, "check", "png")
	if err != nil {
		fmt.Println("KEY ERROR:", err)
		os.Exit(1)
	}
	body, _ := base64.StdEncoding.DecodeString(tinyPNG)

	// Same backend-side upload path the API uses.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := r2.PutObject(ctx, key, "image/png", body); err != nil {
		fmt.Println("\nPUT FAILED:", err)
		os.Exit(1)
	}
	fmt.Println("\nPUT ok")
	defer func() { _ = r2.DeleteObject(ctx, key); fmt.Println("deleted test object") }()

	pub := r2.PublicURL(key)
	g, err := http.Get(pub)
	if err != nil {
		fmt.Println("GET ERROR:", err)
		os.Exit(1)
	}
	g.Body.Close()
	fmt.Println("\npublic url:", pub)
	fmt.Println("GET status:", g.Status)

}

func mask(s string) string {
	if len(s) <= 8 {
		return "(too short)"
	}
	return s[:8] + "…"
}
