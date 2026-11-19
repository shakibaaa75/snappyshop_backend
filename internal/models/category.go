package models

import "time"

// Category is created by a seller, scoped to their store. Two sellers can
// have a "Fashion" category without clashing.
type Category struct {
	ID        string    `bson:"_id"`
	SellerID  string    `bson:"sellerId"`
	Name      string    `bson:"name"`
	Slug      string    `bson:"slug"`
	Emoji     string    `bson:"emoji"`
	ImageURL  string    `bson:"imageUrl"`
	Position  int       `bson:"position"`
	CreatedAt time.Time `bson:"createdAt"`
	UpdatedAt time.Time `bson:"updatedAt"`
}
