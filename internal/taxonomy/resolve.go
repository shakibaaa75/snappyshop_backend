package taxonomy

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"strings"
	"unicode"
)

// Errors a seller's category input can produce. The product service turns
// them into form messages.
var (
	ErrMainRequired      = errors.New("taxonomy: main type is required")
	ErrUnknownMain       = errors.New("taxonomy: unknown main type")
	ErrCategoryRequired  = errors.New("taxonomy: category is required")
	ErrCategoryNotFound  = errors.New("taxonomy: picked category no longer exists")
	ErrCategoryNotInMain = errors.New("taxonomy: category does not belong to that main type")
	ErrInvalidName       = errors.New("taxonomy: invalid category name")
	ErrCreateConflict    = errors.New("taxonomy: could not create category, please retry")
)

// Name length limits for a seller-created category (in characters).
const (
	MinNameLen = 2
	MaxNameLen = 40
)

// Input is what the seller form sends.
type Input struct {
	MainID         string // one of the 10 fixed Main Types (required)
	CategoryID     string // category picked from the list
	CustomCategory string // category typed by the seller
}

// Actor says who triggered a lookup, so a newly created category can record
// who made it and where. Admins read this in the Category Management screen.
type Actor struct {
	SellerID  string
	StoreName string
	// StoreNameFn is called only when a category is actually created, so
	// ordinary product saves never pay for the lookup.
	StoreNameFn func(ctx context.Context) string
	ProductID   string
	Via         string // e.g. "seller_product_form"
}

// Result is what to store on the product.
type Result struct {
	MainID       string
	CategoryID   string // always the ID of an existing (or just-created) category
	CategoryName string // the OFFICIAL name: save this, never the seller's spelling
	Raw          string // what the seller typed (kept for audit)
	Method       MatchMethod
	// Band is how sure the match was. NeedsReview (BandReview) means the
	// category was applied but an admin may want to check the mapping.
	Band        MatchBand
	NeedsReview bool
	Corrected   bool  // true when Raw differs from the official name
	Created     bool  // true when this call created a brand-new category
	Node        *Node // the new category when Created
}

// PlanResult is the pure, database-free outcome of Plan.
type PlanResult struct {
	Result  Result
	Create  bool   // no close match: a new category must be created
	NewName string // its tidied name
}

// Plan decides what to do with the seller's input. Pure function: it never
// writes. It compares the text only against categories under the SELECTED Main
// Type, so a typo can never move a product into another Main Type.
func Plan(ix *Index, in Input) (PlanResult, error) {
	mainID := strings.TrimSpace(in.MainID)
	catID := strings.TrimSpace(in.CategoryID)
	raw := strings.TrimSpace(in.CustomCategory)

	// Older clients send only the category ID: take the Main Type from it.
	if mainID == "" && catID != "" {
		if n, ok := ix.Node(catID); ok && IsSub(n) {
			mainID = n.ParentID
		}
	}
	if mainID == "" {
		return PlanResult{}, ErrMainRequired
	}
	main, ok := ix.Node(mainID)
	if !ok || !IsMain(main) {
		return PlanResult{}, ErrUnknownMain
	}

	// 1) Seller picked an existing category.
	if catID != "" {
		if n, ok := ix.Node(catID); ok && IsSub(n) {
			if n.ParentID != main.ID {
				return PlanResult{}, ErrCategoryNotInMain
			}
			return PlanResult{Result: Result{
				MainID: main.ID, CategoryID: n.ID, CategoryName: n.Name, Method: MethodSelected,
			}}, nil
		}
		if raw == "" {
			return PlanResult{}, ErrCategoryNotFound // e.g. an admin merged it meanwhile
		}
	}

	// 2) Seller typed a category.
	if raw == "" {
		return PlanResult{}, ErrCategoryRequired
	}
	name := TidyName(raw)
	if !validName(name) {
		return PlanResult{}, ErrInvalidName
	}
	if n, m, ok := closestInMain(ix, name, main.ID); ok {
		return PlanResult{Result: Result{
			MainID: main.ID, CategoryID: n.ID, CategoryName: n.Name, Raw: raw,
			Method: m.Method, Band: m.Band, NeedsReview: m.Band == BandReview,
			Corrected: !strings.EqualFold(strings.TrimSpace(raw), n.Name),
		}}, nil
	}

	// 3) Nothing close enough: a new category is needed.
	return PlanResult{
		Create: true, NewName: name,
		Result: Result{MainID: main.ID, CategoryName: name, Raw: raw, Method: MethodNone},
	}, nil
}

// closestInMain returns the existing category under mainID that the text
// clearly means: exact name, alias, admin-learned spelling, a typo, or the
// same words in another order / with filler words around them.
func closestInMain(ix *Index, name, mainID string) (Node, Match, bool) {
	inMain := func(n Node) bool { return n.ParentID == mainID }
	m := ix.ResolveText(name, inMain)
	if !m.Found() {
		return Node{}, Match{}, false
	}
	if m.Ambiguous {
		// Two categories were equally close. Take the best-ranked one
		// deterministically rather than creating a near-duplicate.
		if s := ix.Suggest(name, inMain, 1); len(s) > 0 {
			return s[0].Node, Match{NodeID: s[0].Node.ID, Method: MethodFuzzy, Score: s[0].Score, Band: BandReview}, true
		}
		return Node{}, Match{}, false
	}
	// "Watch Strap" contains "Watch" but also a meaningful word the category
	// does not have: the seller probably means a different, new category.
	if m.Extra {
		return Node{}, Match{}, false
	}
	n, ok := ix.Node(m.NodeID)
	if !ok {
		return Node{}, Match{}, false
	}
	return n, m, true
}

func meaningfulWords(s string) int {
	n := 0
	for _, w := range strings.Fields(Normalize(s)) {
		if !stopWords[w] {
			n++
		}
	}
	return n
}

func validName(s string) bool {
	n := len([]rune(s))
	if n < MinNameLen || n > MaxNameLen {
		return false
	}
	for _, r := range s {
		if unicode.IsLetter(r) {
			return true
		}
	}
	return false // digits/symbols only
}

// TidyName cleans a typed name without changing what the seller meant:
// collapses spaces, and Title-Cases it only if it was ALL lower or ALL upper.
// "mens t-shirt" -> "Mens T-Shirt"; "iPhone Cases" is left alone.
func TidyName(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	hasLower, hasUpper := false, false
	for _, r := range s {
		hasLower = hasLower || unicode.IsLower(r)
		hasUpper = hasUpper || unicode.IsUpper(r)
	}
	if hasLower && hasUpper {
		return s
	}
	var b strings.Builder
	start := true
	for _, r := range s {
		if start {
			b.WriteRune(unicode.ToUpper(r))
		} else {
			b.WriteRune(unicode.ToLower(r))
		}
		start = r == ' ' || r == '-' || r == '/'
	}
	return b.String()
}

// newNodeID builds a stable ID like "fashion.pet-wear". Names with no Latin
// letters (e.g. Bangla) get a short hash. attempt > 0 adds a suffix after a
// clash.
func newNodeID(mainID, name string, attempt int) string {
	s := slug(name)
	if s == "" {
		h := fnv.New32a()
		h.Write([]byte(KeyOf(Normalize(name))))
		s = fmt.Sprintf("c%08x", h.Sum32())
	}
	id := mainID + "." + s
	if attempt > 0 {
		id += fmt.Sprintf("-%d", attempt+1)
	}
	return id
}

/* ------------------------------ buyer side ------------------------------ */

// Filter is what the product query should use. IDs only, never text.
type Filter struct {
	MainID string
	SubID  string
}

// BuildFilter validates the IDs from the URL. A sub filter wins over a main
// filter; a main filter includes all of its subcategories.
func (ix *Index) BuildFilter(main, sub string) Filter {
	if sub != "" {
		if n, ok := ix.Node(sub); ok && IsSub(n) {
			return Filter{SubID: n.ID}
		}
	}
	if main != "" {
		if n, ok := ix.Node(main); ok && IsMain(n) {
			return Filter{MainID: n.ID}
		}
	}
	return Filter{}
}

type Brief struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Image string `json:"image,omitempty"`
}

type TreeNode struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Image    string  `json:"image,omitempty"`
	Children []Brief `json:"children"`
}

// Home returns main categories only, for the homepage.
func (ix *Index) Home() []Brief {
	out := []Brief{}
	for _, n := range ix.Mains() {
		if n.ShowOnHome {
			out = append(out, Brief{ID: n.ID, Name: n.Name, Image: n.ImageURL})
		}
	}
	return out
}

// Subcategories returns the children of one main category.
func (ix *Index) Subcategories(mainID string) []Brief {
	out := []Brief{}
	for _, n := range ix.Children(mainID) {
		out = append(out, Brief{ID: n.ID, Name: n.Name, Image: n.ImageURL})
	}
	return out
}

// Tree returns every visible main category with its children (seller form).
func (ix *Index) Tree() []TreeNode {
	out := []TreeNode{}
	for _, m := range ix.Mains() {
		if !m.ShowOnHome {
			continue
		}
		out = append(out, TreeNode{ID: m.ID, Name: m.Name, Image: m.ImageURL, Children: ix.Subcategories(m.ID)})
	}
	return out
}
