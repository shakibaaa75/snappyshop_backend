package service

import (
	"context"
	"errors"
	"strings"
	"time"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/plans"
	"shopbd/backend/internal/repository"
)

// AssignInput is an admin assigning a plan to a seller.
type AssignInput struct {
	SellerID  string
	AdminID   string
	Plan      string // pro | business
	Interval  string // monthly | yearly (billing label; optional)
	Days      int    // length of the period; 0 = one interval
	Free      bool   // true: granted for free; false: paid plan billed outside the gateway
	Note      string
	ExtendOld bool // add on top of the current active period of the same plan
}

const maxAdminDays = 3650

func (s *SubscriptionService) get(ctx context.Context, sellerID string) (*models.Subscription, error) {
	sub, err := s.subs.GetBySeller(ctx, sellerID)
	if errors.Is(err, repository.ErrNotFound) {
		return nil, apperr.NotFound()
	}
	return sub, err
}

// AdminAssign sets a seller's plan. It replaces any current subscription
// (unless ExtendOld is set and the plan is unchanged and still active).
func (s *SubscriptionService) AdminAssign(ctx context.Context, in AssignInput) (*models.Subscription, error) {
	fields := map[string]string{}
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

	now := time.Now().UTC()
	end := plans.AddPeriod(now, in.Interval)
	if in.Days > 0 {
		end = now.AddDate(0, 0, in.Days)
	}

	sub, err := s.subs.GetBySeller(ctx, in.SellerID)
	switch {
	case err == nil:
		if in.ExtendOld && sub.Plan == in.Plan && sub.IsActive(now) {
			if in.Days > 0 {
				end = sub.CurrentPeriodEnd.AddDate(0, 0, in.Days)
			} else {
				end = plans.AddPeriod(sub.CurrentPeriodEnd, in.Interval)
			}
		}
	case errors.Is(err, repository.ErrNotFound):
		sub = &models.Subscription{ID: models.NewID(), SellerID: in.SellerID}
	default:
		return nil, err
	}

	sub.Plan = in.Plan
	sub.Interval = in.Interval
	sub.CurrentPeriodEnd = end
	sub.UpdatedAt = now
	sub.StartedAt = now
	sub.Status = models.SubStatusActive
	sub.GrantedBy = in.AdminID
	sub.Note = strings.TrimSpace(in.Note)
	if in.Free {
		sub.Source = models.SubSourceAdminFree
	} else {
		sub.Source = models.SubSourceAdminPaid
	}
	if err := s.subs.Upsert(ctx, sub); err != nil {
		return nil, err
	}
	return sub, nil
}

// AdminSetActive switches a seller's subscription on or off. Switching it off
// keeps the record and the end date, so it can be switched back on; the seller
// is on Free while it is off. Switching on fails if the period already ended
// (extend it first).
func (s *SubscriptionService) AdminSetActive(ctx context.Context, sellerID, adminID string, active bool) (*models.Subscription, error) {
	sub, err := s.get(ctx, sellerID)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	if active {
		if !sub.CurrentPeriodEnd.After(now) {
			return nil, apperr.Validation(map[string]string{
				"subscription": "This subscription has expired. Extend it to activate it again."})
		}
		sub.Status = models.SubStatusActive
	} else {
		sub.Status = models.SubStatusInactive
	}
	sub.GrantedBy = adminID
	sub.UpdatedAt = now
	if err := s.subs.Upsert(ctx, sub); err != nil {
		return nil, err
	}
	return sub, nil
}

// AdminExtend adds days to a seller's subscription. An expired subscription is
// extended from now; an active one from its current end. Extending also turns
// an inactive subscription back on.
func (s *SubscriptionService) AdminExtend(ctx context.Context, sellerID, adminID string, days int) (*models.Subscription, error) {
	if days <= 0 || days > maxAdminDays {
		return nil, apperr.Validation(map[string]string{"days": "days must be between 1 and 3650"})
	}
	sub, err := s.get(ctx, sellerID)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	base := now
	if sub.CurrentPeriodEnd.After(now) {
		base = sub.CurrentPeriodEnd
	}
	sub.CurrentPeriodEnd = base.AddDate(0, 0, days)
	sub.Status = models.SubStatusActive
	sub.GrantedBy = adminID
	sub.UpdatedAt = now
	if err := s.subs.Upsert(ctx, sub); err != nil {
		return nil, err
	}
	return sub, nil
}

// AdminRemove ends a seller's subscription immediately (back to Free).
func (s *SubscriptionService) AdminRemove(ctx context.Context, sellerID, adminID string) (*models.Subscription, error) {
	sub, err := s.get(ctx, sellerID)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	sub.CurrentPeriodEnd = now
	sub.Status = models.SubStatusInactive
	sub.GrantedBy = adminID
	sub.UpdatedAt = now
	if err := s.subs.Upsert(ctx, sub); err != nil {
		return nil, err
	}
	return sub, nil
}
