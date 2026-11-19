package models

import "time"

// Try the Look limits.
const (
	MaxLookItems       = 30 // individual products in one look
	MaxLooksPerSeller  = 50 // sanity cap on how many looks one seller can keep
	MaxLookTitleLength = 80
)

// Look statuses.
const (
	LookDraft     = "draft"     // only the seller sees it
	LookPublished = "published" // buyers can open it (while the seller has access)
)

// Model genders. Only used to organise looks; "" means not set.
const (
	LookGenderMale   = "male"
	LookGenderFemale = "female"
)

// LookItem points at one of the seller's real products. It stores only the
// product ID: name, price, image and stock are read live from the product, so
// they can never go out of date and no product is ever duplicated.
type LookItem struct {
	ProductID string `bson:"productId"`
}

// Look is stored in the "looks" collection: a showcase image of a model plus
// the products the model is wearing. The model image is a showcase only, it is
// not a product and never appears in listings or search.
type Look struct {
	ID            string     `bson:"_id"`
	SellerID      string     `bson:"sellerId"`
	Title         string     `bson:"title"`
	Gender        string     `bson:"gender,omitempty"`
	ModelImageURL string     `bson:"modelImageUrl"`
	Items         []LookItem `bson:"items"` // in display order, at most MaxLookItems
	Status        string     `bson:"status"`
	PublishedAt   *time.Time `bson:"publishedAt,omitempty"`
	CreatedAt     time.Time  `bson:"createdAt"`
	UpdatedAt     time.Time  `bson:"updatedAt"`
}

// HasProduct reports whether the look already contains the product.
func (l *Look) HasProduct(productID string) bool {
	for _, it := range l.Items {
		if it.ProductID == productID {
			return true
		}
	}
	return false
}
