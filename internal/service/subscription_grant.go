package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/plans"
	"shopbd/backend/internal/repository"
)

const grantMaxReason = 500

// SlugFunc returns a seller's CURRENT store address ("" if none).
type SlugFunc func(ctx context.Context, sellerID string) string

// SubscriptionGrantService handles FREE subscriptions an admin gives to a
// seller. The admin only makes an offer; the plan starts when the seller
// claims it.
//
//	admin offers -> grant "pending" + bell notification to the seller
//	seller claims -> grant "claimed" + subscription row written (plan active)
type SubscriptionGrantService struct {
	grants    repository.SubscriptionGrantRepository
	subs      *SubscriptionService
	overrides repository.SubscriptionOverrideRepository
	notifs    repository.NotificationRepository
	logs      repository.AdminActivityLogRepository
	slugFor   SlugFunc
	now       func() time.Time
}

func NewSubscriptionGrantService(
	grants repository.SubscriptionGrantRepository,
	subs *SubscriptionService,
	overrides repository.SubscriptionOverrideRepository,
	notifs repository.NotificationRepository,
	logs repository.AdminActivityLogRepository,
	slugFor SlugFunc,
) *SubscriptionGrantService {
	return &SubscriptionGrantService{
		grants: grants, subs: subs, overrides: overrides,
		notifs: notifs, logs: logs, slugFor: slugFor, now: time.Now,
	}
}

/* ------------------------------------------------------------------ */
/* View                                                                */
/* ------------------------------------------------------------------ */

// GrantView is what the seller (and the admin) sees for a pending offer.
type GrantView struct {
	ID            string    `json:"id"`
	Plan          string    `json:"plan"`
	PlanName      string    `json:"planName"`
	Interval      string    `json:"interval"`
	Days          int       `json:"days"` // 0 = one interval
	DurationLabel string    `json:"durationLabel"`
	Reason        string    `json:"reason"`
	CreatedAt     time.Time `json:"createdAt"`
}

func durationLabel(days int, interval string) string {
	switch {
	case days == 1:
		return "1 day"
	case days > 1:
		return fmt.Sprintf("%d days", days)
	case interval == plans.Yearly:
		return "1 year"
	case interval == plans.HalfYearly:
		return "6 months"
	default:
		return "1 month"
	}
}

func planDisplayName(id string) string {
	if p, ok := plans.Get(id); ok && p.Name != "" {
		return p.Name
	}
	return id
}

func toGrantView(g *models.SubscriptionGrant) GrantView {
	return GrantView{
		ID: g.ID, Plan: g.Plan, PlanName: planDisplayName(g.Plan),
		Interval: g.Interval, Days: g.Days,
		DurationLabel: durationLabel(g.Days, g.Interval),
		Reason:        g.Reason, CreatedAt: g.CreatedAt,
	}
}

// pendingViews lists the seller's unclaimed offers.
func (g *SubscriptionGrantService) pendingViews(ctx context.Context, sellerID string) ([]GrantView, error) {
	rows, err := g.grants.ListPendingBySeller(ctx, sellerID)
	if err != nil {
		return nil, err
	}
	out := make([]GrantView, 0, len(rows))
	for _, r := range rows {
		out = append(out, toGrantView(r))
	}
	return out, nil
}

/* ------------------------------------------------------------------ */
/* Admin: offer + revoke                                               */
/* ------------------------------------------------------------------ */

// OfferInput is an admin offering a free plan to a seller.
type OfferInput struct {
	SellerID string
	AdminID  string
	Plan     string // pro | business
	Interval string // monthly | yearly (optional)
	Days     int    // 0 = one interval
	Extend   bool
	Reason   string // required, shown to the seller
	IP       string
}

// Offer stores a pending grant and sends the seller a notification. It does
// NOT change the seller's plan.
func (g *SubscriptionGrantService) Offer(ctx context.Context, in OfferInput) (*models.SubscriptionGrant, error) {
	fields := map[string]string{}

	reason := strings.TrimSpace(in.Reason)
	switch n := utf8.RuneCountInString(reason); {
	case n == 0:
		fields["reason"] = "Reason is required"
	case n > grantMaxReason:
		fields["reason"] = "Reason is too long"
	}
	if _, ok := plans.Get(in.Plan); !ok || in.Plan == plans.Free {
		fields["plan"] = "plan must be pro or business"
	}
	if in.Interval == "" {
		in.Interval = plans.Monthly
	}
	if !plans.ValidInterval(in.Interval) {
		fields["interval"] = "must be monthly, half_yearly or yearly"
	}
	if in.Days < 0 || in.Days > maxAdminDays {
		fields["days"] = "days must be between 1 and 3650"
	}
	if len(fields) > 0 {
		return nil, apperr.Validation(fields)
	}

	now := g.now().UTC()
	grant := &models.SubscriptionGrant{
		ID: models.NewID(), SellerID: in.SellerID,
		Plan: in.Plan, Interval: in.Interval, Days: in.Days, Extend: in.Extend,
		Reason: reason, AdminID: in.AdminID, Status: models.GrantPending,
		NotificationID: models.NewID(), CreatedAt: now,
	}
	if err := g.grants.Create(ctx, grant); err != nil {
		return nil, err
	}

	// Bell notification. If it fails the offer still exists and shows on the
	// seller's Subscription page, so we log and carry on.
	if g.notifs != nil {
		n := &models.Notification{
			ID:    grant.NotificationID,
			Title: "You received a free " + planDisplayName(grant.Plan) + " subscription",
			Body: fmt.Sprintf(
				"You got free %s access for %s. Reason: %s. Open your Subscription page and tap Claim to activate it.",
				planDisplayName(grant.Plan), durationLabel(grant.Days, grant.Interval), reason),
			Level:        models.NotificationSuccess,
			LinkURL:      g.subscriptionPath(ctx, in.SellerID),
			Audience:     models.NotificationAudienceSelected,
			RecipientIDs: []string{in.SellerID},
			CreatedBy:    in.AdminID,
			CreatedAt:    now,
		}
		if err := g.notifs.Create(ctx, n); err != nil {
			slog.Error("free-subscription notification failed", "seller", in.SellerID, "err", err)
		}
	}

	g.log(ctx, in.AdminID, "subscription.offer", in.SellerID, reason, in.IP, nil, map[string]any{
		"grantId": grant.ID, "plan": grant.Plan, "days": grant.Days, "interval": grant.Interval,
	})
	return grant, nil
}

// Revoke withdraws a pending offer (the notification is removed too).
func (g *SubscriptionGrantService) Revoke(ctx context.Context, sellerID, grantID, adminID, reason, ip string) error {
	gr, err := g.find(ctx, sellerID, grantID)
	if err != nil {
		return err
	}
	ok, err := g.grants.Revoke(ctx, gr.ID, sellerID, g.now().UTC())
	if err != nil {
		return err
	}
	if !ok {
		return grantUnavailable()
	}
	if g.notifs != nil && gr.NotificationID != "" {
		_ = g.notifs.Delete(ctx, gr.NotificationID)
	}
	g.log(ctx, adminID, "subscription.offer_revoke", sellerID, strings.TrimSpace(reason), ip,
		map[string]any{"grantId": gr.ID, "plan": gr.Plan}, nil)
	return nil
}

/* ------------------------------------------------------------------ */
/* Seller: claim                                                       */
/* ------------------------------------------------------------------ */

// Claim activates the offered plan for the seller. Only the first call wins.
func (g *SubscriptionGrantService) Claim(ctx context.Context, sellerID, grantID string) (*SubscriptionView, error) {
	gr, err := g.find(ctx, sellerID, grantID)
	if err != nil {
		return nil, err
	}
	now := g.now().UTC()

	ok, err := g.grants.MarkClaimed(ctx, gr.ID, sellerID, now)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, grantUnavailable()
	}

	sub, err := g.subs.AdminAssign(ctx, AssignInput{
		SellerID: sellerID, AdminID: gr.AdminID,
		Plan: gr.Plan, Interval: gr.Interval, Days: gr.Days,
		Free: true, Note: gr.Reason, ExtendOld: gr.Extend,
	})
	if err != nil {
		// Put the offer back so the seller can try again.
		if rerr := g.grants.Reopen(ctx, gr.ID); rerr != nil {
			slog.Error("could not reopen grant after failed claim", "grant", gr.ID, "err", rerr)
		}
		return nil, err
	}

	// History record for the admin dashboard (best effort).
	if g.overrides != nil {
		_ = g.overrides.DeactivateActive(ctx, sellerID)
		if err := g.overrides.Create(ctx, &models.SubscriptionOverride{
			ID: models.NewID(), SellerID: sellerID, Plan: gr.Plan, Interval: gr.Interval,
			StartsAt: now, EndsAt: sub.CurrentPeriodEnd,
			DiscountPct: 100, IsPromotional: true,
			Reason: gr.Reason, AdminID: gr.AdminID, Active: true, CreatedAt: now,
		}); err != nil {
			slog.Error("subscription history record failed", "seller", sellerID, "err", err)
		}
	}

	// The notification has done its job.
	if g.notifs != nil && gr.NotificationID != "" {
		_ = g.notifs.MarkRead(ctx, sellerID, []string{gr.NotificationID}, now)
	}

	g.log(ctx, gr.AdminID, "subscription.claim", sellerID, gr.Reason, "",
		nil, map[string]any{"grantId": gr.ID, "plan": gr.Plan, "end": sub.CurrentPeriodEnd})

	return g.subs.Get(ctx, sellerID)
}

/* ------------------------------------------------------------------ */
/* helpers                                                             */
/* ------------------------------------------------------------------ */

func grantUnavailable() error {
	return apperr.Conflict("GRANT_UNAVAILABLE",
		"This free subscription was already claimed or is no longer available.", "grant")
}

// find loads a grant and makes sure it belongs to this seller. A grant that
// belongs to someone else is reported as not found.
func (g *SubscriptionGrantService) find(ctx context.Context, sellerID, grantID string) (*models.SubscriptionGrant, error) {
	gr, err := g.grants.GetByID(ctx, strings.TrimSpace(grantID))
	if errors.Is(err, repository.ErrNotFound) {
		return nil, apperr.NotFound()
	}
	if err != nil {
		return nil, err
	}
	if gr.SellerID != sellerID {
		return nil, apperr.NotFound()
	}
	if gr.Status != models.GrantPending {
		return nil, grantUnavailable()
	}
	return gr, nil
}

func (g *SubscriptionGrantService) subscriptionPath(ctx context.Context, sellerID string) string {
	if g.slugFor != nil {
		if slug := g.slugFor(ctx, sellerID); slug != "" {
			return "/seller/" + url.PathEscape(slug) + "/subscription"
		}
	}
	return "/seller/dashboard"
}

func (g *SubscriptionGrantService) log(ctx context.Context, adminID, action, sellerID, reason, ip string, prev, next any) {
	if g.logs == nil {
		return
	}
	_ = g.logs.Create(ctx, &models.AdminActivityLog{
		ID: models.NewID(), AdminID: adminID, Action: action,
		TargetType: "seller", TargetID: sellerID,
		PreviousValue: prev, NewValue: next, Reason: reason, IP: ip,
		CreatedAt: g.now().UTC(),
	})
}
