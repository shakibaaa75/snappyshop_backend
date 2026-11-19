package models

import "time"

// TrackingSettings is stored in the "tracking_settings" collection, one
// document per seller (the _id is the seller ID).
//
// Pixel IDs are not secrets: they are public in the storefront HTML. They are
// validated before saving because they are rendered inside a <script>.
type TrackingSettings struct {
	SellerID      string `bson:"_id"`
	MetaPixelID   string `bson:"metaPixelId,omitempty"`
	TikTokPixelID string `bson:"tiktokPixelId,omitempty"`
	// RequireConsent shows the visitor a consent banner on this store and keeps
	// ad pixels and the device ID off until they accept. Defaults to true.
	RequireConsent bool      `bson:"requireConsent"`
	UpdatedAt      time.Time `bson:"updatedAt"`
}
