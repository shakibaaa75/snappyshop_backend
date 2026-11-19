package models

import "time"

// Languages a buyer can pick for the storefront UI.
const (
	LangEnglish = "en"
	LangBangla  = "bn"
)

// Home page themes a buyer can pick.
const (
	ThemeLight = "light"
	ThemeDark  = "dark"
)

// BuyerAddress is one saved delivery address, in the Bangladesh format.
type BuyerAddress struct {
	ID         string    `bson:"id"`
	Label      string    `bson:"label"` // Home | Office | Other
	FullName   string    `bson:"fullName"`
	Phone      string    `bson:"phone"` // normalized 01XXXXXXXXX
	Division   string    `bson:"division"`
	District   string    `bson:"district"`
	Upazila    string    `bson:"upazila"` // upazila / thana
	Area       string    `bson:"area"`    // house, road, village, landmark
	PostalCode string    `bson:"postalCode,omitempty"`
	IsDefault  bool      `bson:"isDefault"`
	CreatedAt  time.Time `bson:"createdAt"`
}

// BuyerProfile is stored in "buyer_profiles"; _id is the user ID.
// Name and email stay on the user record.
type BuyerProfile struct {
	UserID    string         `bson:"_id"`
	Phone     string         `bson:"phone,omitempty"`
	AvatarURL string         `bson:"avatarUrl,omitempty"`
	Bio       string         `bson:"bio,omitempty"`
	Language  string         `bson:"language,omitempty"`
	HomeTheme string         `bson:"homeTheme,omitempty"`
	Addresses []BuyerAddress `bson:"addresses"`
	CreatedAt time.Time      `bson:"createdAt"`
	UpdatedAt time.Time      `bson:"updatedAt"`
}
