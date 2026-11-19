package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/repository"
	"shopbd/backend/internal/tracking"
)

// TrackingSettings is the pixel configuration as the API sends and receives it.
type TrackingSettings struct {
	MetaPixelID    string `json:"metaPixelId"`
	TikTokPixelID  string `json:"tiktokPixelId"`
	RequireConsent bool   `json:"requireConsent"`
}

// TrackingService manages each seller's own Meta / TikTok pixel settings.
// Every method is keyed by seller ID (or by a published store slug that
// resolves to one seller), so settings never cross between stores.
type TrackingService struct {
	repo    repository.TrackingSettingsRepository
	designs repository.StoreDesignRepository

	mu   sync.RWMutex
	gate TrackingGate // optional: nil means no plan checks
}

// TrackingGate is satisfied by *SubscriptionService.
type TrackingGate interface {
	RequireMetaPixel(ctx context.Context, sellerID string) error
	RequirePyxel(ctx context.Context, sellerID string) error
}

// SetGate turns on plan checks for saving pixel settings.
func (s *TrackingService) SetGate(g TrackingGate) {
	s.mu.Lock()
	s.gate = g
	s.mu.Unlock()
}

func NewTrackingService(repo repository.TrackingSettingsRepository, designs repository.StoreDesignRepository) *TrackingService {
	return &TrackingService{repo: repo, designs: designs}
}

// defaultTrackingSettings applies to sellers who have not saved anything:
// no pixels, and consent required (the privacy-safe default).
func defaultTrackingSettings() TrackingSettings {
	return TrackingSettings{RequireConsent: true}
}

func fromModel(m *models.TrackingSettings) TrackingSettings {
	return TrackingSettings{
		MetaPixelID:    m.MetaPixelID,
		TikTokPixelID:  m.TikTokPixelID,
		RequireConsent: m.RequireConsent,
	}
}

// Get returns the seller's settings, or the defaults if none are saved.
func (s *TrackingService) Get(ctx context.Context, sellerID string) (TrackingSettings, error) {
	m, err := s.repo.Get(ctx, sellerID)
	if errors.Is(err, repository.ErrNotFound) {
		return defaultTrackingSettings(), nil
	}
	if err != nil {
		return TrackingSettings{}, err
	}
	return fromModel(m), nil
}

// Save validates and stores the seller's settings. An empty pixel ID switches
// that pixel off.
func (s *TrackingService) Save(ctx context.Context, sellerID string, in TrackingSettings) (TrackingSettings, error) {
	meta := strings.TrimSpace(in.MetaPixelID)
	tt := tracking.NormalizeTikTokPixelID(in.TikTokPixelID)

	fields := map[string]string{}
	if meta != "" && !tracking.ValidMetaPixelID(meta) {
		fields["metaPixelId"] = "A Meta Pixel ID is 8 to 20 digits (numbers only)"
	}
	if tt != "" && !tracking.ValidTikTokPixelID(tt) {
		fields["tiktokPixelId"] = "A TikTok Pixel ID is 10 to 32 letters and numbers"
	}
	if len(fields) > 0 {
		return TrackingSettings{}, apperr.Validation(fields)
	}

	// Plan checks run only when a pixel ID is being set, so a seller on the
	// Free plan can still clear a pixel (or change the consent flag).
	s.mu.RLock()
	gate := s.gate
	s.mu.RUnlock()
	if gate != nil {
		if meta != "" {
			if err := gate.RequireMetaPixel(ctx, sellerID); err != nil {
				return TrackingSettings{}, err
			}
		}
		if tt != "" {
			if err := gate.RequirePyxel(ctx, sellerID); err != nil {
				return TrackingSettings{}, err
			}
		}
	}

	m := &models.TrackingSettings{
		SellerID:       sellerID,
		MetaPixelID:    meta,
		TikTokPixelID:  tt,
		RequireConsent: in.RequireConsent,
		UpdatedAt:      time.Now().UTC(),
	}
	if err := s.repo.Upsert(ctx, m); err != nil {
		return TrackingSettings{}, err
	}
	return fromModel(m), nil
}

// PublicBySlug returns the settings a storefront needs, for a published store.
// It only exposes the pixel IDs (already public in the page) and the consent
// flag. Unknown or unpublished stores are a 404.
func (s *TrackingService) PublicBySlug(ctx context.Context, slug string) (TrackingSettings, error) {
	slug = strings.ToLower(strings.TrimSpace(slug))
	if slug == "" {
		return TrackingSettings{}, apperr.NotFound()
	}
	design, err := s.designs.GetPublishedBySlug(ctx, slug)
	if errors.Is(err, repository.ErrNotFound) {
		return TrackingSettings{}, apperr.NotFound()
	}
	if err != nil {
		return TrackingSettings{}, err
	}
	return s.Get(ctx, design.SellerID)
}
