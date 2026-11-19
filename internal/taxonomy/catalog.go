package taxonomy

import (
	"context"
	"sync"
	"time"
)

// NodeStore is the persistence layer for taxonomy nodes (MongoDB in
// production, a fake in tests). It is separate from the older Store interface
// in resolve.go, which is the suggestion-queue design that Part 2 retires.
type NodeStore interface {
	// ListNodes returns every node, active or not.
	ListNodes(ctx context.Context) ([]Node, error)
	// InsertMissing inserts nodes whose ID does not exist yet and never
	// touches existing ones (so admin renames and deletions are safe).
	InsertMissing(ctx context.Context, nodes []Node) error
	CountNodes(ctx context.Context) (int64, error)
	// InsertIfAbsent inserts one node and reports whether it was inserted
	// (false = a node with that ID already exists). Must be atomic.
	InsertIfAbsent(ctx context.Context, n Node) (bool, error)
	// UpdateNode replaces the node with the same ID.
	UpdateNode(ctx context.Context, n Node) error
	DeleteNode(ctx context.Context, id string) error
	// MarkSeen sets AdminSeen=true on the given IDs (all nodes if empty).
	MarkSeen(ctx context.Context, ids []string) error
}

// Catalog keeps the in-memory Index in sync with the database. Index is the
// single shared pointer: hand it to handlers and services once, and Reload
// updates it in place.
type Catalog struct {
	Index *Index
	Store NodeStore
	mu    sync.Mutex // serialises reloads
}

// NewCatalog loads the taxonomy from the store.
//
//   - Empty database: the whole seed (10 Main Types + starter categories) is inserted once.
//   - Otherwise: only the 10 Main Types are re-checked, so they always exist,
//     while starter categories an admin deleted are NOT brought back.
func NewCatalog(ctx context.Context, store NodeStore) (*Catalog, error) {
	n, err := store.CountNodes(ctx)
	if err != nil {
		return nil, err
	}
	seed := Seed()
	toInsert := seed
	if n > 0 {
		toInsert = toInsert[:0:0]
		for _, s := range seed {
			if IsMain(s) {
				toInsert = append(toInsert, s)
			}
		}
	}
	if err := store.InsertMissing(ctx, toInsert); err != nil {
		return nil, err
	}
	c := &Catalog{Index: NewIndex(nil, nil), Store: store}
	if err := c.Reload(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

// Reload re-reads every node and swaps the shared Index. Call it after any
// change to the taxonomy.
func (c *Catalog) Reload(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	nodes, err := c.Store.ListNodes(ctx)
	if err != nil {
		return err
	}
	c.Index.Replace(nodes, nil)
	return nil
}

// Resolve is the seller-facing entry point.
//
//   - Strong match under the chosen Main Type -> the official category is used.
//   - No close match -> a new category is created immediately, live and public,
//     with NO pending status and no approval step.
//
// The new category is stored with AdminSeen=false; that flag is the
// "new category" notification the admin dashboard counts.
func (c *Catalog) Resolve(ctx context.Context, in Input, who Actor) (Result, error) {
	for attempt := 0; attempt < 4; attempt++ {
		p, err := Plan(c.Index, in)
		if err != nil {
			return Result{}, err
		}
		if !p.Create {
			return p.Result, nil
		}
		storeName := who.StoreName
		if storeName == "" && who.StoreNameFn != nil {
			storeName = who.StoreNameFn(ctx)
		}
		node := Node{
			ID:                 newNodeID(p.Result.MainID, p.NewName, attempt),
			ParentID:           p.Result.MainID,
			Name:               p.NewName,
			SortOrder:          1000, // after the starter categories
			IsActive:           true,
			ShowOnHome:         true,
			Source:             SourceSeller,
			CreatedAt:          time.Now().UTC(),
			CreatedBySellerID:  who.SellerID,
			CreatedByStoreName: storeName,
			CreatedVia:         who.Via,
			CreatedFromProduct: who.ProductID,
			RawInput:           p.Result.Raw,
			AdminSeen:          false,
		}
		ok, err := c.Store.InsertIfAbsent(ctx, node)
		if err != nil {
			return Result{}, err
		}
		if ok {
			c.Index.Add(node)
			r := p.Result
			r.CategoryID, r.CategoryName, r.Created, r.Node = node.ID, node.Name, true, &node
			return r, nil
		}
		// Someone else just created it (or the ID clashed): reload and re-plan,
		// which will now find their category instead of making a duplicate.
		if err := c.Reload(ctx); err != nil {
			return Result{}, err
		}
	}
	return Result{}, ErrCreateConflict
}
