package handler

import (
	"context"
	"errors"
	"strings"
	"time"

	"shopbd/backend/internal/cache"
	"shopbd/backend/internal/service"
)

// slugsTag is the shared version tag for everything keyed by a store URL: the
// slug -> seller lookup and the published store design. It is bumped when a
// shop name or URL changes, when a store is published, and when an admin
// changes a seller's visibility, so an old URL answers 404 on every API
// instance at once. These events are rare compared to page views.
const slugsTag = "slugs"

// invalidateSlugs drops every cached slug lookup and public store design.
// Failures are logged and retried in the background by the cache.
func invalidateSlugs(c *cache.Cache) {
	if c == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = c.Invalidate(ctx, slugsTag)
}

// cachedSellerID resolves a public store slug to its seller ID through the
// shared cache. It returns "" for an unknown or unpublished slug (caller
// answers 404). A miss is cached for a short time only (NegativeTTL), and
// publishing a store invalidates slugsTag, so a new store never stays 404.
func cachedSellerID(ctx context.Context, c *cache.Cache, svc *service.StoreDesignService, slug string) (string, error) {
	slug = strings.ToLower(strings.TrimSpace(slug))
	load := func(ctx context.Context) (string, error) {
		id, err := svc.SellerIDBySlug(ctx, slug)
		if err != nil {
			return "", err
		}
		if id == "" {
			return "", cache.ErrNotFound
		}
		return id, nil
	}
	if c == nil || slug == "" {
		id, err := load(ctx)
		if errors.Is(err, cache.ErrNotFound) {
			return "", nil
		}
		return id, err
	}
	id, err := cache.Get(ctx, c, "storefront", "slug:"+slug, slugsTag, load)
	switch {
	case err == nil:
		return id, nil
	case errors.Is(err, cache.ErrNotFound):
		return "", nil
	case errors.Is(err, cache.ErrInvalidKey):
		id, err := load(ctx) // unusual slug: skip the cache, never fail
		if errors.Is(err, cache.ErrNotFound) {
			return "", nil
		}
		return id, err
	}
	return "", err
}
