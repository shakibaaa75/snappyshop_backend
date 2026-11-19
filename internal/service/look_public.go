package service

import (
	"context"
	"strings"

	"shopbd/backend/internal/models"
)

const (
	publicLooksDefault = 12
	publicLooksMax     = 60
)

// PublicLookCard is one look in a public list (marketplace home, store home,
// all-looks page). Full items come from the single-look endpoint.
type PublicLookCard struct {
	ID            string   `json:"id"`
	Title         string   `json:"title"`
	Gender        string   `json:"gender,omitempty"`
	ModelImageURL string   `json:"modelImageUrl"`
	StoreSlug     string   `json:"storeSlug"`
	ItemCount     int      `json:"itemCount"`
	Thumbs        []string `json:"thumbs"` // up to 3 product images
}

// SetSlugResolver lets the public list filter by store address.
func (s *LookService) SetSlugResolver(f func(ctx context.Context, slug string) (string, error)) {
	s.sellerBySlug = f
}

type publishedLister interface {
	ListPublished(ctx context.Context, sellerID, gender string, limit int) ([]*models.Look, error)
}

// visibleSeller applies the same rules as Public: the seller must still have
// Try the Look, a visible (not suspended or banned) store and a store address.
func (s *LookService) visibleSeller(ctx context.Context, sellerID string) (string, bool, error) {
	ok, err := s.gate.HasAccess(ctx, sellerID, models.FeatureTryTheLook)
	if err != nil || !ok {
		return "", false, err
	}
	if s.sellerStatus != nil {
		st, err := s.sellerStatus(ctx, sellerID)
		if err != nil {
			return "", false, nil // missing seller: just hide their looks
		}
		if !st.StoreVisible() {
			return "", false, nil
		}
	}
	if s.storeSlug == nil {
		return "", false, nil
	}
	slug := s.storeSlug(ctx, sellerID)
	return slug, slug != "", nil
}

// PublicList returns published looks that buyers may see, newest first.
// storeSlug and gender are optional filters. A look is left out when its
// seller lost access or is hidden, or when none of its products can be shown.
// An unknown store gives an empty list, never an error.
func (s *LookService) PublicList(ctx context.Context, storeSlug, gender string, limit int) ([]PublicLookCard, error) {
	out := []PublicLookCard{}
	lister, ok := s.looks.(publishedLister)
	if !ok {
		return out, nil
	}
	if limit <= 0 {
		limit = publicLooksDefault
	}
	if limit > publicLooksMax {
		limit = publicLooksMax
	}
	gender = strings.ToLower(strings.TrimSpace(gender))
	if gender != models.LookGenderMale && gender != models.LookGenderFemale {
		gender = ""
	}
	sellerID := ""
	if storeSlug = strings.TrimSpace(storeSlug); storeSlug != "" {
		if s.sellerBySlug == nil {
			return out, nil
		}
		id, err := s.sellerBySlug(ctx, storeSlug)
		if err != nil || id == "" {
			return out, nil
		}
		sellerID = id
	}

	// Read extra rows: some will be hidden by the checks below.
	rows, err := lister.ListPublished(ctx, sellerID, gender, limit*3)
	if err != nil {
		return nil, err
	}
	type seen struct {
		slug string
		ok   bool
	}
	cache := map[string]seen{}
	for _, l := range rows {
		if len(out) >= limit {
			break
		}
		v, done := cache[l.SellerID]
		if !done {
			slug, ok, err := s.visibleSeller(ctx, l.SellerID)
			if err != nil {
				return nil, err
			}
			v = seen{slug, ok}
			cache[l.SellerID] = v
		}
		if !v.ok {
			continue
		}
		items, err := s.resolve(ctx, l)
		if err != nil {
			return nil, err
		}
		card := PublicLookCard{
			ID: l.ID, Title: l.Title, Gender: l.Gender,
			ModelImageURL: l.ModelImageURL, StoreSlug: v.slug, Thumbs: []string{},
		}
		for _, it := range items {
			if it.Status != LookItemAvailable && it.Status != LookItemOutOfStock {
				continue
			}
			card.ItemCount++
			if len(card.Thumbs) < 3 && it.ImageURL != "" {
				card.Thumbs = append(card.Thumbs, it.ImageURL)
			}
		}
		if card.ItemCount == 0 {
			continue
		}
		out = append(out, card)
	}
	return out, nil
}
