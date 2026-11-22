package handler

import (
	"net/http"
	"regexp"
	"unicode/utf8"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/database"
	"shopbd/backend/internal/httpx"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type reviewDisplay struct {
	Enabled  bool   `json:"reviewSection" bson:"reviewSection"`
	Heading  string `json:"reviewHeading" bson:"reviewHeading"`
	Layout   string `json:"reviewLayout" bson:"reviewLayout"`
	Color    string `json:"reviewColor" bson:"reviewColor"`
	Position string `json:"reviewPosition" bson:"reviewPosition"`
	Padding  int    `json:"reviewPadding" bson:"reviewPadding"`
}

var reviewHexColor = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

func (h Reviews) HomeSettings(w http.ResponseWriter, r *http.Request) {
	// Prefer the live design when migrating a shop from the design editor.
	var doc struct {
		Draft         bson.M
		Published     bson.M
		ReviewDisplay bson.M `bson:"reviewDisplay"`
	}
	projection := bson.M{"reviewDisplay": 1}
	for _, key := range []string{"reviewSection", "reviewHeading", "reviewLayout", "reviewColor", "reviewPadding", "reviewIds", "sections", "reviewPosition"} {
		if key == "sections" {
			for _, field := range []string{"id", "title", "enabled"} {
				projection["draft.sections."+field] = 1
				projection["published.sections."+field] = 1
			}
			continue
		}
		projection["draft."+key] = 1
		projection["published."+key] = 1
	}
	err := h.DB.Collection(database.StoreDesignsCollection).FindOne(r.Context(), bson.M{"sellerId": actor(r)}, options.FindOne().SetProjection(projection).SetSort(bson.D{{Key: "updatedAt", Value: -1}})).Decode(&doc)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	display := doc.Published
	if display == nil {
		display = doc.Draft
	}
	if display == nil {
		display = bson.M{}
	}
	for key, value := range doc.ReviewDisplay {
		display[key] = value
	}
	httpx.WriteJSON(w, 200, display)
}

func (h Reviews) SaveHomeSettings(w http.ResponseWriter, r *http.Request) {
	var display reviewDisplay
	if err := httpx.DecodeJSON(w, r, &display, 2048); err != nil {
		h.fail(w, r, err)
		return
	}
	switch display.Layout {
	case "grid", "carousel", "testimonial":
	default:
		h.fail(w, r, apperr.BadRequest("Choose a review layout."))
		return
	}
	switch display.Position {
	case "":
		display.Position = "bottom"
	case "top", "bottom", "hero", "trust", "categories", "featured", "bestSellers", "spotlight", "promo", "newArrivals", "allProducts", "story", "countdown", "gallery":
	default:
		h.fail(w, r, apperr.BadRequest("Choose a homepage position."))
		return
	}
	if utf8.RuneCountInString(display.Heading) > 80 || display.Padding < 0 || display.Padding > 64 || (display.Color != "" && !reviewHexColor.MatchString(display.Color)) {
		h.fail(w, r, apperr.BadRequest("Check the heading, background and spacing."))
		return
	}
	// Keep this override outside the draft so publishing an open editor cannot undo it.
	res := h.DB.Collection(database.StoreDesignsCollection).FindOneAndUpdate(r.Context(), bson.M{"sellerId": actor(r)}, bson.M{"$set": bson.M{"reviewDisplay": display}}, options.FindOneAndUpdate().SetSort(bson.D{{Key: "updatedAt", Value: -1}}).SetProjection(bson.M{"_id": 1}))
	if err := res.Err(); err != nil {
		h.fail(w, r, err)
		return
	}
	invalidateSlugs(h.Cache)
	httpx.WriteJSON(w, 200, display)
}
