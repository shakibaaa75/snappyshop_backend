package models

import "time"

// ProductOptionValue is a single selectable value within an option,
// e.g. "Sky Blue" inside the "Color" option.
type ProductOptionValue struct {
	ID       string `bson:"id"`
	Label    string `bson:"label"`
	ColorHex string `bson:"colorHex,omitempty"`
}

// ProductOption is a variant axis the seller can define, e.g. "Color"
// with values ["Sky Blue", "Black"], or "Size" with ["S", "M", "L"].
type ProductOption struct {
	ID     string               `bson:"id"`
	Name   string               `bson:"name"`
	Values []ProductOptionValue `bson:"values"`
}

// ProductCombination is one purchasable combination — the cartesian
// product of one value from each option. Stock, price, and images can
// all be overridden per combination.
type ProductCombination struct {
	ID        string            `bson:"id"`
	ValueIDs  map[string]string `bson:"valueIds"` // optionID -> valueID
	Stock     int               `bson:"stock"`
	ImageURLs []string          `bson:"imageUrls"` // empty = use shared gallery
	Price     float64           `bson:"price,omitempty"`
	SKU       string            `bson:"sku,omitempty"`
}

// ProductAttribute is a free-form spec (e.g. "Material" -> "100% cotton").
type ProductAttribute struct {
	Key   string `bson:"key"`
	Value string `bson:"value"`
}

// Product is stored in the "products" collection. One document per product.
type Product struct {
	ReviewAverage float64 `bson:"reviewAverage,omitempty"`
	ReviewCount   int     `bson:"reviewCount,omitempty"`
	ID            string  `bson:"_id"`
	SellerID      string  `bson:"sellerId"`
	Name          string  `bson:"name"`
	Slug          string  `bson:"slug"`
	Description   string  `bson:"description"`

	// Seller's own storefront categories. CategoryID/CategoryName hold the
	// primary (first) one; CategoryIDs/CategoryNames hold all of them.
	// Products saved before multi-category only have the primary.
	CategoryID    string   `bson:"categoryId"`
	CategoryName  string   `bson:"categoryName"`
	CategoryIDs   []string `bson:"categoryIds"`
	CategoryNames []string `bson:"categoryNames"`

	Price        float64 `bson:"price"`
	ComparePrice float64 `bson:"comparePrice"`
	Stock        int     `bson:"stock"` // total; sum of combination stocks when variants exist
	SKU          string  `bson:"sku,omitempty"`

	// Platform-wide category (global taxonomy). Set by the server from the
	// product type the seller picked or typed. Filter on these, never on text.
	MainID         string `bson:"mainId"`                   // e.g. "fashion"; "other" if unmapped
	SubID          string `bson:"subId"`                    // category ID, e.g. "fashion.hoodie"
	SubName        string `bson:"subName"`                  // official category name at save time (admin renames keep this in sync)
	RawSubCategory string `bson:"rawSubCategory,omitempty"` // what the seller typed, kept for audit
	CategoryStatus string `bson:"categoryStatus,omitempty"` // "mapped" | "unmapped"

	// Gallery
	MainImageURL string   `bson:"mainImageUrl,omitempty"`
	ImageURLs    []string `bson:"imageUrls"`

	// Fallback art (used when no images)
	Emoji string `bson:"emoji"`
	Hue   int    `bson:"hue"`

	// Variants
	Options          []ProductOption      `bson:"options"`
	Combinations     []ProductCombination `bson:"combinations"`
	VariantImageMode string               `bson:"variantImageMode"` // "shared" | "per-variant"
	Attributes       []ProductAttribute   `bson:"attributes"`

	// Promotion / lifecycle
	IsNewArrival bool     `bson:"isNewArrival"`
	IsFeatured   bool     `bson:"isFeatured"`
	IsBestSeller bool     `bson:"isBestSeller"`
	Tags         []string `bson:"tags"`
	IsActive     bool     `bson:"isActive"`

	CreatedAt time.Time `bson:"createdAt"`
	UpdatedAt time.Time `bson:"updatedAt"`
}
