# ShopBD Super Admin Backend (Phase 1)

## New files
- internal/models/admin.go
- internal/repository/mongorepo/admin.go
- internal/repository/mongorepo/admin_stats.go
- internal/service/admin.go
- internal/handler/admin.go

## Modified
- internal/database/database.go
- internal/repository/repository.go
- internal/repository/mongorepo/mongorepo.go
- internal/repository/memrepo/memrepo.go
- internal/router/router.go
- cmd/api/main.go

## Admin API (all require RoleAdmin + session cookie)

GET  /api/v1/admin/dashboard
GET  /api/v1/admin/sellers?q=&status=&limit=&skip=
GET  /api/v1/admin/sellers/{id}
POST /api/v1/admin/sellers/{id}/suspend
POST /api/v1/admin/sellers/{id}/restrict
POST /api/v1/admin/sellers/{id}/ban
POST /api/v1/admin/sellers/{id}/restore
GET  /api/v1/admin/sellers/{id}/notes
POST /api/v1/admin/sellers/{id}/notes
POST /api/v1/admin/sellers/{id}/subscription/grant
POST /api/v1/admin/sellers/{id}/feature
POST /api/v1/admin/sellers/{id}/unfeature
GET  /api/v1/admin/customers
POST /api/v1/admin/customers/{id}/action
GET  /api/v1/admin/verifications
POST /api/v1/admin/verifications/{id}/review
GET  /api/v1/admin/reports
GET  /api/v1/admin/reports/{id}
PATCH /api/v1/admin/reports/{id}
GET  /api/v1/admin/activity-logs
GET  /api/v1/admin/slug-history?q=&sellerId=&limit=&skip=

`GET /admin/sellers/{id}` also returns `slugHistory` for that seller.

## Public visibility of suspended / banned sellers
A seller whose account status is `suspended` or `banned` is not public:
- `GET /store/{slug}` and everything resolved from a store slug (products,
  categories, tracking, checkout) answer 404.
- The seller, their products and their categories are removed from
  `/marketplace/*` (the marketplace cache is dropped immediately on
  suspend / ban / restore).
- `restricted` keeps the public store. To hide those too, add
  `StatusRestricted` to `models.StoreHiddenStatuses`.

## Store slug history
Every store-address change (address edit or shop rename) is appended to the
`store_slug_history` collection (sellerId, oldSlug, newSlug, changedAt,
source). On startup `BackfillLegacy` rebuilds rows from the old
`previousSlugs` lists (older changes have no recorded date). A seller always
has exactly one `store_designs` document that carries the latest slug; the
admin list/summary read the most recently updated one.

## Create a Super Admin user
Insert into MongoDB users collection with role "ADMIN" and status "active".

## Plans & pricing (admin-editable)
The plan catalog (prices, product limits, feature switches, discounts and extra
feature lines) is no longer fixed in code. The built-in values in
`internal/plans` are the defaults; an admin can change them and the saved copy
is stored in the `plan_catalog` collection (one document, `_id: "catalog"`).
It is loaded on startup and re-read every 30 seconds, so several server
instances stay in step.

GET  /api/v1/plans                (public: pricing page)
GET  /api/v1/admin/plans          (admin: editor data)
PUT  /api/v1/admin/plans          (admin: save; validated, applied at once)
POST /api/v1/admin/plans/reset    (admin: back to the built-in plans)

Editable: per plan name / descriptions / "Popular" badge / monthly price /
product limit / switches (analytics, Meta Pixel, Pyxel, auto courier, premium
design, custom domain, all themes & templates) / extra feature lines; and the
6-month, 12-month and first-purchase discount percentages.
Fixed: the three plan ids (free, pro, business) and the Free price (0).

Rules: a price change only affects purchases made after it is saved (paid
periods are never repriced). Switches and limits apply at once to everyone on
the plan. An "extra feature" is shown on the pricing page and its key is sent
in `entitlements.extras`; it unlocks something only where a screen checks that
key (`extraAllowed(entitlements, "key")` on the frontend).
Every save is written to the activity log (`plans.update` / `plans.reset`).
