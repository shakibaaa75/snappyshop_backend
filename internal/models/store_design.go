package models

import "time"

// StoreDesign is stored in the "store_designs" collection, one document per
// seller. Draft holds whatever the editor last saved. Published is the last
// snapshot the seller chose to make public.
//
// Slug is the current public store address. PreviousSlugs holds every slug
// this seller has ever used; those addresses stay permanently invalid (they
// never resolve to a store and cannot be claimed by anyone else).
// SlugChangedAt is set whenever the slug actually changes from a previous
// non-empty value; sellers must wait 30 days between changes.
type StoreDesign struct {
	ID            string     `bson:"_id"`
	SellerID      string     `bson:"sellerId"`
	Slug          string     `bson:"slug"`
	PreviousSlugs []string   `bson:"previousSlugs,omitempty"`
	SlugChangedAt *time.Time `bson:"slugChangedAt,omitempty"`

	// StoreName is the current public shop name. NameChangeCount is how many
	// times the seller has renamed the shop since registration (the shop name
	// chosen at registration is free; only ONE later rename is allowed).
	// Previous shop names are deliberately NOT stored anywhere.
	StoreName       string `bson:"storeName,omitempty"`
	NameChangeCount int    `bson:"nameChangeCount,omitempty"`

	Draft       any       `bson:"draft"`
	Published   any       `bson:"published,omitempty"`
	IsPublished bool      `bson:"isPublished"`
	CreatedAt   time.Time `bson:"createdAt"`
	UpdatedAt   time.Time `bson:"updatedAt"`
}
