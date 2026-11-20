package handler

import (
	"context"
	"log/slog"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"shopbd/backend/internal/cache"
	"shopbd/backend/internal/database"
	"shopbd/backend/internal/httpx"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/taxonomy"
)

// MarketplaceHandler serves the public, all-sellers endpoints used by the
// home page:
//
//	GET /api/v1/marketplace/products   ?sort&q&category&page&limit&minReviews&minDiscount&maxDiscount
//	GET /api/v1/marketplace/sellers    ?sort&category&page&limit
//	GET /api/v1/marketplace/categories
//
// Only sellers with a PUBLISHED store appear, and only active products.
type MarketplaceHandler struct {
	DB  *mongo.Database
	Log *slog.Logger
	// Tax is the shared platform category index, used ONLY to standardize
	// seller category names on the marketplace. Optional (nil = names are just
	// tidied, not matched).
	Tax        *taxonomy.Index
	canonCache sync.Map // lower-cased seller name -> canonEntry

	// Snapshot cache (see marketplacecache.go). Zero values are ready to use.
	mu    sync.Mutex
	snap  *marketSnapshot
	build func(context.Context) (*marketSnapshot, error) // test seam; nil = MongoDB

	// Cache, when set, shares invalidations between API instances (version tag
	// in Redis). Nil = invalidation only affects this instance, as before.
	Cache        *cache.Cache
	snapVer      string        // shared version the current snapshot was built under
	lastShared   string        // last shared version read ("" = unknown)
	verCheckedAt time.Time     // when it was read
	verInterval  time.Duration // test seam; 0 = defaultVersionCheck

	// ServeStale: when the snapshot is only expired by age, answer with the
	// old copy at once and rebuild in the background, so no visitor waits for
	// the database. An invalidation (shop rename, suspension) still rebuilds
	// before answering. False (the default) = rebuild before answering.
	ServeStale bool
	refreshing bool      // a background rebuild is running
	gen        uint64    // bumped by InvalidateSnapshot; drops in-flight rebuilds
	retryAt    time.Time // no background rebuild before this time (after a failure)
}

const marketplaceTimeout = 8 * time.Second

/* ------------------------------------------------------------------ */
/* Wire shapes (match lib/api/marketplace.ts)                          */
/* ------------------------------------------------------------------ */

type mkSellerDTO struct {
	ID              string  `json:"id"`
	Slug            string  `json:"slug"`
	Name            string  `json:"name"`
	LogoURL         string  `json:"logoUrl"`
	BannerURL       string  `json:"bannerUrl"`
	Description     string  `json:"description"`
	IsVerified      bool    `json:"isVerified"`
	Rating          float64 `json:"rating"`
	ReviewCount     int     `json:"reviewCount"`
	ProductCount    int     `json:"productCount"`
	CompletedOrders int     `json:"completedOrders"`
	Followers       int     `json:"followers"`
}

type mkProductDTO struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	Slug         string  `json:"slug"`
	Description  string  `json:"description"`
	Price        float64 `json:"price"`
	ComparePrice float64 `json:"comparePrice,omitempty"`
	Stock        int     `json:"stock"`
	// HasVariants tells the home-page card to open the product page (to pick
	// size/colour) instead of adding straight to the cart.
	HasVariants  bool        `json:"hasVariants"`
	MainImageURL string      `json:"mainImageUrl,omitempty"`
	ImageURLs    []string    `json:"imageUrls"`
	CategoryID   string      `json:"categoryId"`
	CategoryName string      `json:"categoryName"`
	CategorySlug string      `json:"categorySlug"`
	Rating       float64     `json:"rating"`
	Reviews      int         `json:"reviews"`
	Sold         int         `json:"sold"`
	CreatedAt    string      `json:"createdAt"`
	Seller       mkSellerDTO `json:"seller"`
}

type mkCategoryDTO struct {
	ID       string `json:"id"`
	Slug     string `json:"slug"`
	Name     string `json:"name"`
	Emoji    string `json:"emoji"`
	ImageURL string `json:"imageUrl,omitempty"`
}

/* ------------------------------------------------------------------ */
/* Data loading                                                        */
/* ------------------------------------------------------------------ */

// storeInfo is the public identity of one seller's published store.
type storeInfo struct {
	SellerID    string
	Slug        string
	Name        string
	LogoURL     string
	BannerURL   string
	Description string
}

// publishedFields are the public fields read from a published store design.
type publishedFields struct {
	StoreName   string `bson:"storeName"`
	LogoURL     string `bson:"logoUrl"`
	CoverURL    string `bson:"coverUrl"`
	Description string `bson:"description"`
}

type storeDesignRow struct {
	SellerID  string   `bson:"sellerId"`
	Slug      string   `bson:"slug"`
	Published bson.Raw `bson:"published"`
}

func (h *MarketplaceHandler) fail(w http.ResponseWriter, r *http.Request, err error) {
	httpx.WriteError(w, r, err, h.Log)
}

// stores returns every published store keyed by seller ID.
func (h *MarketplaceHandler) stores(ctx context.Context) (map[string]*storeInfo, error) {
	// Suspended / banned sellers are not public: leave their stores out, which
	// also removes their products, categories and counts (everything else in
	// the snapshot is derived from this map).
	hidden, err := h.hiddenSellerIDs(ctx)
	if err != nil {
		return nil, err
	}
	filter := bson.D{{Key: "isPublished", Value: true}}
	if len(hidden) > 0 {
		filter = append(filter, bson.E{Key: "sellerId", Value: bson.D{{Key: "$nin", Value: hidden}}})
	}

	col := h.DB.Collection(database.StoreDesignsCollection)
	// Oldest first, so if a seller ever has more than one document the most
	// recently updated one (the current address) overwrites the others below.
	cur, err := col.Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "updatedAt", Value: 1}}))
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)

	out := map[string]*storeInfo{}
	for cur.Next(ctx) {
		var row storeDesignRow
		if err := cur.Decode(&row); err != nil {
			return nil, err
		}
		if row.SellerID == "" || row.Slug == "" {
			continue
		}
		info := &storeInfo{SellerID: row.SellerID, Slug: row.Slug, Name: row.Slug}
		if len(row.Published) > 0 {
			var pub publishedFields
			if err := bson.Unmarshal(row.Published, &pub); err == nil {
				if strings.TrimSpace(pub.StoreName) != "" {
					info.Name = strings.TrimSpace(pub.StoreName)
				}
				info.LogoURL = pub.LogoURL
				info.BannerURL = pub.CoverURL
				info.Description = pub.Description
			}
		}
		out[row.SellerID] = info
	}
	return out, cur.Err()
}

// hiddenSellerIDs returns the IDs of sellers whose account status keeps their
// store off the marketplace (see models.StoreHiddenStatuses).
func (h *MarketplaceHandler) hiddenSellerIDs(ctx context.Context) ([]string, error) {
	cur, err := h.DB.Collection(database.UsersCollection).Find(ctx,
		bson.D{{Key: "status", Value: bson.D{{Key: "$in", Value: models.StoreHiddenStatuses}}}},
		options.Find().SetProjection(bson.D{{Key: "_id", Value: 1}}),
	)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var ids []string
	for cur.Next(ctx) {
		var u struct {
			ID string `bson:"_id"`
		}
		if err := cur.Decode(&u); err != nil {
			return nil, err
		}
		ids = append(ids, u.ID)
	}
	return ids, cur.Err()
}

// products returns active products of the given sellers, newest first.
func (h *MarketplaceHandler) products(ctx context.Context, sellerIDs []string) ([]*models.Product, error) {
	if len(sellerIDs) == 0 {
		return nil, nil
	}
	col := h.DB.Collection(database.ProductsCollection)
	opts := options.Find().
		SetSort(bson.D{{Key: "createdAt", Value: -1}}).
		SetLimit(2000)
	cur, err := col.Find(ctx, bson.D{
		{Key: "isActive", Value: true},
		{Key: "sellerId", Value: bson.D{{Key: "$in", Value: sellerIDs}}},
	}, opts)
	if err != nil {
		return nil, err
	}
	var out []*models.Product
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// soldByProduct counts units in orders the seller has accepted
// (confirmed, shipped or delivered). Pending and cancelled do not count.
func (h *MarketplaceHandler) soldByProduct(ctx context.Context) (map[string]int, error) {
	col := h.DB.Collection(database.OrdersCollection)
	pipe := mongo.Pipeline{
		{{Key: "$match", Value: bson.D{{Key: "status", Value: bson.D{{Key: "$in", Value: bson.A{
			string(models.OrderConfirmed), string(models.OrderShipped), string(models.OrderDelivered),
		}}}}}}},
		{{Key: "$unwind", Value: "$lines"}},
		{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: "$lines.productId"},
			{Key: "qty", Value: bson.D{{Key: "$sum", Value: "$lines.qty"}}},
		}}},
	}
	cur, err := col.Aggregate(ctx, pipe)
	if err != nil {
		return nil, err
	}
	var rows []struct {
		ID  string `bson:"_id"`
		Qty int    `bson:"qty"`
	}
	if err := cur.All(ctx, &rows); err != nil {
		return nil, err
	}
	out := make(map[string]int, len(rows))
	for _, r := range rows {
		out[r.ID] = r.Qty
	}
	return out, nil
}

// deliveredBySeller counts delivered (completed) orders per seller.
func (h *MarketplaceHandler) deliveredBySeller(ctx context.Context) (map[string]int, error) {
	col := h.DB.Collection(database.OrdersCollection)
	pipe := mongo.Pipeline{
		{{Key: "$match", Value: bson.D{{Key: "status", Value: string(models.OrderDelivered)}}}},
		{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: "$sellerId"},
			{Key: "n", Value: bson.D{{Key: "$sum", Value: 1}}},
		}}},
	}
	cur, err := col.Aggregate(ctx, pipe)
	if err != nil {
		return nil, err
	}
	var rows []struct {
		ID string `bson:"_id"`
		N  int    `bson:"n"`
	}
	if err := cur.All(ctx, &rows); err != nil {
		return nil, err
	}
	out := make(map[string]int, len(rows))
	for _, r := range rows {
		out[r.ID] = r.N
	}
	return out, nil
}

func (h *MarketplaceHandler) categoriesByID(ctx context.Context) (map[string]*models.Category, error) {
	col := h.DB.Collection(database.CategoriesCollection)
	cur, err := col.Find(ctx, bson.D{}, options.Find().SetSort(bson.D{{Key: "position", Value: 1}}))
	if err != nil {
		return nil, err
	}
	var list []*models.Category
	if err := cur.All(ctx, &list); err != nil {
		return nil, err
	}
	out := make(map[string]*models.Category, len(list))
	for _, c := range list {
		out[c.ID] = c
	}
	return out, nil
}

/* ------------------------------------------------------------------ */
/* Helpers                                                             */
/* ------------------------------------------------------------------ */

func intParam(r *http.Request, key string, def, min, max int) int {
	v, err := strconv.Atoi(r.URL.Query().Get(key))
	if err != nil {
		return def
	}
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

// discountPercent is the whole-number discount of a product, 0 when it is
// not on sale. Same formula as the frontend (Math.round).
func discountPercent(p *models.Product) int {
	if p.ComparePrice <= 0 || p.ComparePrice <= p.Price || p.Price < 0 {
		return 0
	}
	return int(math.Round((1 - p.Price/p.ComparePrice) * 100))
}

// productCategorySlug returns the category slug of a product ("" if unknown).
func productCategorySlug(p *models.Product, cats map[string]*models.Category) string {
	if c, ok := cats[p.CategoryID]; ok {
		return c.Slug
	}
	return ""
}

// matchesCategory reports whether the product belongs to the (already
// lower-cased) category filter. An empty filter matches everything.
//
// The product's PLATFORM category (Main Type + category, set when the seller
// picks the product type) decides. A Main Type ID ("fashion") matches every
// category under it ("fashion.hoodie"), so the homepage can show a whole
// Main Type. Only products with no platform category yet (older data) fall
// back to the seller's own storefront category.
func (h *MarketplaceHandler) matchesCategory(p *models.Product, cats map[string]*models.Category, filter string) bool {
	if filter == "" {
		return true
	}
	mainFilter := !strings.Contains(filter, ".")
	if slug, _ := h.productStandardCategory(p, cats); slug != "" {
		slug = strings.ToLower(slug)
		if slug == filter || (mainFilter && strings.HasPrefix(slug, filter+".")) {
			return true
		}
	}
	if mainFilter && p.MainID != "" && strings.ToLower(p.MainID) == filter {
		return true // filed under a Main Type only
	}
	if p.SubID != "" || p.MainID != "" {
		return false // has a platform category: the seller's storefront category must not pull it elsewhere
	}
	return strings.ToLower(productCategorySlug(p, cats)) == filter ||
		strings.ToLower(p.CategoryID) == filter
}

func sellerDTO(s *storeInfo, productCount, completed int) mkSellerDTO {
	return mkSellerDTO{
		ID:              s.SellerID,
		Slug:            s.Slug,
		Name:            s.Name,
		LogoURL:         s.LogoURL,
		BannerURL:       s.BannerURL,
		Description:     s.Description,
		ProductCount:    productCount,
		CompletedOrders: completed,
	}
}

func paginate[T any](items []T, page, limit int) []T {
	start := (page - 1) * limit
	if start >= len(items) {
		return []T{}
	}
	end := start + limit
	if end > len(items) {
		end = len(items)
	}
	return items[start:end]
}

/* ------------------------------------------------------------------ */
/* Handlers                                                            */
/* ------------------------------------------------------------------ */

// Products — GET /api/v1/marketplace/products
//
// ?q does Google-style matching (see search_index.go): every typed word must
// match the start of a word in the product, in any order, with small typo
// tolerance. Results are ranked by relevance.
func (h *MarketplaceHandler) Products(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	sortBy := q.Get("sort")
	search := strings.ToLower(strings.TrimSpace(q.Get("q")))
	search = strings.Join(strings.Fields(search), " ") // collapse repeated spaces
	searchToks := tokenize(search)
	categoryFilter := strings.ToLower(strings.TrimSpace(q.Get("category")))
	page := intParam(r, "page", 1, 1, 10000)
	limit := intParam(r, "limit", 12, 1, 60)
	minReviews := intParam(r, "minReviews", 0, 0, 1000000)
	// 0 = "not set". Any value > 0 also means "only discounted products".
	minDiscount := intParam(r, "minDiscount", 0, 0, 100)
	maxDiscount := intParam(r, "maxDiscount", 0, 0, 100)
	discountFilter := minDiscount > 0 || maxDiscount > 0

	snap, err := h.snapshot()
	if err != nil {
		h.fail(w, r, err)
		return
	}
	stores, all, cats, sold, completed, counts := snap.stores, snap.products, snap.cats, snap.sold, snap.completed, snap.counts

	type row struct {
		p     *models.Product
		slug  string
		score int
		off   int
	}
	rows := make([]row, 0, len(all))
	// The index is built together with the snapshot and is parallel to
	// snap.products. If it is missing (e.g. in tests), docs are built on the fly.
	useIdx := len(snap.index) == len(all)
	for i, p := range all {
		if !h.matchesCategory(p, cats, categoryFilter) {
			continue
		}
		// Reviews are not collected yet, so every product has 0 reviews.
		if minReviews > 0 {
			continue
		}
		off := discountPercent(p)
		if discountFilter {
			if off == 0 || off < minDiscount || (maxDiscount > 0 && off > maxDiscount) {
				continue
			}
		}
		score := 0
		if search != "" {
			var doc *searchDoc
			if useIdx {
				doc = snap.index[i]
			} else {
				doc = buildSearchIndex([]*models.Product{p}, stores)[0]
			}
			score = doc.score(search, searchToks)
			if score == 0 {
				continue
			}
		}
		rows = append(rows, row{p: p, slug: productCategorySlug(p, cats), score: score, off: off})
	}

	// `all` is already newest-first, so stable sorts keep newest as tiebreak.
	switch sortBy {
	case "top_selling":
		sort.SliceStable(rows, func(i, j int) bool { return sold[rows[i].p.ID] > sold[rows[j].p.ID] })
	case "price_asc":
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].p.Price < rows[j].p.Price })
	case "price_desc":
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].p.Price > rows[j].p.Price })
	case "relevance", "":
		// A text search with no explicit sort also ranks by relevance.
		if search != "" {
			sort.SliceStable(rows, func(i, j int) bool { return rows[i].score > rows[j].score })
		}
	default: // "newest", "best_rated" (no reviews yet) and unknown values
	}
	// Deals: biggest discount first, so the home page shows the best ones
	// even when the category has more than one page of deals.
	if discountFilter && (sortBy == "" || sortBy == "newest") {
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].off > rows[j].off })
	}

	total := len(rows)
	pageRows := paginate(rows, page, limit)

	out := make([]mkProductDTO, 0, len(pageRows))
	for _, rw := range pageRows {
		p := rw.p
		store := stores[p.SellerID]
		imgs := p.ImageURLs
		if imgs == nil {
			imgs = []string{}
		}
		catSlug, catName := h.productStandardCategory(p, cats)
		if catSlug == "" {
			catSlug, catName = rw.slug, p.CategoryName
		}
		out = append(out, mkProductDTO{
			ID:           p.ID,
			Name:         p.Name,
			Slug:         p.Slug,
			Description:  p.Description,
			Price:        p.Price,
			ComparePrice: p.ComparePrice,
			Stock:        p.Stock,
			HasVariants:  len(p.Combinations) > 0,
			MainImageURL: p.MainImageURL,
			ImageURLs:    imgs,
			CategoryID:   p.CategoryID,
			CategoryName: catName,
			CategorySlug: catSlug,
			Rating:       0,
			Reviews:      0,
			Sold:         sold[p.ID],
			CreatedAt:    p.CreatedAt.UTC().Format(time.RFC3339),
			Seller:       sellerDTO(store, counts[p.SellerID], completed[p.SellerID]),
		})
	}

	setPublicCache(w)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"products": out,
		"page":     page,
		"limit":    limit,
		"total":    total,
	})
}

// Sellers — GET /api/v1/marketplace/sellers
//
// ?category=<slug> keeps only sellers that have at least one active product
// in that category.
func (h *MarketplaceHandler) Sellers(w http.ResponseWriter, r *http.Request) {
	page := intParam(r, "page", 1, 1, 10000)
	limit := intParam(r, "limit", 12, 1, 60)
	categoryFilter := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("category")))

	snap, err := h.snapshot()
	if err != nil {
		h.fail(w, r, err)
		return
	}
	stores, completed, counts := snap.stores, snap.completed, snap.counts

	// Sellers that sell something in the requested category.
	var inCategory map[string]bool
	if categoryFilter != "" {
		inCategory = make(map[string]bool)
		for _, p := range snap.products {
			if !inCategory[p.SellerID] && h.matchesCategory(p, snap.cats, categoryFilter) {
				inCategory[p.SellerID] = true
			}
		}
	}

	list := make([]mkSellerDTO, 0, len(stores))
	for _, s := range stores {
		if inCategory != nil && !inCategory[s.SellerID] {
			continue
		}
		list = append(list, sellerDTO(s, counts[s.SellerID], completed[s.SellerID]))
	}
	// "popular": most completed orders, then most products, then name.
	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if a.CompletedOrders != b.CompletedOrders {
			return a.CompletedOrders > b.CompletedOrders
		}
		if a.ProductCount != b.ProductCount {
			return a.ProductCount > b.ProductCount
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	})

	setPublicCache(w)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"sellers": paginate(list, page, limit),
		"page":    page,
		"limit":   limit,
		"total":   len(list),
	})
}

// Categories — GET /api/v1/marketplace/categories
// One entry per standardized category that has at least one active product of
// a published store, built from the products' PLATFORM categories (so a
// product moved to another type shows up under its new category). Products
// without a platform category (older data) still count through their seller
// category. "Hodie", "hoodies" and "HOODIE" all become "Hoodie". No images here.
func (h *MarketplaceHandler) Categories(w http.ResponseWriter, r *http.Request) {
	snap, err := h.snapshot()
	if err != nil {
		h.fail(w, r, err)
		return
	}
	cats := snap.cats

	type entry struct{ slug, name, emoji string }
	seen := map[string]bool{}
	list := make([]entry, 0, 32)
	for _, p := range snap.products { // already limited to active products of published stores
		slug, name := h.productStandardCategory(p, cats)
		if slug == "" || seen[slug] {
			continue
		}
		seen[slug] = true
		emoji := ""
		if c, ok := cats[p.CategoryID]; ok {
			emoji = c.Emoji
		}
		list = append(list, entry{slug, name, emoji})
	}
	sort.SliceStable(list, func(i, j int) bool {
		return strings.ToLower(list[i].name) < strings.ToLower(list[j].name)
	})

	out := make([]mkCategoryDTO, 0, len(list))
	for _, e := range list {
		out = append(out, mkCategoryDTO{ID: e.slug, Slug: e.slug, Name: e.name, Emoji: e.emoji})
	}

	setPublicCache(w)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"categories": out})
}

type canonEntry struct {
	slug, name string
	at         time.Time
}

// standardCategory turns whatever a seller typed into the marketplace name:
// the official taxonomy name when it matches one (typos, case, plurals and
// extra words are tolerated), otherwise a tidied version of the text. The slug
// is stable for every spelling, so filters and merging work across sellers.
func (h *MarketplaceHandler) standardCategory(raw string) (slug, name string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", ""
	}
	key := strings.ToLower(raw)
	if v, ok := h.canonCache.Load(key); ok {
		if e := v.(canonEntry); time.Since(e.at) < time.Minute { // short TTL: admin renames show soon
			return e.slug, e.name
		}
	}
	if h.Tax != nil {
		m := h.Tax.ResolveText(raw, func(taxonomy.Node) bool { return true })
		if n, ok := h.Tax.Node(m.NodeID); ok && m.Found() && !m.Ambiguous && !m.Extra {
			slug, name = n.ID, n.Name
		}
	}
	if slug == "" {
		name = taxonomy.TidyName(raw)
		k := taxonomy.KeyOf(taxonomy.Normalize(name))
		if k == "" {
			k = strings.ToLower(name)
		}
		slug = "c-" + strings.ReplaceAll(k, " ", "-")
	}
	h.canonCache.Store(key, canonEntry{slug: slug, name: name, at: time.Now()})
	return slug, name
}

// productStandardCategory is the standardized category of a product's seller category.
func (h *MarketplaceHandler) productStandardCategory(p *models.Product, cats map[string]*models.Category) (slug, name string) {
	// The category the seller chose for the product on the platform tree wins.
	// Editing the product type changes SubID/MainID, so the product moves to
	// the new Main Type and category at once (the seller's storefront
	// category is a separate thing and is not used here).
	if p.SubID != "" {
		name = p.SubName
		if h.Tax != nil {
			if n, ok := h.Tax.Node(p.SubID); ok {
				name = n.Name // current official name (admin renames show immediately)
			}
		}
		if name != "" {
			return p.SubID, name
		}
	}
	raw := p.CategoryName
	if c, ok := cats[p.CategoryID]; ok {
		raw = c.Name
	}
	return h.standardCategory(raw)
}
