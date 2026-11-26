package storage

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
)

// Kind describes one kind of upload: which folder in the bucket it goes to,
// which file types are allowed and how big the file may be.
type Kind struct {
	Folder   string            // top-level folder in the bucket (matches the R2 dashboard)
	Types    map[string]string // allowed Content-Type -> file extension
	MaxBytes int64
}

var imageTypes = map[string]string{
	"image/jpeg": "jpg",
	"image/png":  "png",
	"image/webp": "webp",
}

var videoTypes = map[string]string{
	"video/mp4":  "mp4",
	"video/webm": "webm",
}

const (
	mb = 1 << 20
)

// Kinds are the names the frontend sends as "kind".
// Folder names are spelled exactly as the bucket folders are.
var Kinds = map[string]Kind{
	"product":        {Folder: "products_images", Types: imageTypes, MaxBytes: 5 * mb},
	"category":       {Folder: "catagory_image", Types: imageTypes, MaxBytes: 3 * mb},
	"seller_cover":   {Folder: "seller_cover", Types: imageTypes, MaxBytes: 5 * mb},
	"seller_profile": {Folder: "seller_profile", Types: imageTypes, MaxBytes: 2 * mb},
	// The owner's personal profile picture (Settings > Personal information).
	// Its own folder: "seller_profile" is the storefront image.
	"avatar":       {Folder: "seller_avatar", Types: imageTypes, MaxBytes: 2 * mb},
	"homepage":     {Folder: "server_homepage", Types: imageTypes, MaxBytes: 5 * mb},
	"look_model":   {Folder: "try_the_look", Types: imageTypes, MaxBytes: 5 * mb},
	"banner_video": {Folder: "banner_video", Types: videoTypes, MaxBytes: 50 * mb},
}

// ReviewPhotos is where buyers' review photos are stored. It is not in Kinds on
// purpose: review photos are uploaded through the review form (which shrinks
// them to JPEG thumbnails), not through the seller/admin upload endpoint.
var ReviewPhotos = Kind{Folder: "review_photos", Types: map[string]string{"image/jpeg": "jpg"}, MaxBytes: 1 * mb}

// IsImage reports whether the kind holds images. Image kinds are uploaded
// through the validating endpoint and always stored as WebP.
func (k Kind) IsImage() bool { _, ok := k.Types["image/webp"]; return ok }

// BuildKey makes an unguessable object key: <folder>/<ownerID>/<random>.<ext>.
// The owner folder keeps one seller's files together and never reuses a name,
// so an upload can never overwrite someone else's file.
func BuildKey(k Kind, ownerID, ext string) (string, error) {
	owner := safeSegment(ownerID)
	if owner == "" {
		return "", errors.New("storage: owner id is empty")
	}
	var rnd [16]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return "", err
	}
	return k.Folder + "/" + owner + "/" + hex.EncodeToString(rnd[:]) + "." + ext, nil
}

func safeSegment(s string) string {
	var b strings.Builder
	for _, c := range s {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' {
			b.WriteRune(c)
		}
	}
	return b.String()
}

// OwnerSegment is the folder name BuildKey uses for an owner ID.
func OwnerSegment(ownerID string) string { return safeSegment(ownerID) }
