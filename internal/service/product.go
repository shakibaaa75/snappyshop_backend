package service

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/repository"
	"shopbd/backend/internal/taxonomy"
)

var productSlugRe = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// safeImageURL accepts only https:// (or http://) addresses and inline
// png/jpeg/webp/gif data URIs. It rejects javascript:, data:text/html, SVG and
// every other scheme, so a seller cannot plant a script or a tracker address.
func safeImageURL(u string) bool {
	u = strings.TrimSpace(u)
	if u == "" {
		return true // empty entries are filtered elsewhere
	}
	l := strings.ToLower(u)
	if strings.HasPrefix(l, "https://") || strings.HasPrefix(l, "http://") {
		return !strings.ContainsAny(u, " \t\r\n<>\"'")
	}
	for _, p := range []string{"data:image/png;base64,", "data:image/jpeg;base64,", "data:image/webp;base64,", "data:image/gif;base64,"} {
		if strings.HasPrefix(l, p) {
			return true
		}
	}
	return false
}

// maxProductCategories is how many storefront categories one product can have.
const maxProductCategories = 5

// PlanLimiter is satisfied by *SubscriptionService.
type PlanLimiter interface {
	CheckProductLimit(ctx context.Context, sellerID string, current int) error
}

// ProductCounter is satisfied by *mongorepo.ProductCounter.
type ProductCounter interface {
	CountBySeller(ctx context.Context, sellerID string) (int, error)
}

type ProductService struct {
	repo    repository.ProductRepository
	limiter PlanLimiter       // optional: nil means no plan limits
	counter ProductCounter    // optional: needed together with limiter
	tax     *taxonomy.Index   // optional: nil means product types are not mapped
	cat     *taxonomy.Catalog // optional: needed to auto-create new categories
	// storeName looks up the seller's shop name; only used when a new
	// category is created. Optional.
	storeName func(ctx context.Context, sellerID string) string
	// imageOK, when set, decides whether a seller may use an image address.
	// Wired when R2 is on: only images this seller uploaded through the
	// validating endpoint are accepted on new or changed images.
	imageOK func(sellerID, url string) bool
}

// SetImagePolicy restricts product image addresses (see imageOK).
func (s *ProductService) SetImagePolicy(f func(sellerID, url string) bool) { s.imageOK = f }

// checkImageOwnership rejects image addresses the seller did not upload.
// Addresses already on the product (keep) stay allowed, so products saved
// before this rule existed can still be edited.
func (s *ProductService) checkImageOwnership(sellerID string, in ProductInput, keep map[string]bool) error {
	if s.imageOK == nil {
		return nil
	}
	bad := func(u string) bool {
		u = strings.TrimSpace(u)
		return u != "" && !keep[u] && !s.imageOK(sellerID, u)
	}
	msg := "Images must be uploaded through the image uploader"
	if bad(in.MainImageURL) {
		return apperr.Validation(map[string]string{"mainImageUrl": msg})
	}
	for _, u := range in.ImageURLs {
		if bad(u) {
			return apperr.Validation(map[string]string{"imageUrls": msg})
		}
	}
	for _, c := range in.Combinations {
		for _, u := range c.ImageURLs {
			if bad(u) {
				return apperr.Validation(map[string]string{"combinations": msg})
			}
		}
	}
	return nil
}

func productImageSet(p *models.Product) map[string]bool {
	m := map[string]bool{p.MainImageURL: true}
	for _, u := range p.ImageURLs {
		m[u] = true
	}
	for _, c := range p.Combinations {
		for _, u := range c.ImageURLs {
			m[u] = true
		}
	}
	return m
}

func NewProductService(repo repository.ProductRepository) *ProductService {
	return &ProductService{repo: repo}
}

// SetPlanLimits turns on the per-plan product limit for Create.
// Existing products are never touched: a seller over the limit keeps what
// they have and simply can't add more until they upgrade.
func (s *ProductService) SetPlanLimits(l PlanLimiter, c ProductCounter) {
	s.limiter = l
	s.counter = c
}

// SetTaxonomy turns on mapping of the seller's product type onto the
// platform-wide category tree (Fashion -> Hoodie ...).
func (s *ProductService) SetTaxonomy(ix *taxonomy.Index) {
	s.tax = ix
}

// SetCatalog turns on the full category flow: strong matches use the official
// name, and a typed category with no close match is created on the spot.
// storeName (optional) records which shop created it.
func (s *ProductService) SetCatalog(cat *taxonomy.Catalog, storeName func(ctx context.Context, sellerID string) string) {
	s.cat = cat
	s.tax = cat.Index
	s.storeName = storeName
}

// applyCategory files the product under a Main Type + category.
// The seller must have chosen a Main Type. A typo like "Headfone" becomes the
// official "Headphones"; text with no close match creates a new category that
// is live immediately (no approval). The product always stores the OFFICIAL
// category name, never the seller's spelling.
func (s *ProductService) applyCategory(ctx context.Context, sellerID string, p *models.Product, in ProductInput) error {
	if s.tax == nil {
		return nil
	}
	input := taxonomy.Input{
		MainID:         strings.TrimSpace(in.MainID),
		CategoryID:     strings.TrimSpace(in.SubCategoryID),
		CustomCategory: strings.TrimSpace(in.CustomSubCategory),
	}
	// No separate category field on the form: the platform category is taken
	// from the seller's primary shop category (the one they named/imaged).
	derived := false
	if input.CategoryID == "" && input.CustomCategory == "" {
		if _, names := categoryLists(in); len(names) > 0 {
			input.CustomCategory = names[0]
			derived = true
		}
	}
	var (
		r   taxonomy.Result
		err error
	)
	if s.cat != nil {
		who := taxonomy.Actor{SellerID: sellerID, ProductID: p.ID, Via: "seller_product_form"}
		if s.storeName != nil {
			who.StoreNameFn = func(c context.Context) string { return s.storeName(c, sellerID) }
		}
		r, err = s.cat.Resolve(ctx, input, who)
	} else {
		// Read-only mode (database unavailable): existing categories only.
		var plan taxonomy.PlanResult
		if plan, err = taxonomy.Plan(s.tax, input); err == nil {
			if plan.Create {
				return apperr.Validation(map[string]string{
					"subCategoryId": "New categories can't be created right now, please pick an existing one",
				})
			}
			r = plan.Result
		}
	}
	if err != nil {
		// A shop category name we can't use as a platform category (too short,
		// digits only, none) must never block saving the product: file it under
		// its Main Type only.
		if (derived || input.CustomCategory == "") &&
			(errors.Is(err, taxonomy.ErrInvalidName) || errors.Is(err, taxonomy.ErrCategoryRequired)) {
			p.MainID = input.MainID
			p.SubID, p.SubName, p.RawSubCategory = "", "", ""
			p.CategoryStatus = "mapped"
			return nil
		}
		return categoryError(err)
	}
	p.MainID = r.MainID
	p.SubID = r.CategoryID
	p.SubName = r.CategoryName
	p.RawSubCategory = r.Raw
	p.CategoryStatus = "mapped"
	return nil
}

// categoryError turns taxonomy errors into messages for the product form.
func categoryError(err error) error {
	msg := func(field, text string) error { return apperr.Validation(map[string]string{field: text}) }
	switch {
	case errors.Is(err, taxonomy.ErrMainRequired), errors.Is(err, taxonomy.ErrUnknownMain):
		return msg("mainId", "Please choose a main type")
	case errors.Is(err, taxonomy.ErrCategoryRequired):
		return msg("subCategoryId", "Choose or enter a category")
	case errors.Is(err, taxonomy.ErrCategoryNotFound):
		return msg("subCategoryId", "That category was just updated, please choose it again")
	case errors.Is(err, taxonomy.ErrCategoryNotInMain):
		return msg("subCategoryId", "That category belongs to a different main type")
	case errors.Is(err, taxonomy.ErrInvalidName):
		return msg("subCategoryId", fmt.Sprintf("Category name must be %d-%d characters and contain letters", taxonomy.MinNameLen, taxonomy.MaxNameLen))
	case errors.Is(err, taxonomy.ErrCreateConflict):
		return msg("subCategoryId", "Could not save the category, please try again")
	}
	return err
}

// applyStorefrontCategories sets the seller's own categories on the product.
// The first one is the primary and also fills the single-category fields, so
// older code that reads CategoryID / CategoryName keeps working.
// validateProduct has already checked there is at least one.
func applyStorefrontCategories(p *models.Product, in ProductInput) {
	ids, names := categoryLists(in)
	p.CategoryIDs = ids
	p.CategoryNames = names
	if len(ids) > 0 {
		p.CategoryID = ids[0]
		p.CategoryName = names[0]
	}
}

// categoryLists returns the de-duplicated category IDs and their names, in the
// order the seller chose. Clients that only send CategoryID still work.
// names always has the same length as ids.
func categoryLists(in ProductInput) (ids, names []string) {
	srcIDs, srcNames := in.CategoryIDs, in.CategoryNames
	if len(srcIDs) == 0 && strings.TrimSpace(in.CategoryID) != "" {
		srcIDs = []string{in.CategoryID}
		srcNames = []string{in.CategoryName}
	}

	ids = make([]string, 0, len(srcIDs))
	names = make([]string, 0, len(srcIDs))
	seen := make(map[string]bool, len(srcIDs))
	for i, raw := range srcIDs {
		id := strings.TrimSpace(raw)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		name := ""
		if i < len(srcNames) {
			name = strings.TrimSpace(srcNames[i])
		}
		ids = append(ids, id)
		names = append(names, name)
	}
	return ids, names
}

// ---------- Input types ----------

type OptionValueInput struct {
	ID       string
	Label    string
	ColorHex string
}

type OptionInput struct {
	ID     string
	Name   string
	Values []OptionValueInput
}

type CombinationInput struct {
	ID        string
	ValueIDs  map[string]string
	Stock     int
	ImageURLs []string
	Price     float64
	SKU       string
}

type AttributeInput struct {
	Key   string
	Value string
}

type ProductInput struct {
	Name         string
	Slug         string
	Description  string
	CategoryID   string
	CategoryName string

	// All storefront categories. The first one is the primary.
	CategoryIDs   []string
	CategoryNames []string

	// Platform category. MainID is one of the 10 fixed Main Types (required).
	// SubCategoryID is a category picked from the list; CustomSubCategory is
	// text the seller typed (matched to an existing category or created).
	MainID            string
	SubCategoryID     string
	CustomSubCategory string

	Price            float64
	ComparePrice     float64
	Stock            int
	SKU              string
	MainImageURL     string
	ImageURLs        []string
	Emoji            string
	Hue              int
	Options          []OptionInput
	Combinations     []CombinationInput
	VariantImageMode string
	Attributes       []AttributeInput
	IsNewArrival     bool
	IsFeatured       bool
	IsBestSeller     bool
	Tags             []string
	IsActive         bool
}

// ---------- Create ----------

func (s *ProductService) Create(ctx context.Context, sellerID string, in ProductInput) (*models.Product, error) {
	if err := validateProduct(in); err != nil {
		return nil, err
	}
	if err := s.checkImageOwnership(sellerID, in, nil); err != nil {
		return nil, err
	}

	// Plan limit: checked after validation so bad input fails first.
	if s.limiter != nil && s.counter != nil {
		n, err := s.counter.CountBySeller(ctx, sellerID)
		if err != nil {
			return nil, err
		}
		if err := s.limiter.CheckProductLimit(ctx, sellerID, n); err != nil {
			return nil, err
		}
	}

	now := time.Now().UTC()

	options := buildOptions(in.Options)
	combos := buildCombinations(in.Combinations, in.Price)
	attrs := buildAttributes(in.Attributes)

	// Total stock: sum of combination stocks when variants exist.
	total := in.Stock
	if len(combos) > 0 {
		total = 0
		for _, c := range combos {
			total += c.Stock
		}
	}

	emoji := strings.TrimSpace(in.Emoji)
	if emoji == "" {
		emoji = "🛍️"
	}
	hue := in.Hue
	if hue == 0 {
		hue = 210
	}
	mode := in.VariantImageMode
	if mode != "per-variant" {
		mode = "shared"
	}

	p := &models.Product{
		ID:               models.NewID(),
		SellerID:         sellerID,
		Name:             strings.TrimSpace(in.Name),
		Slug:             strings.ToLower(strings.TrimSpace(in.Slug)),
		Description:      strings.TrimSpace(in.Description),
		Price:            in.Price,
		ComparePrice:     in.ComparePrice,
		Stock:            total,
		SKU:              strings.TrimSpace(in.SKU),
		MainImageURL:     in.MainImageURL,
		ImageURLs:        nonNil(in.ImageURLs),
		Emoji:            emoji,
		Hue:              hue,
		Options:          options,
		Combinations:     combos,
		VariantImageMode: mode,
		Attributes:       attrs,
		IsNewArrival:     in.IsNewArrival,
		IsFeatured:       in.IsFeatured,
		IsBestSeller:     in.IsBestSeller,
		Tags:             nonNil(in.Tags),
		IsActive:         in.IsActive,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	applyStorefrontCategories(p, in)
	if err := s.applyCategory(ctx, sellerID, p, in); err != nil {
		return nil, err
	}

	if err := s.repo.Create(ctx, p); err != nil {
		if errors.Is(err, repository.ErrProductSlug) {
			return nil, apperr.Conflict(
				apperr.CodeSlugTaken,
				"A product with this URL already exists",
				"slug",
			)
		}
		return nil, err
	}
	return p, nil
}

// ---------- Update ----------

func (s *ProductService) Update(ctx context.Context, sellerID, id string, in ProductInput) (*models.Product, error) {
	if err := validateProduct(in); err != nil {
		return nil, err
	}
	existing, err := s.repo.FindByID(ctx, sellerID, id)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, apperr.NotFound()
		}
		return nil, err
	}
	if err := s.checkImageOwnership(sellerID, in, productImageSet(existing)); err != nil {
		return nil, err
	}

	options := buildOptions(in.Options)
	combos := buildCombinations(in.Combinations, in.Price)
	attrs := buildAttributes(in.Attributes)

	total := in.Stock
	if len(combos) > 0 {
		total = 0
		for _, c := range combos {
			total += c.Stock
		}
	}

	emoji := strings.TrimSpace(in.Emoji)
	if emoji == "" {
		emoji = "🛍️"
	}
	hue := in.Hue
	if hue == 0 {
		hue = 210
	}
	mode := in.VariantImageMode
	if mode != "per-variant" {
		mode = "shared"
	}

	existing.Name = strings.TrimSpace(in.Name)
	existing.Slug = strings.ToLower(strings.TrimSpace(in.Slug))
	existing.Description = strings.TrimSpace(in.Description)
	existing.Price = in.Price
	existing.ComparePrice = in.ComparePrice
	existing.Stock = total
	existing.SKU = strings.TrimSpace(in.SKU)
	existing.MainImageURL = in.MainImageURL
	existing.ImageURLs = nonNil(in.ImageURLs)
	existing.Emoji = emoji
	existing.Hue = hue
	existing.Options = options
	existing.Combinations = combos
	existing.VariantImageMode = mode
	existing.Attributes = attrs
	existing.IsNewArrival = in.IsNewArrival
	existing.IsFeatured = in.IsFeatured
	existing.IsBestSeller = in.IsBestSeller
	existing.Tags = nonNil(in.Tags)
	existing.IsActive = in.IsActive
	applyStorefrontCategories(existing, in)
	if err := s.applyCategory(ctx, sellerID, existing, in); err != nil {
		return nil, err
	}

	if err := s.repo.Update(ctx, sellerID, id, existing); err != nil {
		if errors.Is(err, repository.ErrProductSlug) {
			return nil, apperr.Conflict(
				apperr.CodeSlugTaken,
				"A product with this URL already exists",
				"slug",
			)
		}
		if errors.Is(err, repository.ErrNotFound) {
			return nil, apperr.NotFound()
		}
		return nil, err
	}
	return existing, nil
}

// ---------- Delete / List (unchanged) ----------

func (s *ProductService) Delete(ctx context.Context, sellerID, id string) error {
	if err := s.repo.Delete(ctx, sellerID, id); err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return apperr.NotFound()
		}
		return err
	}
	return nil
}

func (s *ProductService) List(ctx context.Context, sellerID string) ([]*models.Product, error) {
	return s.repo.List(ctx, sellerID, 200)
}

func (s *ProductService) ListPublic(ctx context.Context, sellerID string) ([]*models.Product, error) {
	return s.repo.ListPublic(ctx, sellerID, 200)
}

// ---------- Helpers ----------

func buildOptions(in []OptionInput) []models.ProductOption {
	out := make([]models.ProductOption, 0, len(in))
	for _, o := range in {
		values := make([]models.ProductOptionValue, 0, len(o.Values))
		for _, v := range o.Values {
			if strings.TrimSpace(v.Label) == "" {
				continue
			}
			id := v.ID
			if id == "" {
				id = models.NewID()
			}
			values = append(values, models.ProductOptionValue{
				ID:       id,
				Label:    strings.TrimSpace(v.Label),
				ColorHex: strings.TrimSpace(v.ColorHex),
			})
		}
		if len(values) == 0 {
			continue
		}
		name := strings.TrimSpace(o.Name)
		if name == "" {
			continue
		}
		id := o.ID
		if id == "" {
			id = models.NewID()
		}
		out = append(out, models.ProductOption{
			ID:     id,
			Name:   name,
			Values: values,
		})
	}
	return out
}

// buildCombinations turns the seller's variants into stored ones. A variant
// saved without a price of its own gets the product's price written into it
// here, so every stored variant carries an explicit price and checkout never
// has to guess one.
func buildCombinations(in []CombinationInput, basePrice float64) []models.ProductCombination {
	out := make([]models.ProductCombination, 0, len(in))
	for _, c := range in {
		price := c.Price
		if price <= 0 {
			price = basePrice
		}
		id := c.ID
		if id == "" {
			id = models.NewID()
		}
		// Filter empty image URLs.
		imgs := make([]string, 0, len(c.ImageURLs))
		for _, u := range c.ImageURLs {
			if strings.TrimSpace(u) != "" {
				imgs = append(imgs, u)
			}
		}
		valueIDs := c.ValueIDs
		if valueIDs == nil {
			valueIDs = map[string]string{}
		}
		out = append(out, models.ProductCombination{
			ID:        id,
			ValueIDs:  valueIDs,
			Stock:     c.Stock,
			ImageURLs: imgs,
			Price:     price,
			SKU:       strings.TrimSpace(c.SKU),
		})
	}
	return out
}

func buildAttributes(in []AttributeInput) []models.ProductAttribute {
	out := make([]models.ProductAttribute, 0, len(in))
	for _, a := range in {
		k := strings.TrimSpace(a.Key)
		v := strings.TrimSpace(a.Value)
		if k == "" || v == "" {
			continue
		}
		out = append(out, models.ProductAttribute{Key: k, Value: v})
	}
	return out
}

func nonNil(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

// ---------- Validation ----------

func validateProduct(in ProductInput) error {
	fields := map[string]string{}

	name := strings.TrimSpace(in.Name)
	switch {
	case len(name) < 2:
		fields["name"] = "Product name is required"
	case len(name) > 120:
		fields["name"] = "Product name must be at most 120 characters"
	}

	slug := strings.ToLower(strings.TrimSpace(in.Slug))
	switch {
	case len(slug) < 2:
		fields["slug"] = "URL slug is required"
	case len(slug) > 80:
		fields["slug"] = "URL slug must be at most 80 characters"
	case !productSlugRe.MatchString(slug):
		fields["slug"] = "Use only lowercase letters, numbers and hyphens"
	}

	// Storefront categories: at least one, at most maxProductCategories.
	catIDs, _ := categoryLists(in)
	switch {
	case len(catIDs) == 0:
		fields["categoryId"] = "Please choose a category"
	case len(catIDs) > maxProductCategories:
		fields["categoryId"] = fmt.Sprintf("A product can have at most %d categories", maxProductCategories)
	}

	// Product type: pick one from the list or type your own.
	customType := strings.TrimSpace(in.CustomSubCategory)
	switch {
	case len([]rune(customType)) > 60:
		fields["subCategoryId"] = "Category name must be at most 60 characters"
	}

	if in.Price <= 0 {
		fields["price"] = "Price must be greater than 0"
	}
	if in.Price > 10_000_000 {
		fields["price"] = "Price looks too high"
	}
	if in.ComparePrice > 0 && in.ComparePrice <= in.Price {
		fields["comparePrice"] = "Compare price must be higher than the sale price"
	}
	if in.Stock < 0 {
		fields["stock"] = "Stock can't be negative"
	}
	if len(in.Description) > 2000 {
		fields["description"] = "Description is too long (max 2000 characters)"
	}

	// Images
	if len(in.MainImageURL) > 1_500_000 {
		fields["mainImageUrl"] = "Main image is too large"
	} else if !safeImageURL(in.MainImageURL) {
		fields["mainImageUrl"] = "Main image must be an https link or an uploaded image"
	}
	for i, u := range in.ImageURLs {
		if len(u) > 1_500_000 {
			fields["imageUrls"] = fmt.Sprintf("Gallery image %d is too large", i+1)
			break
		}
		if !safeImageURL(u) {
			fields["imageUrls"] = fmt.Sprintf("Gallery image %d must be an https link or an uploaded image", i+1)
			break
		}
	}
	if len(in.ImageURLs) > 6 {
		fields["imageUrls"] = "At most 6 gallery images"
	}

	// Options
	if len(in.Options) > 5 {
		fields["options"] = "At most 5 options per product"
	}
	for _, o := range in.Options {
		if len(o.Values) > 40 {
			fields["options"] = "At most 40 values per option"
			break
		}
	}

	// Combinations
	if len(in.Combinations) > 200 {
		fields["combinations"] = "Too many combinations"
	}
	for _, c := range in.Combinations {
		if c.Stock < 0 {
			fields["combinations"] = "Variant stock can't be negative"
			break
		}
		for _, u := range c.ImageURLs {
			if len(u) > 1_500_000 {
				fields["combinations"] = "A variant image is too large"
				break
			}
			if !safeImageURL(u) {
				fields["combinations"] = "A variant image must be an https link or an uploaded image"
				break
			}
		}
	}

	// Variants must be well formed, or checkout cannot sell them.
	if _, taken := fields["combinations"]; !taken {
		if msg := validateCombinations(in); msg != "" {
			fields["combinations"] = msg
		}
	}

	// Attributes
	if len(in.Attributes) > 20 {
		fields["attributes"] = "At most 20 attributes"
	}

	if len(fields) > 0 {
		return apperr.Validation(fields)
	}
	return nil
}
