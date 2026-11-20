package handler

import (
	"context"
	"net/http"
	"time"

	"golang.org/x/sync/errgroup"

	"shopbd/backend/internal/models"
)

// marketplaceTag is the shared-cache version tag for the marketplace. Bumping
// it (InvalidateSnapshot) tells every API instance to drop its snapshot.
const marketplaceTag = "marketplace"

// defaultVersionCheck is how often an instance asks Redis whether another
// instance invalidated the marketplace. One tiny MGET per second per instance.
const defaultVersionCheck = time.Second

// marketSnapshotTTL is how long the public marketplace data is reused before
// it is rebuilt from MongoDB. Everything in the snapshot is public data
// (published stores, active products, public categories, aggregate counts),
// identical for every visitor, so sharing it between requests cannot leak
// per-user data. Seller edits become visible on the marketplace within this
// window.
const marketSnapshotTTL = 30 * time.Second

// marketSnapshotMaxStale bounds ServeStale: an older snapshot is rebuilt
// before answering.
const marketSnapshotMaxStale = 10 * time.Minute

// marketSnapshot is a read-only view of everything the marketplace endpoints
// need. Handlers MUST NOT mutate the maps or slices inside it.
type marketSnapshot struct {
	stores    map[string]*storeInfo
	products  []*models.Product // active products of published stores, newest first
	index     []*searchDoc      // search tokens, parallel to products
	cats      map[string]*models.Category
	sold      map[string]int // productID -> units in accepted orders
	completed map[string]int // sellerID  -> delivered orders
	counts    map[string]int // sellerID  -> active product count
	builtAt   time.Time
}

// buildSnapshot loads the snapshot from MongoDB using the same queries the
// handlers previously ran on every request.
func (h *MarketplaceHandler) buildSnapshot(ctx context.Context) (*marketSnapshot, error) {
	// The queries are independent except products (which need the store ids),
	// so they run in parallel: the build takes about two database round trips
	// instead of five or six in a row.
	var (
		stores    map[string]*storeInfo
		prods     []*models.Product
		cats      map[string]*models.Category
		sold      map[string]int
		completed map[string]int
	)
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		var err error
		if stores, err = h.stores(gctx); err != nil {
			return err
		}
		ids := make([]string, 0, len(stores))
		for id := range stores {
			ids = append(ids, id)
		}
		prods, err = h.products(gctx, ids)
		return err
	})
	g.Go(func() error {
		var err error
		cats, err = h.categoriesByID(gctx)
		return err
	})
	g.Go(func() error {
		var err error
		sold, err = h.soldByProduct(gctx)
		return err
	})
	g.Go(func() error {
		var err error
		completed, err = h.deliveredBySeller(gctx)
		return err
	})
	if err := g.Wait(); err != nil {
		return nil, err
	}
	counts := make(map[string]int, len(stores))
	for _, p := range prods {
		counts[p.SellerID]++
	}
	return &marketSnapshot{
		stores:    stores,
		products:  prods,
		index:     buildSearchIndex(prods, stores),
		cats:      cats,
		sold:      sold,
		completed: completed,
		counts:    counts,
		builtAt:   time.Now(),
	}, nil
}

// Warm builds the snapshot once, so the first visitor after a start does not
// wait for it. Call it in a goroutine at startup.
func (h *MarketplaceHandler) Warm() { _, _ = h.snapshot() }

// snapshot returns the cached snapshot, rebuilding it when older than the TTL
// or when another instance invalidated the marketplace (shared version tag).
//
// The mutex is held during a rebuild on purpose: concurrent requests wait for
// one rebuild instead of each issuing their own (no cache stampede). The
// rebuild uses its own bounded context, so one client disconnecting cannot
// fail the rebuild for everyone else. If a rebuild fails and an older
// snapshot exists, the older one is served rather than returning an error.
//
// The shared version is read BEFORE the rebuild and stored with the new
// snapshot, so an invalidation that lands during a rebuild makes the next check
// rebuild again instead of being lost. If Redis is unavailable the snapshot
// simply ages out by TTL, as it did before.
func (h *MarketplaceHandler) snapshot() (*marketSnapshot, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	shared := h.sharedVersion()
	if h.snap != nil && (shared == "" || shared == h.snapVer) {
		age := time.Since(h.snap.builtAt)
		if age < marketSnapshotTTL {
			return h.snap, nil
		}
		// Expired by age only, nobody invalidated it: with ServeStale the
		// visitor gets the old copy now and a fresh one is built behind it.
		if h.ServeStale && age < marketSnapshotMaxStale {
			h.refreshInBackground(shared)
			return h.snap, nil
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), marketplaceTimeout)
	defer cancel()

	build := h.build
	if build == nil {
		build = h.buildSnapshot
	}
	snap, err := build(ctx)
	if err != nil {
		if h.snap != nil {
			if h.Log != nil {
				h.Log.Warn("marketplace snapshot rebuild failed; serving stale data", "error", err.Error())
			}
			return h.snap, nil
		}
		return nil, err
	}
	h.snap = snap
	h.snapVer = shared
	return snap, nil
}

// refreshInBackground rebuilds the snapshot without blocking the caller. It is
// called with h.mu held. One rebuild runs at a time; after a failure the next
// attempt waits 5 seconds. A rebuild that finishes after an invalidation, or
// that is older than what is already stored, is dropped.
func (h *MarketplaceHandler) refreshInBackground(shared string) {
	if h.refreshing || time.Now().Before(h.retryAt) {
		return
	}
	h.refreshing = true
	gen := h.gen
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), marketplaceTimeout)
		defer cancel()
		build := h.build
		if build == nil {
			build = h.buildSnapshot
		}
		snap, err := build(ctx)

		h.mu.Lock()
		defer h.mu.Unlock()
		h.refreshing = false
		if err != nil {
			h.retryAt = time.Now().Add(5 * time.Second)
			if h.Log != nil {
				h.Log.Warn("marketplace snapshot background rebuild failed; serving stale data", "error", err.Error())
			}
			return
		}
		if h.gen != gen || (h.snap != nil && !snap.builtAt.After(h.snap.builtAt)) {
			return
		}
		h.snap = snap
		h.snapVer = shared
	}()
}

// sharedVersion returns the fleet-wide marketplace version, or "" when it is
// unknown (no cache configured or Redis unavailable). Called with h.mu held;
// the Redis call is bounded and throttled.
func (h *MarketplaceHandler) sharedVersion() string {
	if h.Cache == nil {
		return ""
	}
	every := h.verInterval
	if every <= 0 {
		every = defaultVersionCheck
	}
	if !h.verCheckedAt.IsZero() && time.Since(h.verCheckedAt) < every {
		return h.lastShared
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	v, err := h.Cache.Version(ctx, marketplaceTag)
	h.verCheckedAt = time.Now()
	if err != nil {
		h.lastShared = ""
		return ""
	}
	h.lastShared = v
	return v
}

// InvalidateSnapshot drops the cached snapshot so the next request rebuilds
// it from MongoDB, on this instance at once and on every other instance within
// about a second (shared version tag). Called when a shop name or URL changed,
// or a seller was suspended, so the old data is not served from the cache.
func (h *MarketplaceHandler) InvalidateSnapshot() {
	h.mu.Lock()
	h.snap = nil
	h.gen++
	h.verCheckedAt = time.Time{}
	h.mu.Unlock()
	if h.Cache != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		// Failures are logged and retried in the background by the cache.
		_ = h.Cache.Invalidate(ctx, marketplaceTag)
	}
}

// setPublicCache marks a response as cacheable by browsers/CDNs for a short
// time. Only call this for responses that are identical for every visitor.
func setPublicCache(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "public, max-age=10, stale-while-revalidate=30")
}
