// Package handler contains the HTTP handlers.
package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/cache"
	"shopbd/backend/internal/httpx"
	"shopbd/backend/internal/middleware"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/service"
)

type ProductHandler struct {
	Svc         *service.ProductService
	StoreDesign *service.StoreDesignService
	Log         *slog.Logger

	// Cache is the shared read-through cache (Redis). Nil means the public
	// product list always reads MongoDB, exactly as before.
	Cache *cache.Cache
}

// storefrontSellerTag is the cache version tag for everything public that
// belongs to one seller. Bumping it drops that seller's cached product list on
// every API instance at once.
func storefrontSellerTag(sellerID string) string { return "seller:" + sellerID }

// invalidateSellerStorefront drops one seller's cached public data. It is
// called after a product is created, changed or deleted, and after an order
// changed stock. Failures are logged and retried in the background by the
// cache, so callers ignore the result.
func invalidateSellerStorefront(c *cache.Cache, sellerID string) {
	if c == nil || sellerID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = c.Invalidate(ctx, storefrontSellerTag(sellerID))
}

// ---------- DTO types ----------

type OptionValueDTO struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	ColorHex string `json:"colorHex,omitempty"`
}

type OptionDTO struct {
	ID     string           `json:"id"`
	Name   string           `json:"name"`
	Values []OptionValueDTO `json:"values"`
}

type CombinationDTO struct {
	ID        string            `json:"id"`
	ValueIDs  map[string]string `json:"valueIds"`
	Stock     int               `json:"stock"`
	ImageURLs []string          `json:"imageUrls"`
	Price     float64           `json:"price,omitempty"`
	SKU       string            `json:"sku,omitempty"`
}

type AttributeDTO struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type ProductDTO struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Slug         string `json:"slug"`
	Description  string `json:"description"`
	CategoryID   string `json:"categoryId"`
	CategoryName string `json:"categoryName"`

	// All storefront categories. The first one is the primary.
	CategoryIDs   []string `json:"categoryIds"`
	CategoryNames []string `json:"categoryNames"`

	// Platform-wide category (product type).
	MainID            string `json:"mainId"`
	SubID             string `json:"subId"`
	SubName           string `json:"subName"`
	CustomSubCategory string `json:"customSubCategory"`

	Price            float64          `json:"price"`
	ComparePrice     float64          `json:"comparePrice,omitempty"`
	Stock            int              `json:"stock"`
	SKU              string           `json:"sku,omitempty"`
	MainImageURL     string           `json:"mainImageUrl,omitempty"`
	ImageURLs        []string         `json:"imageUrls"`
	Emoji            string           `json:"emoji"`
	Hue              int              `json:"hue"`
	Options          []OptionDTO      `json:"options"`
	Combinations     []CombinationDTO `json:"combinations"`
	VariantImageMode string           `json:"variantImageMode"`
	Attributes       []AttributeDTO   `json:"attributes"`
	IsNewArrival     bool             `json:"isNewArrival"`
	IsFeatured       bool             `json:"isFeatured"`
	IsBestSeller     bool             `json:"isBestSeller"`
	Tags             []string         `json:"tags"`
	IsActive         bool             `json:"isActive"`
	Rating           float64          `json:"rating"`
	Reviews          int              `json:"reviews"`
	Sold             int              `json:"sold"`
	CreatedAt        string           `json:"createdAt"`
	UpdatedAt        string           `json:"updatedAt"`
}

func toProductDTO(p *models.Product) ProductDTO {
	imgs := p.ImageURLs
	if imgs == nil {
		imgs = []string{}
	}
	tags := p.Tags
	if tags == nil {
		tags = []string{}
	}

	opts := make([]OptionDTO, 0, len(p.Options))
	for _, o := range p.Options {
		vals := make([]OptionValueDTO, 0, len(o.Values))
		for _, v := range o.Values {
			vals = append(vals, OptionValueDTO{
				ID:       v.ID,
				Label:    v.Label,
				ColorHex: v.ColorHex,
			})
		}
		opts = append(opts, OptionDTO{ID: o.ID, Name: o.Name, Values: vals})
	}

	combos := make([]CombinationDTO, 0, len(p.Combinations))
	for _, c := range p.Combinations {
		vids := c.ValueIDs
		if vids == nil {
			vids = map[string]string{}
		}
		cimgs := c.ImageURLs
		if cimgs == nil {
			cimgs = []string{}
		}
		combos = append(combos, CombinationDTO{
			ID:        c.ID,
			ValueIDs:  vids,
			Stock:     c.Stock,
			ImageURLs: cimgs,
			Price:     c.Price,
			SKU:       c.SKU,
		})
	}

	attrs := make([]AttributeDTO, 0, len(p.Attributes))
	for _, a := range p.Attributes {
		attrs = append(attrs, AttributeDTO{Key: a.Key, Value: a.Value})
	}

	mode := p.VariantImageMode
	if mode != "per-variant" {
		mode = "shared"
	}

	// If the seller picked from the list there is no typed text to show again.
	customSub := ""
	if p.SubID == "" {
		customSub = p.RawSubCategory
	}

	// Older products only have one category. Treat it as a list of one.
	catIDs := p.CategoryIDs
	catNames := p.CategoryNames
	if len(catIDs) == 0 && p.CategoryID != "" {
		catIDs = []string{p.CategoryID}
		catNames = []string{p.CategoryName}
	}
	if catIDs == nil {
		catIDs = []string{}
	}
	if catNames == nil {
		catNames = []string{}
	}

	return ProductDTO{
		ID:                p.ID,
		Name:              p.Name,
		Slug:              p.Slug,
		Description:       p.Description,
		CategoryID:        p.CategoryID,
		CategoryName:      p.CategoryName,
		CategoryIDs:       catIDs,
		CategoryNames:     catNames,
		MainID:            p.MainID,
		SubID:             p.SubID,
		SubName:           p.SubName,
		CustomSubCategory: customSub,
		Price:             p.Price,
		ComparePrice:      p.ComparePrice,
		Stock:             p.Stock,
		SKU:               p.SKU,
		MainImageURL:      p.MainImageURL,
		ImageURLs:         imgs,
		Emoji:             p.Emoji,
		Hue:               p.Hue,
		Options:           opts,
		Combinations:      combos,
		VariantImageMode:  mode,
		Attributes:        attrs,
		IsNewArrival:      p.IsNewArrival,
		IsFeatured:        p.IsFeatured,
		IsBestSeller:      p.IsBestSeller,
		Tags:              tags,
		IsActive:          p.IsActive,
		Rating:            p.ReviewAverage,
		Reviews:           p.ReviewCount,
		Sold:              0,
		CreatedAt:         p.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:         p.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

// ---------- Request body ----------

type optionValueBody struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	ColorHex string `json:"colorHex"`
}

type optionBody struct {
	ID     string            `json:"id"`
	Name   string            `json:"name"`
	Values []optionValueBody `json:"values"`
}

type combinationBody struct {
	ID        string            `json:"id"`
	ValueIDs  map[string]string `json:"valueIds"`
	Stock     int               `json:"stock"`
	ImageURLs []string          `json:"imageUrls"`
	Price     float64           `json:"price"`
	SKU       string            `json:"sku"`
}

type attributeBody struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type productBody struct {
	Name         string `json:"name"`
	Slug         string `json:"slug"`
	Description  string `json:"description"`
	CategoryID   string `json:"categoryId"`
	CategoryName string `json:"categoryName"`

	// A product can have several storefront categories. The first is the primary.
	CategoryIDs   []string `json:"categoryIds"`
	CategoryNames []string `json:"categoryNames"`

	// Product type: an ID when picked from the list, text when typed.
	MainID            string `json:"mainId"`
	SubCategoryID     string `json:"subCategoryId"`
	CustomSubCategory string `json:"customSubCategory"`

	Price            float64           `json:"price"`
	ComparePrice     float64           `json:"comparePrice"`
	Stock            int               `json:"stock"`
	SKU              string            `json:"sku"`
	MainImageURL     string            `json:"mainImageUrl"`
	ImageURLs        []string          `json:"imageUrls"`
	Emoji            string            `json:"emoji"`
	Hue              int               `json:"hue"`
	Options          []optionBody      `json:"options"`
	Combinations     []combinationBody `json:"combinations"`
	VariantImageMode string            `json:"variantImageMode"`
	Attributes       []attributeBody   `json:"attributes"`
	IsNewArrival     bool              `json:"isNewArrival"`
	IsFeatured       bool              `json:"isFeatured"`
	IsBestSeller     bool              `json:"isBestSeller"`
	Tags             []string          `json:"tags"`
	IsActive         *bool             `json:"isActive"`
}

func (b productBody) toInput() service.ProductInput {
	active := true
	if b.IsActive != nil {
		active = *b.IsActive
	}
	emoji := b.Emoji
	if emoji == "" {
		emoji = "🛍️"
	}
	hue := b.Hue
	if hue == 0 {
		hue = 210
	}

	options := make([]service.OptionInput, 0, len(b.Options))
	for _, o := range b.Options {
		vals := make([]service.OptionValueInput, 0, len(o.Values))
		for _, v := range o.Values {
			vals = append(vals, service.OptionValueInput{
				ID: v.ID, Label: v.Label, ColorHex: v.ColorHex,
			})
		}
		options = append(options, service.OptionInput{
			ID: o.ID, Name: o.Name, Values: vals,
		})
	}

	combos := make([]service.CombinationInput, 0, len(b.Combinations))
	for _, c := range b.Combinations {
		combos = append(combos, service.CombinationInput{
			ID:        c.ID,
			ValueIDs:  c.ValueIDs,
			Stock:     c.Stock,
			ImageURLs: c.ImageURLs,
			Price:     c.Price,
			SKU:       c.SKU,
		})
	}

	attrs := make([]service.AttributeInput, 0, len(b.Attributes))
	for _, a := range b.Attributes {
		attrs = append(attrs, service.AttributeInput{Key: a.Key, Value: a.Value})
	}

	return service.ProductInput{
		Name:              b.Name,
		Slug:              b.Slug,
		Description:       b.Description,
		CategoryID:        b.CategoryID,
		CategoryName:      b.CategoryName,
		CategoryIDs:       b.CategoryIDs,
		CategoryNames:     b.CategoryNames,
		MainID:            b.MainID,
		SubCategoryID:     b.SubCategoryID,
		CustomSubCategory: b.CustomSubCategory,
		Price:             b.Price,
		ComparePrice:      b.ComparePrice,
		Stock:             b.Stock,
		SKU:               b.SKU,
		MainImageURL:      b.MainImageURL,
		ImageURLs:         b.ImageURLs,
		Emoji:             emoji,
		Hue:               hue,
		Options:           options,
		Combinations:      combos,
		VariantImageMode:  b.VariantImageMode,
		Attributes:        attrs,
		IsNewArrival:      b.IsNewArrival,
		IsFeatured:        b.IsFeatured,
		IsBestSeller:      b.IsBestSeller,
		Tags:              b.Tags,
		IsActive:          active,
	}
}

// ---------- HTTP handlers ----------

func (h *ProductHandler) fail(w http.ResponseWriter, r *http.Request, err error) {
	httpx.WriteError(w, r, err, h.Log)
}

// List — GET /api/v1/seller/products
func (h *ProductHandler) List(w http.ResponseWriter, r *http.Request) {
	claims, ok := middleware.ClaimsFrom(r.Context())
	if !ok {
		h.fail(w, r, apperr.Unauthenticated("Authentication required"))
		return
	}
	items, err := h.Svc.List(r.Context(), claims.UserID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := make([]ProductDTO, 0, len(items))
	for _, p := range items {
		out = append(out, toProductDTO(p))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"products": out})
}

// Create — POST /api/v1/seller/products
func (h *ProductHandler) Create(w http.ResponseWriter, r *http.Request) {
	claims, ok := middleware.ClaimsFrom(r.Context())
	if !ok {
		h.fail(w, r, apperr.Unauthenticated("Authentication required"))
		return
	}
	var body productBody
	if err := httpx.DecodeJSON(w, r, &body, httpx.MaxBodyBytes); err != nil {
		h.fail(w, r, err)
		return
	}
	p, err := h.Svc.Create(r.Context(), claims.UserID, body.toInput())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	invalidateSellerStorefront(h.Cache, claims.UserID)
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"product": toProductDTO(p)})
}

// Update — PUT /api/v1/seller/products/{id}
func (h *ProductHandler) Update(w http.ResponseWriter, r *http.Request) {
	claims, ok := middleware.ClaimsFrom(r.Context())
	if !ok {
		h.fail(w, r, apperr.Unauthenticated("Authentication required"))
		return
	}
	id := chi.URLParam(r, "id")
	var body productBody
	if err := httpx.DecodeJSON(w, r, &body, httpx.MaxBodyBytes); err != nil {
		h.fail(w, r, err)
		return
	}
	p, err := h.Svc.Update(r.Context(), claims.UserID, id, body.toInput())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	invalidateSellerStorefront(h.Cache, claims.UserID)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"product": toProductDTO(p)})
}

// Delete — DELETE /api/v1/seller/products/{id}
func (h *ProductHandler) Delete(w http.ResponseWriter, r *http.Request) {
	claims, ok := middleware.ClaimsFrom(r.Context())
	if !ok {
		h.fail(w, r, apperr.Unauthenticated("Authentication required"))
		return
	}
	id := chi.URLParam(r, "id")
	if err := h.Svc.Delete(r.Context(), claims.UserID, id); err != nil {
		h.fail(w, r, err)
		return
	}
	invalidateSellerStorefront(h.Cache, claims.UserID)
	w.WriteHeader(http.StatusNoContent)
}

// PublicList — GET /api/v1/store/{slug}/products (public, no auth)
//
// The slug is resolved through the shared cache (tag "slugs", invalidated on
// every shop name/URL change, publish and visibility change), so a retired or
// unpublished store URL answers 404 at once on every instance. The product list
// of that seller is cached under its own tag. Both are the same for every
// visitor, so sharing them cannot leak per-user data. The
// list is the same for every visitor, so sharing it cannot leak per-user data.
// Checkout never uses it: orders read prices and stock from MongoDB.
func (h *ProductHandler) PublicList(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	if slug == "" {
		h.fail(w, r, apperr.BadRequest("Missing slug"))
		return
	}
	sellerID, err := cachedSellerID(r.Context(), h.Cache, h.StoreDesign, slug)
	if err != nil {
		if errors.Is(err, cache.ErrBusy) {
			w.Header().Set("Retry-After", "2")
			h.fail(w, r, apperr.New(http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE",
				"The store is busy. Please try again shortly."))
			return
		}
		h.fail(w, r, err)
		return
	}
	if sellerID == "" {
		h.fail(w, r, apperr.NotFound())
		return
	}
	out, err := h.publicProducts(r.Context(), sellerID)
	if err != nil {
		if errors.Is(err, cache.ErrBusy) {
			// Too many concurrent loads on this instance: shed load instead
			// of queueing database work.
			w.Header().Set("Retry-After", "2")
			h.fail(w, r, apperr.New(http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE",
				"The store is busy. Please try again shortly."))
			return
		}
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"products": out})
}

// publicProducts returns the public product DTOs of one seller, through the
// shared cache when it is configured.
func (h *ProductHandler) publicProducts(ctx context.Context, sellerID string) ([]ProductDTO, error) {
	load := func(ctx context.Context) ([]ProductDTO, error) {
		items, err := h.Svc.ListPublic(ctx, sellerID)
		if err != nil {
			return nil, err
		}
		out := make([]ProductDTO, 0, len(items))
		for _, p := range items {
			out = append(out, toProductDTO(p))
		}
		return out, nil
	}
	if h.Cache == nil {
		return load(ctx)
	}
	key, kerr := cache.ScopedKey(sellerID, "public-products")
	if kerr != nil {
		return load(ctx) // unusual seller id: skip the cache, never fail
	}
	out, err := cache.Get(ctx, h.Cache, "product", key, storefrontSellerTag(sellerID), load)
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = []ProductDTO{} // keep "products": [] and never null
	}
	return out, nil
}
