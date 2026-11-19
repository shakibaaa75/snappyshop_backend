package router

import (
	"context"
	"log/slog"
	"time"

	"shopbd/backend/internal/handler"
	"shopbd/backend/internal/repository/mongorepo"
	"shopbd/backend/internal/taxonomy"
)

// loadTaxonomy builds the platform category tree from MongoDB (seeding it on
// first run) and shares the one *Index with the product service, so every
// part of the app sees the same, current categories.
//
// It returns the Catalog too: the admin API (Part 3) calls Catalog.Reload
// after every rename / merge / move / delete / auto-create. If MongoDB is
// unavailable it falls back to the built-in seed and returns a nil Catalog, so
// the site still works read-only.
func loadTaxonomy(d Deps, log *slog.Logger) (*taxonomy.Index, *taxonomy.Catalog) {
	fallback := func() (*taxonomy.Index, *taxonomy.Catalog) {
		ix := taxonomy.NewIndex(taxonomy.Seed(), nil)
		if d.Products != nil {
			d.Products.SetTaxonomy(ix)
		}
		return ix, nil
	}
	if d.Mongo == nil {
		return fallback()
	}
	store := mongorepo.NewTaxonomyNodes(d.Mongo)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if err := store.EnsureIndexes(ctx); err != nil {
		log.Warn("taxonomy: could not create indexes", "err", err)
	}
	cat, err := taxonomy.NewCatalog(ctx, store)
	if err != nil {
		log.Error("taxonomy: load failed, using built-in seed", "err", err)
		return fallback()
	}
	if d.Products != nil {
		d.Products.SetCatalog(cat, mongorepo.NewStoreNames(d.Mongo).NameFor)
	}
	return cat.Index, cat
}

// newAdminCategories builds the Category Management API. nil when MongoDB or
// the catalog is unavailable (the routes are then simply not registered).
func newAdminCategories(d Deps, cat *taxonomy.Catalog, log *slog.Logger) *handler.AdminCategoryHandler {
	if cat == nil || d.Mongo == nil {
		return nil
	}
	return &handler.AdminCategoryHandler{
		Svc: &taxonomy.Admin{Cat: cat, Products: mongorepo.NewTaxonomyProducts(d.Mongo)},
		Log: log,
	}
}
