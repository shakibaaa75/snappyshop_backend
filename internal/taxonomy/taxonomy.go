// Package taxonomy is the platform-wide category system: a fixed two-level
// tree (main category -> subcategory) with stable IDs, plus typo-tolerant
// matching of seller-typed text onto that tree.
package taxonomy

import (
	"strings"
	"time"
)

// OtherID is the legacy fallback main category.
//
// Deprecated: the 10 Main Types are fixed and sellers must pick one, so there
// is no "Other" any more. Kept only so the old Resolve keeps compiling until
// Part 2 replaces it.
const OtherID = "other"

// Where a node came from.
const (
	SourceSeed   = "seed"   // built-in Main Type or starter category
	SourceSeller = "seller" // auto-created from a seller's product form
	SourceAdmin  = "admin"  // created or moved by an admin
)

// Node is one category. ParentID == "" means a main category.
type Node struct {
	ID         string   `json:"id" bson:"_id"` // stable, e.g. "fashion", "fashion.hoodie"
	ParentID   string   `json:"parentId" bson:"parentId"`
	Name       string   `json:"name" bson:"name"`
	Aliases    []string `json:"-" bson:"aliases"` // other spellings / languages / synonyms
	SortOrder  int      `json:"sortOrder" bson:"sortOrder"`
	IsActive   bool     `json:"-" bson:"isActive"`
	ShowOnHome bool     `json:"-" bson:"showOnHome"`
	// ImageURL is the picture the admin chose for this category (main or sub).
	// Shown on the home page "All categories" view. Empty = no picture yet.
	ImageURL string `json:"-" bson:"imageUrl,omitempty"`

	// Provenance. Never sent to the public API (json:"-"); the admin API
	// builds its own response from these.
	Source             string    `json:"-" bson:"source"`                       // SourceSeed | SourceSeller | SourceAdmin
	CreatedAt          time.Time `json:"-" bson:"createdAt"`                    // when it was created
	CreatedBySellerID  string    `json:"-" bson:"createdBySellerId,omitempty"`  // who created it
	CreatedByStoreName string    `json:"-" bson:"createdByStoreName,omitempty"` // ...and their store
	CreatedVia         string    `json:"-" bson:"createdVia,omitempty"`         // where, e.g. "seller_product_form"
	CreatedFromProduct string    `json:"-" bson:"createdFromProduct,omitempty"` // product that triggered it
	RawInput           string    `json:"-" bson:"rawInput,omitempty"`           // exactly what the seller typed
	AdminSeen          bool      `json:"-" bson:"adminSeen"`                    // false = shows in the admin "new" badge
}

type seedMain struct {
	Name    string
	Aliases string
	Subs    []string
}

func slug(s string) string {
	s = strings.ToLower(strings.ReplaceAll(s, "&", "and"))
	var b strings.Builder
	dash := false
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.TrimRight(b.String(), "-")
}

// withParts adds both halves of "A & B" names as aliases and drops blanks/dupes.
func withParts(name string, aliases []string) []string {
	all := append([]string{}, aliases...)
	if strings.Contains(name, "&") {
		for _, p := range strings.Split(name, "&") {
			all = append(all, strings.TrimSpace(p))
		}
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(all))
	for _, a := range all {
		a = strings.TrimSpace(a)
		if a == "" || seen[a] {
			continue
		}
		seen[a] = true
		out = append(out, a)
	}
	return out
}

// Seed returns the initial taxonomy. Until you build an admin screen you can
// use it directly: taxonomy.NewIndex(taxonomy.Seed(), nil).
func Seed() []Node {
	out := make([]Node, 0, 160)
	for mi, m := range seed {
		id := slug(m.Name)
		out = append(out, Node{
			ID: id, Name: m.Name, Aliases: withParts(m.Name, strings.Split(m.Aliases, "|")),
			SortOrder: mi, IsActive: true, ShowOnHome: true,
			Source: SourceSeed, AdminSeen: true,
		})
		for si, s := range m.Subs {
			parts := strings.Split(s, "|")
			out = append(out, Node{
				ID: id + "." + slug(parts[0]), ParentID: id, Name: parts[0],
				Aliases:   withParts(parts[0], parts[1:]),
				SortOrder: si, IsActive: true, ShowOnHome: true,
				Source: SourceSeed, AdminSeen: true,
			})
		}
	}
	return out
}
