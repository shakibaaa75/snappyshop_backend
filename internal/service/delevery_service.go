package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/delivery"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/repository"
)

const (
	// maxDeliveryOverrides caps how many products one seller can give a custom
	// charge. It keeps the document and the request body small.
	maxDeliveryOverrides = 1000
	// maxDeliveryFieldErrors stops a bad request from producing a huge error map.
	maxDeliveryFieldErrors = 20
	// productListPage is how many products ProductRepository.List returns at most.
	productListPage = 500
)

// DeliveryCharges is the charge for each zone, as the API sends and receives it.
type DeliveryCharges struct {
	InsideDhaka  float64 `json:"insideDhaka"`
	OutsideDhaka float64 `json:"outsideDhaka"`
}

// DeliverySettings is a seller's delivery configuration as the API sends and
// receives it.
//
// When saving, only Global and Products are read. FreeDeliveryThreshold and
// Configured are output-only (they are accepted and ignored so a client can
// send back what it fetched).
type DeliverySettings struct {
	// Global applies to every product that has no custom charge.
	Global DeliveryCharges `json:"global"`
	// Products maps a product ID to its custom charges.
	Products map[string]DeliveryCharges `json:"products"`
	// FreeDeliveryThreshold is the store's "free delivery over X" amount from
	// Shop options (0 = off). Read-only here.
	FreeDeliveryThreshold float64 `json:"freeDeliveryThreshold"`
	// Configured is false until the seller saves for the first time. Until
	// then Global shows the charge customers currently pay.
	Configured bool `json:"configured"`
}

// DeliveryPricing is everything needed to price one seller's orders.
type DeliveryPricing struct {
	Settings   *models.DeliverySettings
	Configured bool
	// FreeOver is the "free delivery over X" threshold, or 0.
	FreeOver float64
}

// DeliveryService manages each seller's delivery charges and prices orders
// with them. Every method is keyed by seller ID (or by a published store slug
// that resolves to one seller), so charges never cross between stores.
type DeliveryService struct {
	repo     repository.DeliverySettingsRepository
	products repository.ProductRepository
	designs  repository.StoreDesignRepository
}

func NewDeliveryService(
	repo repository.DeliverySettingsRepository,
	products repository.ProductRepository,
	designs repository.StoreDesignRepository,
) *DeliveryService {
	return &DeliveryService{repo: repo, products: products, designs: designs}
}

// freeThreshold reads the store's "free delivery over X" amount. It is stored
// in the store design (Shop options). Any problem reading it means no
// threshold, exactly as the order service has always behaved.
func (s *DeliveryService) freeThreshold(ctx context.Context, sellerID string) float64 {
	d, err := s.designs.GetBySeller(ctx, sellerID)
	if err != nil || d == nil {
		return 0
	}
	return extractFreeDeliveryThreshold(d.Draft)
}

// Pricing returns what is needed to price this seller's orders. A seller who
// has never saved Delivery Settings gets the legacy rule, so nothing changes
// for their customers until they choose to.
func (s *DeliveryService) Pricing(ctx context.Context, sellerID string) (DeliveryPricing, error) {
	freeOver := s.freeThreshold(ctx, sellerID)
	m, err := s.repo.Get(ctx, sellerID)
	if errors.Is(err, repository.ErrNotFound) {
		return DeliveryPricing{
			Settings:   delivery.Legacy(freeOver),
			Configured: false,
			FreeOver:   freeOver,
		}, nil
	}
	if err != nil {
		return DeliveryPricing{}, err
	}
	return DeliveryPricing{Settings: m, Configured: true, FreeOver: freeOver}, nil
}

// Quote prices the delivery of one order for a seller. district is the
// district the customer chose at checkout. It is the single entry point used
// both to show a price and to charge it when the order is placed.
func (s *DeliveryService) Quote(
	ctx context.Context,
	sellerID, district string,
	productIDs []string,
	subtotal float64,
) (delivery.Breakdown, error) {
	p, err := s.Pricing(ctx, sellerID)
	if err != nil {
		return delivery.Breakdown{}, err
	}
	return delivery.Calculate(p.Settings, delivery.ZoneFor(district), productIDs, subtotal, p.FreeOver), nil
}

func deliveryView(p DeliveryPricing) DeliverySettings {
	out := DeliverySettings{
		Global: DeliveryCharges{
			InsideDhaka:  p.Settings.Global.InsideDhaka,
			OutsideDhaka: p.Settings.Global.OutsideDhaka,
		},
		Products:              make(map[string]DeliveryCharges, len(p.Settings.Products)),
		FreeDeliveryThreshold: p.FreeOver,
		Configured:            p.Configured,
	}
	for id, c := range p.Settings.Products {
		out.Products[id] = DeliveryCharges{InsideDhaka: c.InsideDhaka, OutsideDhaka: c.OutsideDhaka}
	}
	return out
}

// Get returns the seller's settings, or what customers currently pay if the
// seller has not saved any.
func (s *DeliveryService) Get(ctx context.Context, sellerID string) (DeliverySettings, error) {
	p, err := s.Pricing(ctx, sellerID)
	if err != nil {
		return DeliverySettings{}, err
	}
	return deliveryView(p), nil
}

// Save validates and stores the seller's settings.
//
// Custom charges for products that no longer exist, or that belong to another
// seller, are dropped silently: a stale screen must not be able to fail a save
// or write charges for someone else's products.
func (s *DeliveryService) Save(ctx context.Context, sellerID string, in DeliverySettings) (DeliverySettings, error) {
	fields := map[string]string{}
	problem := fmt.Sprintf("Enter an amount from 0 to %.0f", delivery.MaxCharge)
	check := func(prefix string, c DeliveryCharges) {
		if len(fields) >= maxDeliveryFieldErrors {
			return
		}
		if !delivery.ValidCharge(c.InsideDhaka) {
			fields[prefix+"insideDhaka"] = problem
		}
		if !delivery.ValidCharge(c.OutsideDhaka) {
			fields[prefix+"outsideDhaka"] = problem
		}
	}

	check("global.", in.Global)
	if len(in.Products) > maxDeliveryOverrides {
		fields["products"] = fmt.Sprintf("You can set custom charges for up to %d products", maxDeliveryOverrides)
	} else {
		for id, c := range in.Products {
			check("products."+id+".", c)
		}
	}
	if len(fields) > 0 {
		return DeliverySettings{}, apperr.Validation(fields)
	}

	// Tidy the product IDs (trim, drop blanks) before checking ownership.
	wanted := make(map[string]DeliveryCharges, len(in.Products))
	ids := make([]string, 0, len(in.Products))
	for id, c := range in.Products {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, dup := wanted[id]; !dup {
			ids = append(ids, id)
		}
		wanted[id] = c
	}
	owned, err := s.ownedProductIDs(ctx, sellerID, ids)
	if err != nil {
		return DeliverySettings{}, err
	}

	m := &models.DeliverySettings{
		SellerID: sellerID,
		Global: models.DeliveryCharges{
			InsideDhaka:  delivery.Round(in.Global.InsideDhaka),
			OutsideDhaka: delivery.Round(in.Global.OutsideDhaka),
		},
		Products:  make(map[string]models.DeliveryCharges, len(owned)),
		UpdatedAt: time.Now().UTC(),
	}
	for id, c := range wanted {
		if !owned[id] {
			continue
		}
		m.Products[id] = models.DeliveryCharges{
			InsideDhaka:  delivery.Round(c.InsideDhaka),
			OutsideDhaka: delivery.Round(c.OutsideDhaka),
		}
	}

	if err := s.repo.Upsert(ctx, m); err != nil {
		return DeliverySettings{}, err
	}
	return deliveryView(DeliveryPricing{
		Settings:   m,
		Configured: true,
		FreeOver:   s.freeThreshold(ctx, sellerID),
	}), nil
}

// ownedProductIDs returns which of the given product IDs belong to the seller.
func (s *DeliveryService) ownedProductIDs(ctx context.Context, sellerID string, wanted []string) (map[string]bool, error) {
	owned := make(map[string]bool, len(wanted))
	if len(wanted) == 0 {
		return owned, nil
	}
	list, err := s.products.List(ctx, sellerID, productListPage)
	if err != nil {
		return nil, err
	}
	have := make(map[string]struct{}, len(list))
	for _, p := range list {
		have[p.ID] = struct{}{}
	}
	for _, id := range wanted {
		if _, ok := have[id]; ok {
			owned[id] = true
			continue
		}
		// The list was not truncated, so a miss really is "not theirs".
		if len(list) < productListPage {
			continue
		}
		// A very large catalogue: check the product directly.
		_, err := s.products.FindByID(ctx, sellerID, id)
		switch {
		case err == nil:
			owned[id] = true
		case errors.Is(err, repository.ErrNotFound):
			// deleted, or not this seller's: leave it out
		default:
			return nil, err
		}
	}
	return owned, nil
}

// PublicBySlug returns the delivery charges a storefront needs to show prices
// on the product page and in the cart. Charges are public by nature (the
// customer pays them). Unknown or unpublished stores are a 404.
func (s *DeliveryService) PublicBySlug(ctx context.Context, slug string) (DeliverySettings, error) {
	slug = strings.ToLower(strings.TrimSpace(slug))
	if slug == "" {
		return DeliverySettings{}, apperr.NotFound()
	}
	design, err := s.designs.GetPublishedBySlug(ctx, slug)
	if errors.Is(err, repository.ErrNotFound) {
		return DeliverySettings{}, apperr.NotFound()
	}
	if err != nil {
		return DeliverySettings{}, err
	}
	return s.Get(ctx, design.SellerID)
}
