package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"shopbd/backend/internal/models"
)

var (
	ErrNotFound       = errors.New("repository: not found")
	ErrDuplicateEmail = errors.New("repository: duplicate email")
	// ErrProviderLinked means LinkProvider refused: this user already has a
	// different Google / Apple account linked, or this Google / Apple account
	// already belongs to another user. Nothing was changed.
	ErrProviderLinked = errors.New("repository: provider account already linked")
	ErrSlugTaken      = errors.New("repository: slug taken")
	// ErrIdentityConflict means the shop name/URL change no longer applies
	// (someone changed it concurrently, or the rules now forbid it).
	ErrIdentityConflict     = errors.New("repository: shop identity changed concurrently")
	ErrProductSlug          = errors.New("repository: product slug taken")
	ErrResetNotFound        = errors.New("repository: reset not found")
	ErrVerificationNotFound = errors.New("repository: email verification not found")
	// ErrDuplicateOrder means an order with the same ID or the same
	// (seller, number) already exists. Nothing was stored and no stock was
	// reserved, so the caller can safely retry with a fresh ID and number.
	ErrDuplicateOrder = errors.New("repository: duplicate order id or number")
	// ErrOrderChanged means the order was modified by someone else between
	// reading it and saving the change. Nothing was written; retry.
	ErrOrderChanged = errors.New("repository: order changed concurrently")
)

// InsufficientStockError is returned when an order (or re-opening a cancelled
// order) asks for more units than a product or variant has left. Nothing has
// been changed when it is returned.
type InsufficientStockError struct {
	ProductID string
	VariantID string
	Name      string
	Available int
}

func (e *InsufficientStockError) Error() string {
	return fmt.Sprintf("repository: insufficient stock for %s (%d available)", e.ProductID, e.Available)
}

type UserRepository interface {
	Create(ctx context.Context, u *models.User) error
	FindByEmail(ctx context.Context, email string) (*models.User, error)
	FindByID(ctx context.Context, id string) (*models.User, error)
	UpdatePassword(ctx context.Context, userID, passwordHash string) error
	// Admin listing / moderation
	ListByRole(ctx context.Context, filter UserListFilter) ([]*models.User, int64, error)
	UpdateStatus(ctx context.Context, userID string, status models.Status) error
	CountByRole(ctx context.Context, role models.Role) (int64, error)
	CountByRoleAndStatus(ctx context.Context, role models.Role, status models.Status) (int64, error)
	UpdateProfile(ctx context.Context, userID string, name, email string) error
	UpdateRole(ctx context.Context, userID string, role models.Role) error
	// Social sign-in (Google / Apple)
	FindByProvider(ctx context.Context, provider, subject string) (*models.User, error)
	// LinkProvider attaches the provider account. It never touches the password
	// hash, role or status. It is idempotent for the same subject and returns
	// ErrProviderLinked rather than replacing a different linked account.
	LinkProvider(ctx context.Context, userID, provider, subject string) error
}

// UserListFilter is used by admin seller/customer lists.
type UserListFilter struct {
	Role       models.Role
	Status     *models.Status
	Search     string // name or email substring
	Limit      int
	Skip       int
	SortNewest bool
}

type TokenRepository interface {
	Create(ctx context.Context, t *models.RefreshToken) error
	FindByHash(ctx context.Context, hash string) (*models.RefreshToken, error)
	Revoke(ctx context.Context, id, replacedBy string, at time.Time) (bool, error)
	RevokeFamily(ctx context.Context, familyID string, at time.Time) error
	// RevokeAllForUser revokes every active refresh token for a user
	// (password change, account compromise, admin force-logout).
	RevokeAllForUser(ctx context.Context, userID string, at time.Time) error
}

// UpsertDraftParams carries everything needed to save a draft, including an
// optional slug change (old slug retired, cooldown timestamp updated).
type UpsertDraftParams struct {
	SellerID string
	Slug     string
	Draft    any
	// StoreName is the (already validated) shop name to store alongside the
	// draft. It is only written on first creation; later renames go through
	// ApplyIdentityChange.
	StoreName string
	// SlugChanged is true when the seller is switching to a different slug.
	// When set, OldSlug is appended to previousSlugs and slugChangedAt is set
	// to ChangedAt.
	SlugChanged bool
	OldSlug     string
	ChangedAt   time.Time
}

// IdentityChangeParams describes one atomic change of the shop name and/or
// the shop URL. The old URL is retired permanently; the old shop name is not
// kept anywhere.
type IdentityChangeParams struct {
	SellerID string
	OldSlug  string
	NewSlug  string // equal to OldSlug when the URL does not change
	NewName  string // "" when only the URL changes
	// CountName marks this change as a shop-name change: it consumes one of
	// the MaxNameChanges allowed renames.
	CountName      bool
	MaxNameChanges int
	// LockedBefore: the change is only applied when the last change was at or
	// before this instant (or there was none). Enforces the 30-day lock.
	LockedBefore time.Time
	ChangedAt    time.Time
	// Published is true when the store is live, so the public payload is
	// rewritten too.
	Published bool
	// Source says why the address moved (models.SlugChangeAddress or
	// models.SlugChangeRename). It is stored in the slug history.
	Source string
}

type StoreDesignRepository interface {
	GetBySeller(ctx context.Context, sellerID string) (*models.StoreDesign, error)
	GetPublishedBySlug(ctx context.Context, slug string) (*models.StoreDesign, error)
	UpsertDraft(ctx context.Context, p UpsertDraftParams) error
	Publish(ctx context.Context, sellerID string, draft any) error
	// ApplyIdentityChange atomically renames the shop and/or moves it to a new
	// URL, rewriting draft, published payload and related references. It
	// returns ErrIdentityConflict when the preconditions no longer hold and
	// ErrSlugTaken when the new URL is already used.
	ApplyIdentityChange(ctx context.Context, p IdentityChangeParams) error
	// SlugTaken reports whether slug is the current or a previously used
	// address of any seller other than exceptSellerID.
	SlugTaken(ctx context.Context, slug, exceptSellerID string) (bool, error)
}

// SlugHistoryFilter selects rows of the store-address history.
type SlugHistoryFilter struct {
	SellerID string // "" = every seller
	Search   string // matches old/new address, seller name or seller email
	Limit    int
	Skip     int
}

// SlugChangeItem is one history row plus the seller details the admin list shows.
type SlugChangeItem struct {
	Change       *models.StoreSlugChange
	SellerName   string
	SellerEmail  string
	SellerStatus models.Status
	CurrentSlug  string // the seller's CURRENT store address
}

// SlugHistoryRepository stores and reads the store-address change history.
type SlugHistoryRepository interface {
	Record(ctx context.Context, c *models.StoreSlugChange) error
	ListBySeller(ctx context.Context, sellerID string, limit int) ([]*models.StoreSlugChange, error)
	List(ctx context.Context, f SlugHistoryFilter) ([]*SlugChangeItem, int64, error)
	// BackfillLegacy rebuilds history rows from each store's previousSlugs
	// list. It is idempotent and returns how many rows were added.
	BackfillLegacy(ctx context.Context) (int, error)
}

type ProductRepository interface {
	Create(ctx context.Context, p *models.Product) error
	FindByID(ctx context.Context, sellerID, id string) (*models.Product, error)
	FindBySlug(ctx context.Context, sellerID, slug string) (*models.Product, error)
	List(ctx context.Context, sellerID string, limit int) ([]*models.Product, error)
	ListPublic(ctx context.Context, sellerID string, limit int) ([]*models.Product, error)
	// ListPublicPage returns one page of active products (newest first) and
	// whether more exist after it. skip >= 0, 1 <= limit <= 200.
	ListPublicPage(ctx context.Context, sellerID string, skip, limit int) (items []*models.Product, hasMore bool, err error)
	Update(ctx context.Context, sellerID, id string, p *models.Product) error
	Delete(ctx context.Context, sellerID, id string) error
}

type CategoryRepository interface {
	Create(ctx context.Context, c *models.Category) error
	List(ctx context.Context, sellerID string) ([]*models.Category, error)
	FindByID(ctx context.Context, sellerID, id string) (*models.Category, error)
	Update(ctx context.Context, sellerID, id string, c *models.Category) error
	Delete(ctx context.Context, sellerID, id string) error
}

type OrderRepository interface {
	// Create inserts the order AND reserves its stock in one atomic step: every
	// line is taken off the product (or variant) only if enough is left, and
	// the order is stored only if every line succeeded. On failure nothing is
	// changed. Returns *InsufficientStockError when a line cannot be covered.
	Create(ctx context.Context, o *models.Order) error
	FindByID(ctx context.Context, sellerID, id string) (*models.Order, error)
	FindByNumber(ctx context.Context, sellerID, number string) (*models.Order, error)
	List(ctx context.Context, sellerID string, filter OrderFilter) ([]*models.Order, error)
	// UpdateStatus changes the status and keeps stock consistent in the same
	// transaction: moving to cancelled releases the reserved stock; moving a
	// cancelled order back to an active status reserves it again (and returns
	// *InsufficientStockError if it is no longer available).
	UpdateStatus(ctx context.Context, sellerID, id string, status models.OrderStatus, note string, at time.Time) error
	MarkPaid(ctx context.Context, sellerID, id string, paid bool) error
	// NextOrderSequence atomically returns the next value (1, 2, 3, ...) of
	// the named counter, creating it on first use. Two callers never get the
	// same value, even across API instances. A value is "used up" even if the
	// order that asked for it later fails, so numbers can have gaps but never
	// repeat.
	NextOrderSequence(ctx context.Context, counter string) (int64, error)
	// ExpireUnpaid cancels pending, unpaid orders whose payment window has
	// passed and releases their stock. It returns the orders it cancelled.
	// Safe to run from several instances at once: each order is released once.
	ExpireUnpaid(ctx context.Context, now time.Time, limit int) ([]*models.Order, error)
	Stats(ctx context.Context, sellerID string, since time.Time) (*OrderStats, error)
}

type OrderFilter struct {
	Status *models.OrderStatus
	Limit  int
	Before time.Time
}

type OrderStats struct {
	OrderCount     int
	PaidOrderCount int
	Revenue        float64
	PendingCount   int
	ConfirmedCount int
	ShippedCount   int
	DeliveredCount int
	CancelledCount int
	AvgOrderValue  float64
	UnitsSold      int
}

type PasswordResetRepository interface {
	Create(ctx context.Context, r *models.PasswordReset) error
	// FindByUserID returns the unexpired challenge, or ErrResetNotFound.
	FindByUserID(ctx context.Context, userID string) (*models.PasswordReset, error)
	// IncrementAttempts atomically adds one guess to the challenge and returns
	// the new count, or ErrResetNotFound if it is gone or expired. Parallel
	// guesses each get a distinct count, so none can slip past the limit.
	IncrementAttempts(ctx context.Context, id string) (int, error)
	// Consume deletes the challenge and reports whether this call was the one
	// that removed it. Exactly one of several parallel callers gets true, which
	// makes a code single-use.
	Consume(ctx context.Context, id string) (bool, error)
	DeleteByUserID(ctx context.Context, userID string) error
}

// EmailVerificationRepository stores pending sign-up codes, one per email.
type EmailVerificationRepository interface {
	// Replace makes v the only pending code for v.Email and resets its attempts.
	Replace(ctx context.Context, v *models.EmailVerification) error
	// FindByEmail returns the unexpired pending code, or ErrVerificationNotFound.
	FindByEmail(ctx context.Context, email string) (*models.EmailVerification, error)
	// IncrementAttempts atomically adds one attempt and returns the new count.
	IncrementAttempts(ctx context.Context, email string) (int, error)
	DeleteByEmail(ctx context.Context, email string) error
}
type AnalyticsRepository interface {
	Insert(ctx context.Context, e *models.AnalyticsEvent) error
	CountByKind(ctx context.Context, sellerID, kind string, since time.Time) (int, error)
	ProductCounts(ctx context.Context, sellerID, kind string, since time.Time, limit int) ([]ProductCount, error)
	TermCounts(ctx context.Context, sellerID string, since time.Time, limit int) ([]TermCount, error)
	SourceCounts(ctx context.Context, sellerID string, since time.Time, limit int) ([]SourceCount, error)
	ProductCountsInRange(ctx context.Context, sellerID, kind string, from, to time.Time) (map[string]int, error)
	// DailyCounts returns per-day event counts for a kind, for [from, to).
	// Keys are "YYYY-MM-DD" (UTC).
	DailyCounts(ctx context.Context, sellerID, kind string, from, to time.Time) (map[string]int, error)
	// SourceBreakdown groups events since `since` by (traffic source, kind):
	// event count, distinct visitors and summed value (purchases).
	SourceBreakdown(ctx context.Context, sellerID string, since time.Time) ([]SourceKindStat, error)
	// UniqueVisitors counts distinct visitor hashes on store-visit events.
	UniqueVisitors(ctx context.Context, sellerID string, since time.Time) (int, error)
}

// SourceKindStat is one (source, kind) cell of the traffic-source breakdown.
type SourceKindStat struct {
	Source   string
	Kind     string
	Count    int
	Visitors int
	Value    float64
}

// DailySourceKindStat is one (day, source, kind) cell, used for the per-platform
// daily charts (Meta, TikTok).
type DailySourceKindStat struct {
	Date   string // "YYYY-MM-DD" (UTC)
	Source string
	Kind   string
	Count  int
	Value  float64 // summed order value (purchase events only)
}

// DailySourceCounter is an OPTIONAL capability of an AnalyticsRepository. It is
// a separate interface on purpose: adding a method to AnalyticsRepository would
// break every fake that implements it. The service type-asserts for it and
// simply leaves the platform charts empty if a repository does not provide it.
type DailySourceCounter interface {
	// DailySourceCounts groups events in [from, to) by (UTC day, source, kind)
	// for the given sources only.
	DailySourceCounts(ctx context.Context, sellerID string, sources []string, from, to time.Time) ([]DailySourceKindStat, error)
}

// TrackingSettingsRepository stores each seller's pixel configuration.
// There is exactly one document per seller (its _id is the seller ID), so one
// seller can never read or overwrite another seller's settings.
type TrackingSettingsRepository interface {
	// Get returns ErrNotFound when the seller has not saved settings yet.
	Get(ctx context.Context, sellerID string) (*models.TrackingSettings, error)
	Upsert(ctx context.Context, s *models.TrackingSettings) error
}

// DeliverySettingsRepository stores each seller's delivery charges. There is
// exactly one document per seller (its _id is the seller ID), so one seller
// can never read or overwrite another seller's charges.
type DeliverySettingsRepository interface {
	// Get returns ErrNotFound when the seller has not saved settings yet.
	Get(ctx context.Context, sellerID string) (*models.DeliverySettings, error)
	// Upsert replaces the seller's whole settings document.
	Upsert(ctx context.Context, s *models.DeliverySettings) error
}

type ProductCount struct {
	ProductID string
	Count     int
}

type TermCount struct {
	Term  string
	Count int
}

type SourceCount struct {
	Source string
	Count  int
}

// ---- Admin / moderation repositories ----

type AdminActivityLogRepository interface {
	Create(ctx context.Context, log *models.AdminActivityLog) error
	List(ctx context.Context, filter AdminLogFilter) ([]*models.AdminActivityLog, int64, error)
}

type AdminLogFilter struct {
	AdminID    string
	TargetType string
	TargetID   string
	Action     string
	Limit      int
	Skip       int
}

type SellerRestrictionRepository interface {
	Create(ctx context.Context, r *models.SellerRestriction) error
	DeactivateActive(ctx context.Context, sellerID string) error
	ListBySeller(ctx context.Context, sellerID string, limit int) ([]*models.SellerRestriction, error)
	ActiveForSeller(ctx context.Context, sellerID string) (*models.SellerRestriction, error)
	ExpireDue(ctx context.Context, now time.Time) (int64, error)
}

type AdminNoteRepository interface {
	Create(ctx context.Context, n *models.AdminNote) error
	List(ctx context.Context, targetType, targetID string, limit int) ([]*models.AdminNote, error)
	Update(ctx context.Context, id, body string) error
	Delete(ctx context.Context, id string) error
}

type SellerVerificationRepository interface {
	Create(ctx context.Context, v *models.SellerVerification) error
	FindByID(ctx context.Context, id string) (*models.SellerVerification, error)
	LatestBySeller(ctx context.Context, sellerID string) (*models.SellerVerification, error)
	List(ctx context.Context, status string, limit, skip int) ([]*models.SellerVerification, int64, error)
	UpdateStatus(ctx context.Context, id, status, reviewedBy, reason string, at time.Time) error
	CountByStatus(ctx context.Context, status string) (int64, error)
}

type SubscriptionOverrideRepository interface {
	Create(ctx context.Context, o *models.SubscriptionOverride) error
	ActiveForSeller(ctx context.Context, sellerID string, now time.Time) (*models.SubscriptionOverride, error)
	DeactivateActive(ctx context.Context, sellerID string) error
	ListBySeller(ctx context.Context, sellerID string, limit int) ([]*models.SubscriptionOverride, error)
	CountActivePromotional(ctx context.Context, now time.Time) (int64, error)
	ExpireDue(ctx context.Context, now time.Time) (int64, error)
}

type SellerReportRepository interface {
	Create(ctx context.Context, r *models.SellerReport) error
	FindByID(ctx context.Context, id string) (*models.SellerReport, error)
	List(ctx context.Context, filter ReportListFilter) ([]*models.SellerReport, int64, error)
	Update(ctx context.Context, r *models.SellerReport) error
	CountByStatus(ctx context.Context, status string) (int64, error)
	CountOpen(ctx context.Context) (int64, error)
}

type ReportListFilter struct {
	Status   string
	SellerID string
	Limit    int
	Skip     int
}

type SellerPromotionRepository interface {
	Create(ctx context.Context, p *models.SellerPromotion) error
	DeactivateActive(ctx context.Context, sellerID, kind string) error
	ListBySeller(ctx context.Context, sellerID string, limit int) ([]*models.SellerPromotion, error)
	ActiveFeatured(ctx context.Context, limit int) ([]*models.SellerPromotion, error)
}

type AdminStatsRepository interface {
	DashboardStats(ctx context.Context) (*DashboardStats, error)
	SellerSummary(ctx context.Context, sellerID string) (*SellerSummary, error)
	ListSellersEnriched(ctx context.Context, filter UserListFilter) ([]*SellerListItem, int64, error)
	ProductCountsGlobal(ctx context.Context) (published, inactive int64, err error)
	OrderCountsGlobal(ctx context.Context) (total, delivered int64, err error)
	SubscriptionRevenue(ctx context.Context) (paidTaka int64, activePaid, activeFree int64, err error)
	RecentSellers(ctx context.Context, limit int) ([]*models.User, error)
	RecentReports(ctx context.Context, limit int) ([]*models.SellerReport, error)
	RecentAdminLogs(ctx context.Context, limit int) ([]*models.AdminActivityLog, error)
}

type DashboardStats struct {
	TotalUsers               int64
	TotalSellers             int64
	ActiveSellers            int64
	SuspendedSellers         int64
	RestrictedSellers        int64
	BannedSellers            int64
	TotalCustomers           int64
	PublishedProducts        int64
	InactiveProducts         int64
	TotalOrders              int64
	DeliveredOrders          int64
	SubscriptionRevenueTaka  int64
	ActivePaidSubscriptions  int64
	ActivePromoSubscriptions int64
	PendingVerifications     int64
	OpenReports              int64
}

type SellerSummary struct {
	SellerID           string
	TotalProducts      int64
	PublishedProducts  int64
	TotalOrders        int64
	DeliveredOrders    int64
	CancelledOrders    int64
	StoreSlug          string
	StoreName          string
	Plan               string
	PlanExpiresAt      *time.Time
	IsPromotionalPlan  bool
	VerificationStatus string
	RestrictionKind    string
}

type SellerListItem struct {
	User               *models.User
	StoreSlug          string
	StoreName          string
	TotalProducts      int64
	PublishedProducts  int64
	TotalOrders        int64
	DeliveredOrders    int64
	Plan               string
	PlanExpiresAt      *time.Time
	IsPromotionalPlan  bool
	VerificationStatus string
}
