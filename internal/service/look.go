package service

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/repository"
)

// LookGate decides whether a seller may use Try the Look.
// *FeatureService satisfies it.
type LookGate interface {
	Require(ctx context.Context, sellerID, feature string) error
	HasAccess(ctx context.Context, sellerID, feature string) (bool, error)
}

// LookProducts reads the seller's products. *mongorepo.Products satisfies it.
type LookProducts interface {
	FindByID(ctx context.Context, sellerID, id string) (*models.Product, error)
	FindByIDs(ctx context.Context, sellerID string, ids []string) ([]*models.Product, error)
}

// LookProductCreator makes a brand-new product through the normal product
// flow (same validation, plan limit and category mapping as the product page).
// *ProductService satisfies it.
type LookProductCreator interface {
	Create(ctx context.Context, sellerID string, in ProductInput) (*models.Product, error)
}

type LookService struct {
	looks    repository.LookRepository
	products LookProducts
	creator  LookProductCreator
	gate     LookGate
	now      func() time.Time

	// Optional, for the public page.
	sellerStatus func(ctx context.Context, sellerID string) (models.Status, error)
	storeSlug    func(ctx context.Context, sellerID string) string
	// Optional, for the public list of one store's looks.
	sellerBySlug func(ctx context.Context, slug string) (string, error)
}

func NewLookService(looks repository.LookRepository, products LookProducts, creator LookProductCreator, gate LookGate) *LookService {
	return &LookService{looks: looks, products: products, creator: creator, gate: gate, now: time.Now}
}

// SetPublic provides the seller's account status and current store address,
// which the public page needs to hide suspended sellers and to build links.
func (s *LookService) SetPublic(
	status func(ctx context.Context, sellerID string) (models.Status, error),
	slug func(ctx context.Context, sellerID string) string,
) {
	s.sellerStatus = status
	s.storeSlug = slug
}

/* ------------------------------------------------------------------ */
/* Errors and validation                                               */
/* ------------------------------------------------------------------ */

func mapLookErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, repository.ErrNotFound):
		return apperr.NotFound()
	case errors.Is(err, repository.ErrLookFull):
		return apperr.New(http.StatusConflict, "LOOK_FULL", "A look can have up to 30 products.")
	case errors.Is(err, repository.ErrLookItemExists):
		return apperr.New(http.StatusConflict, "LOOK_ITEM_EXISTS", "This product is already in the look.")
	case errors.Is(err, repository.ErrLookItemMissing):
		return apperr.New(http.StatusNotFound, "LOOK_ITEM_NOT_FOUND", "This product is not in the look.")
	case errors.Is(err, repository.ErrLookItemsChanged):
		return apperr.New(http.StatusConflict, "LOOK_CHANGED", "The look was changed elsewhere. Reload and try again.")
	}
	return err
}

// validModelImageURL accepts only http(s) addresses. Inline data URIs are
// refused: a model photo is large and belongs in image storage, not in the
// database.
func validModelImageURL(u string) bool {
	u = strings.TrimSpace(u)
	l := strings.ToLower(u)
	if len(u) > 2048 || !(strings.HasPrefix(l, "https://") || strings.HasPrefix(l, "http://")) {
		return false
	}
	return safeImageURL(u)
}

// LookInput is what the seller sends to create or edit a look.
type LookInput struct {
	Title         string
	Gender        string // "", male or female
	ModelImageURL string
}

func cleanLookInput(in LookInput) (LookInput, error) {
	in.Title = strings.TrimSpace(in.Title)
	in.Gender = strings.ToLower(strings.TrimSpace(in.Gender))
	in.ModelImageURL = strings.TrimSpace(in.ModelImageURL)
	fields := map[string]string{}
	if in.Title == "" {
		fields["title"] = "give the look a name"
	} else if utf8.RuneCountInString(in.Title) > models.MaxLookTitleLength {
		fields["title"] = "name is too long (max 80 characters)"
	}
	if in.Gender != "" && in.Gender != models.LookGenderMale && in.Gender != models.LookGenderFemale {
		fields["gender"] = "must be male or female"
	}
	if in.ModelImageURL == "" {
		fields["modelImageUrl"] = "upload the main model image"
	} else if !validModelImageURL(in.ModelImageURL) {
		fields["modelImageUrl"] = "invalid image address"
	}
	if len(fields) > 0 {
		return in, apperr.Validation(fields)
	}
	return in, nil
}

/* ------------------------------------------------------------------ */
/* Views                                                               */
/* ------------------------------------------------------------------ */

// Item availability, as shown to the seller and the buyer.
const (
	LookItemAvailable   = "available"
	LookItemOutOfStock  = "out_of_stock"
	LookItemUnpublished = "unpublished" // the product exists but is switched off
	LookItemRemoved     = "removed"     // the product was deleted
)

// LookItemView is a product as it is RIGHT NOW (read live, never copied).
type LookItemView struct {
	ProductID    string  `json:"productId"`
	Name         string  `json:"name"`
	Slug         string  `json:"slug"`
	Price        float64 `json:"price"`
	ComparePrice float64 `json:"comparePrice,omitempty"`
	ImageURL     string  `json:"imageUrl"`
	Stock        int     `json:"stock"`
	InStock      bool    `json:"inStock"`
	Status       string  `json:"status"`
}

type LookView struct {
	ID            string         `json:"id"`
	Title         string         `json:"title"`
	Gender        string         `json:"gender,omitempty"`
	ModelImageURL string         `json:"modelImageUrl"`
	Status        string         `json:"status"`
	Items         []LookItemView `json:"items"`
	MaxItems      int            `json:"maxItems"`
	// NeedsAttention counts items that are removed or unpublished, so the
	// editor can warn the seller.
	NeedsAttention int       `json:"needsAttention"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

type LookSummary struct {
	ID             string    `json:"id"`
	Title          string    `json:"title"`
	Gender         string    `json:"gender,omitempty"`
	ModelImageURL  string    `json:"modelImageUrl"`
	Status         string    `json:"status"`
	ItemCount      int       `json:"itemCount"`
	NeedsAttention int       `json:"needsAttention"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

// PublicLookView is what a buyer sees: only products that can be shown.
type PublicLookView struct {
	ID            string         `json:"id"`
	Title         string         `json:"title"`
	Gender        string         `json:"gender,omitempty"`
	ModelImageURL string         `json:"modelImageUrl"`
	StoreSlug     string         `json:"storeSlug"`
	Items         []LookItemView `json:"items"`
}

func productImage(p *models.Product) string {
	if strings.TrimSpace(p.MainImageURL) != "" {
		return p.MainImageURL
	}
	for _, u := range p.ImageURLs {
		if strings.TrimSpace(u) != "" {
			return u
		}
	}
	return ""
}

func lookItemView(productID string, p *models.Product) LookItemView {
	if p == nil {
		return LookItemView{ProductID: productID, Status: LookItemRemoved}
	}
	v := LookItemView{
		ProductID: p.ID, Name: p.Name, Slug: p.Slug,
		Price: p.Price, ComparePrice: p.ComparePrice,
		ImageURL: productImage(p), Stock: p.Stock, InStock: p.Stock > 0,
	}
	switch {
	case !p.IsActive:
		v.Status = LookItemUnpublished
	case p.Stock <= 0:
		v.Status = LookItemOutOfStock
	default:
		v.Status = LookItemAvailable
	}
	return v
}

// resolve reads every product of the look in one query and keeps the look's order.
func (s *LookService) resolve(ctx context.Context, l *models.Look) ([]LookItemView, error) {
	ids := make([]string, len(l.Items))
	for i, it := range l.Items {
		ids[i] = it.ProductID
	}
	found, err := s.products.FindByIDs(ctx, l.SellerID, ids)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]*models.Product, len(found))
	for _, p := range found {
		byID[p.ID] = p
	}
	out := make([]LookItemView, 0, len(ids))
	for _, id := range ids {
		out = append(out, lookItemView(id, byID[id]))
	}
	return out, nil
}

func needsAttention(items []LookItemView) int {
	n := 0
	for _, it := range items {
		if it.Status == LookItemRemoved || it.Status == LookItemUnpublished {
			n++
		}
	}
	return n
}

func (s *LookService) view(ctx context.Context, l *models.Look) (*LookView, error) {
	items, err := s.resolve(ctx, l)
	if err != nil {
		return nil, err
	}
	return &LookView{
		ID: l.ID, Title: l.Title, Gender: l.Gender, ModelImageURL: l.ModelImageURL,
		Status: l.Status, Items: items, MaxItems: models.MaxLookItems,
		NeedsAttention: needsAttention(items),
		CreatedAt:      l.CreatedAt, UpdatedAt: l.UpdatedAt,
	}, nil
}

/* ------------------------------------------------------------------ */
/* Seller: looks                                                       */
/* ------------------------------------------------------------------ */

func (s *LookService) require(ctx context.Context, sellerID string) error {
	return s.gate.Require(ctx, sellerID, models.FeatureTryTheLook)
}

// List returns the seller's looks. Reading needs no access, so a seller whose
// access was turned off can still see (and delete) what they made.
func (s *LookService) List(ctx context.Context, sellerID string) ([]LookSummary, error) {
	rows, err := s.looks.ListBySeller(ctx, sellerID)
	if err != nil {
		return nil, err
	}
	out := make([]LookSummary, 0, len(rows))
	for _, l := range rows {
		items, err := s.resolve(ctx, l)
		if err != nil {
			return nil, err
		}
		out = append(out, LookSummary{
			ID: l.ID, Title: l.Title, Gender: l.Gender, ModelImageURL: l.ModelImageURL,
			Status: l.Status, ItemCount: len(l.Items),
			NeedsAttention: needsAttention(items), UpdatedAt: l.UpdatedAt,
		})
	}
	return out, nil
}

func (s *LookService) Get(ctx context.Context, sellerID, id string) (*LookView, error) {
	l, err := s.looks.FindByID(ctx, sellerID, id)
	if err != nil {
		return nil, mapLookErr(err)
	}
	return s.view(ctx, l)
}

func (s *LookService) Create(ctx context.Context, sellerID string, in LookInput) (*LookView, error) {
	if err := s.require(ctx, sellerID); err != nil {
		return nil, err
	}
	in, err := cleanLookInput(in)
	if err != nil {
		return nil, err
	}
	n, err := s.looks.CountBySeller(ctx, sellerID)
	if err != nil {
		return nil, err
	}
	if n >= models.MaxLooksPerSeller {
		return nil, apperr.New(http.StatusConflict, "LOOK_LIMIT",
			"You have reached the maximum number of looks. Delete one to add another.")
	}
	now := s.now().UTC()
	l := &models.Look{
		ID: models.NewID(), SellerID: sellerID, Title: in.Title, Gender: in.Gender,
		ModelImageURL: in.ModelImageURL, Items: []models.LookItem{},
		Status: models.LookDraft, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.looks.Create(ctx, l); err != nil {
		return nil, err
	}
	return s.view(ctx, l)
}

func (s *LookService) Update(ctx context.Context, sellerID, id string, in LookInput) (*LookView, error) {
	if err := s.require(ctx, sellerID); err != nil {
		return nil, err
	}
	in, err := cleanLookInput(in)
	if err != nil {
		return nil, err
	}
	if err := s.looks.UpdateMeta(ctx, sellerID, id, in.Title, in.Gender, in.ModelImageURL, s.now().UTC()); err != nil {
		return nil, mapLookErr(err)
	}
	return s.Get(ctx, sellerID, id)
}

// Delete removes the look only. The products in it are never touched.
func (s *LookService) Delete(ctx context.Context, sellerID, id string) error {
	return mapLookErr(s.looks.Delete(ctx, sellerID, id))
}

/* ------------------------------------------------------------------ */
/* Seller: products in a look                                          */
/* ------------------------------------------------------------------ */

// loadForEdit checks access and ownership and that the look has room.
func (s *LookService) loadForEdit(ctx context.Context, sellerID, id string) (*models.Look, error) {
	if err := s.require(ctx, sellerID); err != nil {
		return nil, err
	}
	l, err := s.looks.FindByID(ctx, sellerID, id)
	if err != nil {
		return nil, mapLookErr(err)
	}
	return l, nil
}

// publishedProduct loads one of the seller's products and makes sure it is
// published (a look only links products buyers can actually open).
func (s *LookService) publishedProduct(ctx context.Context, sellerID, productID string) (*models.Product, error) {
	p, err := s.products.FindByID(ctx, sellerID, strings.TrimSpace(productID))
	if errors.Is(err, repository.ErrNotFound) {
		return nil, apperr.Validation(map[string]string{"productId": "product not found in your store"})
	}
	if err != nil {
		return nil, err
	}
	if !p.IsActive {
		return nil, apperr.Validation(map[string]string{"productId": "publish this product first, then add it to the look"})
	}
	return p, nil
}

// AddExisting connects a product that is already in the seller's store. It
// stores only a reference, so nothing is duplicated.
func (s *LookService) AddExisting(ctx context.Context, sellerID, lookID, productID string) (*LookView, error) {
	l, err := s.loadForEdit(ctx, sellerID, lookID)
	if err != nil {
		return nil, err
	}
	if len(l.Items) >= models.MaxLookItems {
		return nil, mapLookErr(repository.ErrLookFull)
	}
	p, err := s.publishedProduct(ctx, sellerID, productID)
	if err != nil {
		return nil, err
	}
	if err := s.looks.AddItem(ctx, sellerID, lookID, p.ID, models.MaxLookItems, s.now().UTC()); err != nil {
		return nil, mapLookErr(err)
	}
	return s.Get(ctx, sellerID, lookID)
}

// CreateAndAttach makes a brand-new product through the normal product flow
// (so it is a real, published product in the store and on the marketplace)
// and connects it to the look.
func (s *LookService) CreateAndAttach(ctx context.Context, sellerID, lookID string, in ProductInput) (*LookView, error) {
	l, err := s.loadForEdit(ctx, sellerID, lookID)
	if err != nil {
		return nil, err
	}
	// Check room BEFORE creating, so a full look never leaves a stray product.
	if len(l.Items) >= models.MaxLookItems {
		return nil, mapLookErr(repository.ErrLookFull)
	}
	in.IsActive = true // a look product is always a normal, published product
	p, err := s.creator.Create(ctx, sellerID, in)
	if err != nil {
		return nil, err
	}
	if err := s.looks.AddItem(ctx, sellerID, lookID, p.ID, models.MaxLookItems, s.now().UTC()); err != nil {
		if errors.Is(err, repository.ErrLookFull) || errors.Is(err, repository.ErrLookItemExists) || errors.Is(err, repository.ErrNotFound) {
			return nil, apperr.New(http.StatusConflict, "LOOK_ATTACH_FAILED",
				"The product was created and is live in your store, but it could not be added to the look. "+
					"Add it with Choose existing product.")
		}
		return nil, err
	}
	return s.Get(ctx, sellerID, lookID)
}

func (s *LookService) RemoveItem(ctx context.Context, sellerID, lookID, productID string) (*LookView, error) {
	if _, err := s.loadForEdit(ctx, sellerID, lookID); err != nil {
		return nil, err
	}
	if err := s.looks.RemoveItem(ctx, sellerID, lookID, strings.TrimSpace(productID), s.now().UTC()); err != nil {
		return nil, mapLookErr(err)
	}
	return s.Get(ctx, sellerID, lookID)
}

// ReplaceItem swaps one product for another, keeping the position.
func (s *LookService) ReplaceItem(ctx context.Context, sellerID, lookID, oldProductID, newProductID string) (*LookView, error) {
	if _, err := s.loadForEdit(ctx, sellerID, lookID); err != nil {
		return nil, err
	}
	p, err := s.publishedProduct(ctx, sellerID, newProductID)
	if err != nil {
		return nil, err
	}
	if err := s.looks.ReplaceItem(ctx, sellerID, lookID, strings.TrimSpace(oldProductID), p.ID, s.now().UTC()); err != nil {
		return nil, mapLookErr(err)
	}
	return s.Get(ctx, sellerID, lookID)
}

// Reorder sets the display order. The list must contain exactly the products
// that are in the look, each once.
func (s *LookService) Reorder(ctx context.Context, sellerID, lookID string, productIDs []string) (*LookView, error) {
	l, err := s.loadForEdit(ctx, sellerID, lookID)
	if err != nil {
		return nil, err
	}
	if len(productIDs) != len(l.Items) {
		return nil, mapLookErr(repository.ErrLookItemsChanged)
	}
	seen := make(map[string]bool, len(productIDs))
	clean := make([]string, 0, len(productIDs))
	for _, id := range productIDs {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] || !l.HasProduct(id) {
			return nil, apperr.Validation(map[string]string{"productIds": "must list each product in the look exactly once"})
		}
		seen[id] = true
		clean = append(clean, id)
	}
	if len(clean) > 0 {
		if err := s.looks.SetOrder(ctx, sellerID, lookID, clean, s.now().UTC()); err != nil {
			return nil, mapLookErr(err)
		}
	}
	return s.Get(ctx, sellerID, lookID)
}

/* ------------------------------------------------------------------ */
/* Publish                                                             */
/* ------------------------------------------------------------------ */

// Publish makes the look visible to buyers. It needs access, a model image and
// at least one product that can currently be shown.
func (s *LookService) Publish(ctx context.Context, sellerID, id string) (*LookView, error) {
	l, err := s.loadForEdit(ctx, sellerID, id)
	if err != nil {
		return nil, err
	}
	fields := map[string]string{}
	if strings.TrimSpace(l.ModelImageURL) == "" {
		fields["modelImageUrl"] = "upload the main model image first"
	}
	items, err := s.resolve(ctx, l)
	if err != nil {
		return nil, err
	}
	shown := 0
	for _, it := range items {
		if it.Status == LookItemAvailable || it.Status == LookItemOutOfStock {
			shown++
		}
	}
	if shown == 0 {
		fields["items"] = "add at least one published product"
	}
	if len(fields) > 0 {
		return nil, apperr.Validation(fields)
	}
	if err := s.looks.SetStatus(ctx, sellerID, id, models.LookPublished, s.now().UTC()); err != nil {
		return nil, mapLookErr(err)
	}
	return s.Get(ctx, sellerID, id)
}

// Unpublish hides the look again. It needs no access: a seller can always take
// their own look down.
func (s *LookService) Unpublish(ctx context.Context, sellerID, id string) (*LookView, error) {
	if err := s.looks.SetStatus(ctx, sellerID, id, models.LookDraft, s.now().UTC()); err != nil {
		return nil, mapLookErr(err)
	}
	return s.Get(ctx, sellerID, id)
}

/* ------------------------------------------------------------------ */
/* Public                                                              */
/* ------------------------------------------------------------------ */

// Public returns a published look for buyers. It answers "not found" when the
// look is a draft, the seller no longer has access, the seller's store is
// hidden (suspended or banned), or nothing in it can be shown. Prices and
// stock are read live, so they follow the products.
func (s *LookService) Public(ctx context.Context, id string) (*PublicLookView, error) {
	l, err := s.looks.FindPublishedByID(ctx, strings.TrimSpace(id))
	if err != nil {
		return nil, mapLookErr(err)
	}
	ok, err := s.gate.HasAccess(ctx, l.SellerID, models.FeatureTryTheLook)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, apperr.NotFound()
	}
	if s.sellerStatus != nil {
		st, err := s.sellerStatus(ctx, l.SellerID)
		if err != nil {
			if errors.Is(err, repository.ErrNotFound) {
				return nil, apperr.NotFound()
			}
			return nil, err
		}
		if !st.StoreVisible() {
			return nil, apperr.NotFound()
		}
	}
	slug := ""
	if s.storeSlug != nil {
		slug = s.storeSlug(ctx, l.SellerID)
	}
	if slug == "" {
		return nil, apperr.NotFound()
	}
	all, err := s.resolve(ctx, l)
	if err != nil {
		return nil, err
	}
	shown := make([]LookItemView, 0, len(all))
	for _, it := range all {
		if it.Status == LookItemAvailable || it.Status == LookItemOutOfStock {
			shown = append(shown, it)
		}
	}
	if len(shown) == 0 {
		return nil, apperr.NotFound()
	}
	return &PublicLookView{
		ID: l.ID, Title: l.Title, Gender: l.Gender, ModelImageURL: l.ModelImageURL,
		StoreSlug: slug, Items: shown,
	}, nil
}
