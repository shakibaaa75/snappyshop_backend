package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/plans"
	"shopbd/backend/internal/repository"
)

type AdminService struct {
	users         repository.UserRepository
	tokens        repository.TokenRepository
	logs          repository.AdminActivityLogRepository
	restrictions  repository.SellerRestrictionRepository
	notes         repository.AdminNoteRepository
	verifications repository.SellerVerificationRepository
	overrides     repository.SubscriptionOverrideRepository
	reports       repository.SellerReportRepository
	promotions    repository.SellerPromotionRepository
	slugHistory   repository.SlugHistoryRepository
	stats         repository.AdminStatsRepository
	now           func() time.Time
	subs          *SubscriptionService      // set with SetSubscriptions; drives real plan access
	grantOffers   *SubscriptionGrantService // set with SetGrantOffers; free plans are offered, then claimed

	mu                 sync.RWMutex
	onVisibilityChange func()
}

func NewAdminService(
	users repository.UserRepository,
	tokens repository.TokenRepository,
	logs repository.AdminActivityLogRepository,
	restrictions repository.SellerRestrictionRepository,
	notes repository.AdminNoteRepository,
	verifications repository.SellerVerificationRepository,
	overrides repository.SubscriptionOverrideRepository,
	reports repository.SellerReportRepository,
	promotions repository.SellerPromotionRepository,
	slugHistory repository.SlugHistoryRepository,
	stats repository.AdminStatsRepository,
) *AdminService {
	return &AdminService{
		users: users, tokens: tokens, logs: logs, restrictions: restrictions,
		notes: notes, verifications: verifications, overrides: overrides,
		reports: reports, promotions: promotions, slugHistory: slugHistory,
		stats: stats, now: time.Now,
	}
}

// SetOnVisibilityChange registers a callback that runs after a seller's
// account status changed (suspend, restrict, ban, restore). The router uses it
// to drop the public marketplace cache, so a suspended or banned seller
// disappears from the marketplace at once instead of when the cache expires.
func (s *AdminService) SetOnVisibilityChange(fn func()) {
	s.mu.Lock()
	s.onVisibilityChange = fn
	s.mu.Unlock()
}

func (s *AdminService) visibilityChanged() {
	s.mu.RLock()
	fn := s.onVisibilityChange
	s.mu.RUnlock()
	if fn != nil {
		fn()
	}
}

func (s *AdminService) recordLog(ctx context.Context, adminID, action, targetType, targetID, reason string, prev, next any, ip string) {
	_ = s.logs.Create(ctx, &models.AdminActivityLog{
		ID: models.NewID(), AdminID: adminID, Action: action,
		TargetType: targetType, TargetID: targetID,
		PreviousValue: prev, NewValue: next, Reason: reason, IP: ip,
		CreatedAt: s.now().UTC(),
	})
}

func (s *AdminService) Dashboard(ctx context.Context) (*repository.DashboardStats, error) {
	return s.stats.DashboardStats(ctx)
}

func (s *AdminService) RecentActivity(ctx context.Context) (sellers []*models.User, reports []*models.SellerReport, logs []*models.AdminActivityLog, err error) {
	sellers, err = s.stats.RecentSellers(ctx, 10)
	if err != nil {
		return
	}
	reports, err = s.stats.RecentReports(ctx, 10)
	if err != nil {
		return
	}
	logs, err = s.stats.RecentAdminLogs(ctx, 15)
	return
}

func (s *AdminService) ListSellers(ctx context.Context, filter repository.UserListFilter) ([]*repository.SellerListItem, int64, error) {
	filter.Role = models.RoleSeller
	return s.stats.ListSellersEnriched(ctx, filter)
}

func (s *AdminService) GetSeller(ctx context.Context, sellerID string) (*models.User, *repository.SellerSummary, error) {
	u, err := s.users.FindByID(ctx, sellerID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, nil, apperr.NotFound()
		}
		return nil, nil, err
	}
	if u.Role != models.RoleSeller {
		return nil, nil, apperr.NotFound()
	}
	sum, err := s.stats.SellerSummary(ctx, sellerID)
	if err != nil {
		return nil, nil, err
	}
	return u, sum, nil
}

type RestrictInput struct {
	Kind     string
	Reason   string
	Duration *time.Duration
	AdminID  string
	IP       string
}

func (s *AdminService) RestrictSeller(ctx context.Context, sellerID string, in RestrictInput) error {
	reason := strings.TrimSpace(in.Reason)
	if reason == "" {
		return apperr.Validation(map[string]string{"reason": "Reason is required"})
	}
	kind := in.Kind
	switch kind {
	case models.RestrictionSuspend, models.RestrictionRestrict, models.RestrictionBan:
	default:
		return apperr.Validation(map[string]string{"kind": "Invalid restriction kind"})
	}
	u, err := s.users.FindByID(ctx, sellerID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return apperr.NotFound()
		}
		return err
	}
	if u.Role != models.RoleSeller {
		return apperr.NotFound()
	}
	prevStatus := u.Status
	now := s.now().UTC()
	var endsAt *time.Time
	if kind != models.RestrictionBan {
		if in.Duration == nil || *in.Duration <= 0 {
			d := 7 * 24 * time.Hour
			in.Duration = &d
		}
		t := now.Add(*in.Duration)
		endsAt = &t
	}
	if err := s.restrictions.DeactivateActive(ctx, sellerID); err != nil {
		return err
	}
	if err := s.restrictions.Create(ctx, &models.SellerRestriction{
		ID: models.NewID(), SellerID: sellerID, Kind: kind, Reason: reason,
		AdminID: in.AdminID, StartsAt: now, EndsAt: endsAt, Active: true, CreatedAt: now,
	}); err != nil {
		return err
	}
	var newStatus models.Status
	switch kind {
	case models.RestrictionSuspend:
		newStatus = models.StatusSuspended
	case models.RestrictionRestrict:
		newStatus = models.StatusRestricted
	case models.RestrictionBan:
		newStatus = models.StatusBanned
	}
	if err := s.users.UpdateStatus(ctx, sellerID, newStatus); err != nil {
		return err
	}
	if s.tokens != nil {
		_ = s.tokens.RevokeAllForUser(ctx, sellerID, now)
	}
	s.visibilityChanged()
	s.recordLog(ctx, in.AdminID, "seller."+kind, "seller", sellerID, reason, string(prevStatus), string(newStatus), in.IP)
	return nil
}

func (s *AdminService) RestoreSeller(ctx context.Context, sellerID, reason, adminID, ip string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return apperr.Validation(map[string]string{"reason": "Reason is required"})
	}
	u, err := s.users.FindByID(ctx, sellerID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return apperr.NotFound()
		}
		return err
	}
	if u.Role != models.RoleSeller {
		return apperr.NotFound()
	}
	prev := u.Status
	now := s.now().UTC()
	if err := s.restrictions.DeactivateActive(ctx, sellerID); err != nil {
		return err
	}
	_ = s.restrictions.Create(ctx, &models.SellerRestriction{
		ID: models.NewID(), SellerID: sellerID, Kind: models.RestrictionRestore,
		Reason: reason, AdminID: adminID, StartsAt: now, Active: false, CreatedAt: now,
	})
	if err := s.users.UpdateStatus(ctx, sellerID, models.StatusActive); err != nil {
		return err
	}
	s.visibilityChanged()
	s.recordLog(ctx, adminID, "seller.restore", "seller", sellerID, reason, string(prev), string(models.StatusActive), ip)
	return nil
}

// SellerSlugHistory returns one seller's store-address changes, newest first.
func (s *AdminService) SellerSlugHistory(ctx context.Context, sellerID string) ([]*models.StoreSlugChange, error) {
	if s.slugHistory == nil {
		return nil, nil
	}
	return s.slugHistory.ListBySeller(ctx, sellerID, 100)
}

// ListSlugHistory returns store-address changes across all sellers, newest first.
func (s *AdminService) ListSlugHistory(ctx context.Context, f repository.SlugHistoryFilter) ([]*repository.SlugChangeItem, int64, error) {
	if s.slugHistory == nil {
		return nil, 0, nil
	}
	return s.slugHistory.List(ctx, f)
}

func (s *AdminService) SellerRestrictionHistory(ctx context.Context, sellerID string) ([]*models.SellerRestriction, error) {
	return s.restrictions.ListBySeller(ctx, sellerID, 50)
}

func (s *AdminService) AddNote(ctx context.Context, targetType, targetID, body, adminID string) (*models.AdminNote, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return nil, apperr.Validation(map[string]string{"body": "Note cannot be empty"})
	}
	if targetType != "seller" && targetType != "customer" {
		return nil, apperr.Validation(map[string]string{"targetType": "Must be seller or customer"})
	}
	now := s.now().UTC()
	n := &models.AdminNote{
		ID: models.NewID(), TargetType: targetType, TargetID: targetID,
		AdminID: adminID, Body: body, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.notes.Create(ctx, n); err != nil {
		return nil, err
	}
	s.recordLog(ctx, adminID, "note.create", targetType, targetID, "", nil, nil, "")
	return n, nil
}

func (s *AdminService) ListNotes(ctx context.Context, targetType, targetID string) ([]*models.AdminNote, error) {
	return s.notes.List(ctx, targetType, targetID, 50)
}

type GrantSubscriptionInput struct {
	Plan, Reason, AdminID, IP string
	DurationDays              int
	DiscountPct               int
	DiscountFixed             int64
	IsPromotional             bool
}

// GrantSubscription gives a seller a plan. A FREE plan is only OFFERED: the
// seller gets a notification and the plan starts when they claim it, so the
// first return value is nil and the second is the pending offer. A discounted
// (not free) plan is applied right away like before.
func (s *AdminService) GrantSubscription(ctx context.Context, sellerID string, in GrantSubscriptionInput) (*models.SubscriptionOverride, *models.SubscriptionGrant, error) {
	reason := strings.TrimSpace(in.Reason)
	if reason == "" {
		return nil, nil, apperr.Validation(map[string]string{"reason": "Reason is required"})
	}
	if in.DurationDays <= 0 {
		return nil, nil, apperr.Validation(map[string]string{"durationDays": "Duration must be positive"})
	}
	if _, ok := plans.Get(in.Plan); !ok || in.Plan == plans.Free {
		return nil, nil, apperr.Validation(map[string]string{"plan": "Plan must be pro or business"})
	}
	u, err := s.users.FindByID(ctx, sellerID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, nil, apperr.NotFound()
		}
		return nil, nil, err
	}
	if u.Role != models.RoleSeller {
		return nil, nil, apperr.NotFound()
	}

	// FREE plan: only make an offer. The seller is notified and the plan
	// starts when they claim it (SubscriptionGrantService.Claim).
	free := in.IsPromotional || in.DiscountPct >= 100
	if offers := s.grantOffersSvc(); free && offers != nil {
		g, err := offers.Offer(ctx, OfferInput{
			SellerID: sellerID, AdminID: in.AdminID, Plan: in.Plan,
			Days: in.DurationDays, Reason: reason, IP: in.IP,
		})
		if err != nil {
			return nil, nil, err
		}
		return nil, g, nil
	}

	now := s.now().UTC()
	if err := s.overrides.DeactivateActive(ctx, sellerID); err != nil {
		return nil, nil, err
	}
	o := &models.SubscriptionOverride{
		ID: models.NewID(), SellerID: sellerID, Plan: in.Plan,
		StartsAt: now, EndsAt: now.Add(time.Duration(in.DurationDays) * 24 * time.Hour),
		DiscountPct: in.DiscountPct, DiscountFixed: in.DiscountFixed,
		IsPromotional: in.IsPromotional, Reason: reason, AdminID: in.AdminID,
		Active: true, CreatedAt: now,
	}
	if err := s.overrides.Create(ctx, o); err != nil {
		return nil, nil, err
	}
	// The override above is only a history record; the subscription row is
	// what the plan checks read, so apply the plan there too.
	if s.subs != nil {
		if _, err := s.subs.AdminAssign(ctx, AssignInput{
			SellerID: sellerID, AdminID: in.AdminID, Plan: in.Plan,
			Days: in.DurationDays, Free: free,
			Note: reason,
		}); err != nil {
			return nil, nil, err
		}
	}
	s.recordLog(ctx, in.AdminID, "subscription.grant", "seller", sellerID, reason, nil, map[string]any{
		"plan": in.Plan, "days": in.DurationDays, "promotional": in.IsPromotional,
	}, in.IP)
	return o, nil, nil
}

func (s *AdminService) SellerSubscriptionHistory(ctx context.Context, sellerID string) ([]*models.SubscriptionOverride, error) {
	return s.overrides.ListBySeller(ctx, sellerID, 50)
}

func (s *AdminService) FeatureSeller(ctx context.Context, sellerID, kind, reason, adminID, ip string, priority int, endsAt *time.Time) (*models.SellerPromotion, error) {
	if kind == "" {
		kind = "feature"
	}
	if kind != "feature" && kind != "boost" && kind != "campaign" {
		return nil, apperr.Validation(map[string]string{"kind": "Invalid promotion kind"})
	}
	u, err := s.users.FindByID(ctx, sellerID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, apperr.NotFound()
		}
		return nil, err
	}
	if u.Role != models.RoleSeller {
		return nil, apperr.NotFound()
	}
	now := s.now().UTC()
	_ = s.promotions.DeactivateActive(ctx, sellerID, kind)
	p := &models.SellerPromotion{
		ID: models.NewID(), SellerID: sellerID, Kind: kind, Priority: priority,
		StartsAt: now, EndsAt: endsAt, AdminID: adminID, Reason: strings.TrimSpace(reason),
		Active: true, CreatedAt: now,
	}
	if err := s.promotions.Create(ctx, p); err != nil {
		return nil, err
	}
	s.recordLog(ctx, adminID, "seller."+kind, "seller", sellerID, reason, nil, map[string]any{"priority": priority}, ip)
	return p, nil
}

func (s *AdminService) UnfeatureSeller(ctx context.Context, sellerID, kind, adminID, ip string) error {
	if err := s.promotions.DeactivateActive(ctx, sellerID, kind); err != nil {
		return err
	}
	s.recordLog(ctx, adminID, "seller.unfeature", "seller", sellerID, "", nil, nil, ip)
	return nil
}

func (s *AdminService) SellerPromotions(ctx context.Context, sellerID string) ([]*models.SellerPromotion, error) {
	return s.promotions.ListBySeller(ctx, sellerID, 50)
}

func (s *AdminService) ListVerifications(ctx context.Context, status string, limit, skip int) ([]*models.SellerVerification, int64, error) {
	return s.verifications.List(ctx, status, limit, skip)
}

func (s *AdminService) ReviewVerification(ctx context.Context, id, status, reason, adminID, ip string) error {
	if status != models.VerificationApproved && status != models.VerificationRejected && status != models.VerificationRevoked {
		return apperr.Validation(map[string]string{"status": "Invalid status"})
	}
	if (status == models.VerificationRejected || status == models.VerificationRevoked) && strings.TrimSpace(reason) == "" {
		return apperr.Validation(map[string]string{"reason": "Reason is required"})
	}
	v, err := s.verifications.FindByID(ctx, id)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return apperr.NotFound()
		}
		return err
	}
	now := s.now().UTC()
	if err := s.verifications.UpdateStatus(ctx, id, status, adminID, reason, now); err != nil {
		return err
	}
	s.recordLog(ctx, adminID, "verification."+status, "seller", v.SellerID, reason, v.Status, status, ip)
	return nil
}

func (s *AdminService) ListReports(ctx context.Context, filter repository.ReportListFilter) ([]*models.SellerReport, int64, error) {
	return s.reports.List(ctx, filter)
}

func (s *AdminService) GetReport(ctx context.Context, id string) (*models.SellerReport, error) {
	r, err := s.reports.FindByID(ctx, id)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, apperr.NotFound()
		}
		return nil, err
	}
	return r, nil
}

type ResolveReportInput struct {
	Status, Resolution, AdminID, IP string
}

func (s *AdminService) ResolveReport(ctx context.Context, id string, in ResolveReportInput) error {
	switch in.Status {
	case models.ReportStatusResolved, models.ReportStatusDismissed, models.ReportStatusUnderReview,
		models.ReportStatusEscalated, models.ReportStatusWaitingSeller, models.ReportStatusWaitingEvidence:
	default:
		return apperr.Validation(map[string]string{"status": "Invalid status"})
	}
	r, err := s.reports.FindByID(ctx, id)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return apperr.NotFound()
		}
		return err
	}
	prev := r.Status
	now := s.now().UTC()
	r.Status = in.Status
	r.Resolution = strings.TrimSpace(in.Resolution)
	r.UpdatedAt = now
	if in.Status == models.ReportStatusResolved || in.Status == models.ReportStatusDismissed {
		r.ResolvedBy = in.AdminID
		r.ResolvedAt = &now
	}
	if err := s.reports.Update(ctx, r); err != nil {
		return err
	}
	s.recordLog(ctx, in.AdminID, "report."+in.Status, "report", id, in.Resolution, prev, in.Status, in.IP)
	return nil
}

func (s *AdminService) ListActivityLogs(ctx context.Context, filter repository.AdminLogFilter) ([]*models.AdminActivityLog, int64, error) {
	return s.logs.List(ctx, filter)
}

func (s *AdminService) ListCustomers(ctx context.Context, filter repository.UserListFilter) ([]*models.User, int64, error) {
	filter.Role = models.RoleBuyer
	return s.users.ListByRole(ctx, filter)
}

func (s *AdminService) RestrictCustomer(ctx context.Context, customerID, kind, reason, adminID, ip string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return apperr.Validation(map[string]string{"reason": "Reason is required"})
	}
	u, err := s.users.FindByID(ctx, customerID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return apperr.NotFound()
		}
		return err
	}
	if u.Role != models.RoleBuyer {
		return apperr.NotFound()
	}
	// Account type fix: a buyer who registered by mistake can be moved to a
	// seller account (they then land on /seller/setup at next login).
	if kind == "make_seller" {
		if err := s.users.UpdateRole(ctx, customerID, models.RoleSeller); err != nil {
			return err
		}
		if s.tokens != nil {
			_ = s.tokens.RevokeAllForUser(ctx, customerID, s.now().UTC())
		}
		s.recordLog(ctx, adminID, "customer.make_seller", "customer", customerID, reason,
			string(models.RoleBuyer), string(models.RoleSeller), ip)
		return nil
	}
	prev := u.Status
	var status models.Status
	switch kind {
	case "suspend":
		status = models.StatusSuspended
	case "restrict":
		status = models.StatusRestricted
	case "ban":
		status = models.StatusBanned
	case "restore":
		status = models.StatusActive
	default:
		return apperr.Validation(map[string]string{"kind": "Invalid kind"})
	}
	if err := s.users.UpdateStatus(ctx, customerID, status); err != nil {
		return err
	}
	if status != models.StatusActive && s.tokens != nil {
		_ = s.tokens.RevokeAllForUser(ctx, customerID, s.now().UTC())
	}
	s.recordLog(ctx, adminID, "customer."+kind, "customer", customerID, reason, string(prev), string(status), ip)
	return nil
}
