package taxonomy

import (
	"context"
	"errors"
	"net/url"
	"sort"
	"strings"
	"time"
)

// ProductStore is the product-side of admin changes. Every rename, merge,
// move and delete goes through it so products always follow their category
// and no product data is lost.
type ProductStore interface {
	CountByCategory(ctx context.Context) (map[string]int, error)
	CountForCategory(ctx context.Context, categoryID string) (int64, error)
	// Reassign moves every product of category fromID into category `to`
	// (sets mainId, subId and subName). Returns how many products moved.
	Reassign(ctx context.Context, fromID string, to Node) (int64, error)
	SetName(ctx context.Context, categoryID, name string) error
	SetMain(ctx context.Context, categoryID, mainID string) error
}

var (
	ErrNotFound       = errors.New("taxonomy: category not found")
	ErrMainTypeLocked = errors.New("taxonomy: main types cannot be changed")
	ErrSameCategory   = errors.New("taxonomy: source and target are the same category")
	ErrInvalidImage   = errors.New("taxonomy: image must be an https address")
	ErrDifferentMain  = errors.New("taxonomy: merge needs both categories in the same main type")
)

// NameTakenError: another category in that main type already uses the name.
// The admin should merge into Existing instead.
type NameTakenError struct{ Existing Node }

func (e *NameTakenError) Error() string { return "taxonomy: name already used by " + e.Existing.ID }

// HasProductsError: a category with products can only be deleted if the
// products are moved to another category first.
type HasProductsError struct{ Count int64 }

func (e *HasProductsError) Error() string { return "taxonomy: category still has products" }

// Admin implements Category Management.
type Admin struct {
	Cat      *Catalog
	Products ProductStore
}

/* ------------------------------- views ---------------------------------- */

type Similar struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	MainID   string  `json:"mainId"`
	MainName string  `json:"mainName"`
	Score    float64 `json:"score"`
}

type AdminNode struct {
	ID                 string    `json:"id"`
	ParentID           string    `json:"parentId"`
	Name               string    `json:"name"`
	ImageURL           string    `json:"imageUrl,omitempty"`
	Aliases            []string  `json:"aliases"`
	Source             string    `json:"source"`
	CreatedAt          time.Time `json:"createdAt"`
	CreatedBySellerID  string    `json:"createdBySellerId,omitempty"`
	CreatedByStoreName string    `json:"createdByStoreName,omitempty"`
	CreatedVia         string    `json:"createdVia,omitempty"`
	CreatedFromProduct string    `json:"createdFromProduct,omitempty"`
	RawInput           string    `json:"rawInput,omitempty"`
	AdminSeen          bool      `json:"adminSeen"`
	ProductCount       int       `json:"productCount"`
	Similar            []Similar `json:"similar,omitempty"`
}

type AdminMain struct {
	ID           string      `json:"id"`
	Name         string      `json:"name"`
	ImageURL     string      `json:"imageUrl,omitempty"`
	ProductCount int         `json:"productCount"`
	Categories   []AdminNode `json:"categories"`
}

func toAdminNode(n Node, count int) AdminNode {
	al := n.Aliases
	if al == nil {
		al = []string{}
	}
	return AdminNode{
		ID: n.ID, ParentID: n.ParentID, Name: n.Name, ImageURL: n.ImageURL, Aliases: al, Source: n.Source,
		CreatedAt: n.CreatedAt, CreatedBySellerID: n.CreatedBySellerID,
		CreatedByStoreName: n.CreatedByStoreName, CreatedVia: n.CreatedVia,
		CreatedFromProduct: n.CreatedFromProduct, RawInput: n.RawInput,
		AdminSeen: n.AdminSeen, ProductCount: count,
	}
}

func adminKey(s string) string { return KeyOf(Normalize(s)) }

// List returns every category grouped by Main Type, plus how many new
// (unseen) categories need a look.
func (a *Admin) List(ctx context.Context) ([]AdminMain, int, error) {
	all, err := a.Cat.Store.ListNodes(ctx)
	if err != nil {
		return nil, 0, err
	}
	counts, err := a.Products.CountByCategory(ctx)
	if err != nil {
		return nil, 0, err
	}
	var mains []AdminMain
	idx := map[string]int{}
	for _, n := range all {
		if IsMain(n) {
			idx[n.ID] = len(mains)
			mains = append(mains, AdminMain{ID: n.ID, Name: n.Name, ImageURL: n.ImageURL, Categories: []AdminNode{}})
		}
	}
	sort.SliceStable(mains, func(i, j int) bool {
		mi, _ := findNode(all, mains[i].ID)
		mj, _ := findNode(all, mains[j].ID)
		return mi.SortOrder < mj.SortOrder
	})
	for i := range mains {
		idx[mains[i].ID] = i
	}
	unseen := 0
	subs := make([]Node, 0, len(all))
	for _, n := range all {
		if IsSub(n) {
			subs = append(subs, n)
		}
	}
	sort.SliceStable(subs, func(i, j int) bool {
		if subs[i].SortOrder != subs[j].SortOrder {
			return subs[i].SortOrder < subs[j].SortOrder
		}
		return subs[i].Name < subs[j].Name
	})
	for _, n := range subs {
		i, ok := idx[n.ParentID]
		if !ok {
			continue
		}
		mains[i].Categories = append(mains[i].Categories, toAdminNode(n, counts[n.ID]))
		mains[i].ProductCount += counts[n.ID]
		if !n.AdminSeen {
			unseen++
		}
	}
	return mains, unseen, nil
}

// Unseen lists categories sellers created that no admin has looked at yet,
// newest first, each with similar-name suggestions.
func (a *Admin) Unseen(ctx context.Context) ([]AdminNode, error) {
	all, err := a.Cat.Store.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	counts, err := a.Products.CountByCategory(ctx)
	if err != nil {
		return nil, err
	}
	var nodes []Node
	for _, n := range all {
		if IsSub(n) && !n.AdminSeen {
			nodes = append(nodes, n)
		}
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].CreatedAt.After(nodes[j].CreatedAt) })
	out := make([]AdminNode, 0, len(nodes))
	for _, n := range nodes {
		an := toAdminNode(n, counts[n.ID])
		an.Similar = a.similar(n, 3)
		out = append(out, an)
	}
	return out, nil
}

// Similar returns spelling / similar-name suggestions for one category:
// close names in the same Main Type (merge candidates) and the exact same
// name in other Main Types.
func (a *Admin) Similar(ctx context.Context, id string) ([]Similar, error) {
	n, _, err := a.loadSub(ctx, id)
	if err != nil {
		return nil, err
	}
	return a.similar(n, 5), nil
}

func (a *Admin) similar(n Node, limit int) []Similar {
	ix := a.Cat.Index
	mk := func(s Suggestion) Similar {
		m, _ := ix.Node(s.Node.ParentID)
		return Similar{ID: s.Node.ID, Name: s.Node.Name, MainID: m.ID, MainName: m.Name, Score: s.Score}
	}
	out := []Similar{}
	for _, s := range ix.Suggest(n.Name, func(x Node) bool {
		return IsSub(x) && x.ID != n.ID && x.ParentID == n.ParentID
	}, limit) {
		if s.Score >= 0.7 {
			out = append(out, mk(s))
		}
	}
	for _, s := range ix.Suggest(n.Name, func(x Node) bool {
		return IsSub(x) && x.ParentID != n.ParentID
	}, 3) {
		if s.Score >= 1 {
			out = append(out, mk(s))
		}
	}
	return out
}

// MarkSeen clears the "new" flag. No ids = all categories.
func (a *Admin) MarkSeen(ctx context.Context, ids []string) error {
	return a.Cat.Store.MarkSeen(ctx, ids)
}

/* ------------------------------ changes --------------------------------- */

func findNode(all []Node, id string) (Node, bool) {
	for _, n := range all {
		if n.ID == id {
			return n, true
		}
	}
	return Node{}, false
}

func (a *Admin) loadSub(ctx context.Context, id string) (Node, []Node, error) {
	all, err := a.Cat.Store.ListNodes(ctx)
	if err != nil {
		return Node{}, nil, err
	}
	n, ok := findNode(all, id)
	switch {
	case !ok:
		return Node{}, nil, ErrNotFound
	case IsMain(n):
		return Node{}, nil, ErrMainTypeLocked
	}
	return n, all, nil
}

// clash finds another category in parentID using the same name or alias.
func clash(all []Node, parentID, name, exceptID string) (Node, bool) {
	k := adminKey(name)
	for _, n := range all {
		if IsMain(n) || n.ParentID != parentID || n.ID == exceptID {
			continue
		}
		if adminKey(n.Name) == k {
			return n, true
		}
		for _, al := range n.Aliases {
			if adminKey(al) == k {
				return n, true
			}
		}
	}
	return Node{}, false
}

// addAliases appends spellings the target should also answer to, skipping
// blanks, duplicates and its own name.
func addAliases(existing []string, targetName string, add ...string) []string {
	seen := map[string]bool{adminKey(targetName): true}
	out := make([]string, 0, len(existing)+len(add))
	for _, s := range append(append([]string{}, existing...), add...) {
		k := adminKey(s)
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, s)
	}
	return out
}

// Rename fixes a name or spelling. All products show the new name at once,
// and the old spelling keeps resolving to this category.
func (a *Admin) Rename(ctx context.Context, id, newName string) (Node, error) {
	name := TidyName(newName)
	if !validName(name) {
		return Node{}, ErrInvalidName
	}
	n, all, err := a.loadSub(ctx, id)
	if err != nil {
		return Node{}, err
	}
	same := adminKey(name) == adminKey(n.Name) // only case/spacing changes
	if !same {
		if c, ok := clash(all, n.ParentID, name, n.ID); ok {
			return Node{}, &NameTakenError{Existing: c}
		}
		n.Aliases = addAliases(n.Aliases, name, n.Name)
	}
	n.Name, n.AdminSeen = name, true
	if err := a.Cat.Store.UpdateNode(ctx, n); err != nil {
		return Node{}, err
	}
	if err := a.Products.SetName(ctx, n.ID, name); err != nil {
		return Node{}, err
	}
	return n, a.Cat.Reload(ctx)
}

// SetImage sets (or, with an empty URL, removes) the picture of ANY category,
// main type or subcategory. Only the picture changes: name, aliases and
// products are left alone. The address must be https (it comes from the R2
// upload the admin just made).
func (a *Admin) SetImage(ctx context.Context, id, imageURL string) (Node, error) {
	imageURL = strings.TrimSpace(imageURL)
	if imageURL != "" {
		u, err := url.Parse(imageURL)
		if err != nil || u.Scheme != "https" || u.Host == "" || len(imageURL) > 500 {
			return Node{}, ErrInvalidImage
		}
	}
	all, err := a.Cat.Store.ListNodes(ctx)
	if err != nil {
		return Node{}, err
	}
	n, ok := findNode(all, id)
	if !ok {
		return Node{}, ErrNotFound
	}
	n.ImageURL = imageURL
	if err := a.Cat.Store.UpdateNode(ctx, n); err != nil {
		return Node{}, err
	}
	return n, a.Cat.Reload(ctx)
}

// Merge folds a duplicate into an existing category of the same Main Type:
// its products move across, its names become aliases of the target (so the
// same typo never recreates it), and the duplicate is removed.
func (a *Admin) Merge(ctx context.Context, fromID, intoID string) (int64, error) {
	if fromID == intoID {
		return 0, ErrSameCategory
	}
	from, all, err := a.loadSub(ctx, fromID)
	if err != nil {
		return 0, err
	}
	into, ok := findNode(all, intoID)
	if !ok || IsMain(into) {
		return 0, ErrNotFound
	}
	if from.ParentID != into.ParentID {
		return 0, ErrDifferentMain
	}
	moved, err := a.Products.Reassign(ctx, from.ID, into)
	if err != nil {
		return 0, err
	}
	into.Aliases = addAliases(into.Aliases, into.Name, append([]string{from.Name, from.RawInput}, from.Aliases...)...)
	into.AdminSeen = true
	if err := a.Cat.Store.UpdateNode(ctx, into); err != nil {
		return moved, err
	}
	if err := a.Cat.Store.DeleteNode(ctx, from.ID); err != nil {
		return moved, err
	}
	return moved, a.Cat.Reload(ctx)
}

// Move puts a category (and all its products) under another Main Type.
func (a *Admin) Move(ctx context.Context, id, newMainID string) (Node, error) {
	n, all, err := a.loadSub(ctx, id)
	if err != nil {
		return Node{}, err
	}
	m, ok := findNode(all, newMainID)
	if !ok || !IsMain(m) {
		return Node{}, ErrUnknownMain
	}
	if n.ParentID == m.ID {
		return n, nil
	}
	if c, ok := clash(all, m.ID, n.Name, n.ID); ok {
		return Node{}, &NameTakenError{Existing: c}
	}
	n.ParentID, n.AdminSeen = m.ID, true // the ID stays stable
	if err := a.Cat.Store.UpdateNode(ctx, n); err != nil {
		return Node{}, err
	}
	if err := a.Products.SetMain(ctx, n.ID, m.ID); err != nil {
		return Node{}, err
	}
	return n, a.Cat.Reload(ctx)
}

// Delete removes a category. If it still has products, moveToID must name
// another category to receive them; otherwise nothing is deleted.
func (a *Admin) Delete(ctx context.Context, id, moveToID string) (int64, error) {
	n, all, err := a.loadSub(ctx, id)
	if err != nil {
		return 0, err
	}
	count, err := a.Products.CountForCategory(ctx, n.ID)
	if err != nil {
		return 0, err
	}
	var moved int64
	if count > 0 {
		if moveToID == "" {
			return 0, &HasProductsError{Count: count}
		}
		if moveToID == n.ID {
			return 0, ErrSameCategory
		}
		to, ok := findNode(all, moveToID)
		if !ok || IsMain(to) {
			return 0, ErrNotFound
		}
		if moved, err = a.Products.Reassign(ctx, n.ID, to); err != nil {
			return 0, err
		}
	}
	if err := a.Cat.Store.DeleteNode(ctx, n.ID); err != nil {
		return moved, err
	}
	return moved, a.Cat.Reload(ctx)
}
