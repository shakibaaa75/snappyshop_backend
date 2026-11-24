package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"shopbd/backend/internal/cache"
	"shopbd/backend/internal/service"
)

// slugsTag is the shared version tag for the slug -> seller lookup ONLY. It is
// bumped when a shop name or URL changes, when a store is published for the
// FIRST time (so a cached "not found" cannot hide it), and when an admin
// changes a seller's visibility, so an old URL answers 404 on every API
// instance at once. These events are rare, so one shared tag is fine here.
//
// The published design itself is NOT under this tag any more. It has its own
// tag per store (storeDesignTag), so one seller publishing no longer clears
// the cache of every other store.
const slugsTag = "slugs"

// storeDesignTag is the cache version tag for one store's published design.
func storeDesignTag(sellerID string) string { return "design:" + sellerID }

// invalidateStoreDesign drops the cached published design of ONE store on
// every API instance. Call it after a publish or a change to what the public
// store page shows (for example the review display settings).
func invalidateStoreDesign(c *cache.Cache, sellerID string) {
	if c == nil || sellerID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = c.Invalidate(ctx, storeDesignTag(sellerID))
}

// invalidateSlugs drops every cached slug lookup. Use it only for rename,
// retire, visibility and first publish; everything else has a per-store tag.
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

// storefrontCacheControl lets Next.js and a CDN reuse a public store response
// briefly, and serve the old copy for a moment while it refreshes. It is only
// set on 200 answers, so a retired or unknown store address (404) is never
// cached. The response is the same for every visitor, so sharing is safe.
const storefrontCacheControl = "public, s-maxage=30, stale-while-revalidate=60"

// writeCachedJSON writes a public JSON answer with Cache-Control and an ETag,
// and answers 304 Not Modified when the caller already has this exact copy.
func writeCachedJSON(w http.ResponseWriter, r *http.Request, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	sum := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(sum[:12]) + `"`
	h := w.Header()
	h.Set("Cache-Control", storefrontCacheControl)
	h.Set("ETag", etag)
	if etagMatches(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	h.Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(append(body, '\n'))
}

// etagMatches reports whether an If-None-Match header names the given ETag
// (or is "*"). Weak validators (W/"...") count as a match.
func etagMatches(header, etag string) bool {
	if header == "" {
		return false
	}
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		part = strings.TrimPrefix(part, "W/")
		if part == "*" || part == etag {
			return true
		}
	}
	return false
}
