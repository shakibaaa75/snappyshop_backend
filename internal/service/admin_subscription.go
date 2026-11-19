package service

import (
	"context"
	"errors"
	"strings"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/repository"
)

// SetSubscriptions connects the admin service to the subscription service.
// Call it once from main.go. Without it the admin subscription actions
// return an error instead of silently doing nothing.
func (s *AdminService) SetSubscriptions(subs *SubscriptionService) {
	s.mu.Lock()
	s.subs = subs
	s.mu.Unlock()
}

// SetGrantOffers connects free-plan offers (admin offers, seller claims).
// Call it once from main.go. Without it a free plan is applied immediately.
func (s *AdminService) SetGrantOffers(g *SubscriptionGrantService) {
	s.mu.Lock()
	s.grantOffers = g
	s.mu.Unlock()
}

func (s *AdminService) grantOffersSvc() *SubscriptionGrantService {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.grantOffers
}

// RevokeOffer withdraws a free plan the seller has not claimed yet.
func (s *AdminService) RevokeOffer(ctx context.Context, sellerID, grantID, adminID, reason, ip string) (*SubscriptionView, error) {
	subs, err := s.subscriptions()
	if err != nil {
		return nil, err
	}
	offers := s.grantOffersSvc()
	if offers == nil {
		return nil, apperr.New(500, "SUBSCRIPTIONS_UNAVAILABLE", "Free-plan offers are not configured")
	}
	if err := s.requireSeller(ctx, sellerID); err != nil {
		return nil, err
	}
	if err := offers.Revoke(ctx, sellerID, grantID, adminID, reason, ip); err != nil {
		return nil, err
	}
	return subs.Get(ctx, sellerID)
}

func (s *AdminService) subscriptions() (*SubscriptionService, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.subs == nil {
		return nil, apperr.New(500, "SUBSCRIPTIONS_UNAVAILABLE", "Subscription service is not configured")
	}
	return s.subs, nil
}

// requireSeller returns NotFound unless id is a seller account.
func (s *AdminService) requireSeller(ctx context.Context, id string) error {
	u, err := s.users.FindByID(ctx, id)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return apperr.NotFound()
		}
		return err
	}
	if u.Role != models.RoleSeller {
		return apperr.NotFound()
	}
	return nil
}

func subSnapshot(sub *models.Subscription) map[string]any {
	if sub == nil {
		return nil
	}
	return map[string]any{
		"plan": sub.Plan, "interval": sub.Interval, "status": sub.Status,
		"source": sub.Source, "end": sub.CurrentPeriodEnd,
	}
}

// SellerSubscription returns the seller's current subscription view
// (plan, status, expiry, entitlements, payments).
func (s *AdminService) SellerSubscription(ctx context.Context, sellerID string) (*SubscriptionView, error) {
	subs, err := s.subscriptions()
	if err != nil {
		return nil, err
	}
	if err := s.requireSeller(ctx, sellerID); err != nil {
		return nil, err
	}
	return subs.Get(ctx, sellerID)
}

// AssignSubscription gives a seller a plan. A paid plan (billed offline) is
// applied right away; a FREE plan is offered and starts when the seller claims
// it. A reason is required so every change can be traced in the activity log.
func (s *AdminService) AssignSubscription(ctx context.Context, in AssignInput, reason, ip string) (*SubscriptionView, error) {
	subs, err := s.subscriptions()
	if err != nil {
		return nil, err
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return nil, apperr.Validation(map[string]string{"reason": "Reason is required"})
	}
	if err := s.requireSeller(ctx, in.SellerID); err != nil {
		return nil, err
	}
	// FREE plan: offer it. The seller is notified and the plan starts when
	// they claim it. The returned view lists it under pendingGrants.
	if offers := s.grantOffersSvc(); in.Free && offers != nil {
		if _, err := offers.Offer(ctx, OfferInput{
			SellerID: in.SellerID, AdminID: in.AdminID, Plan: in.Plan,
			Interval: in.Interval, Days: in.Days, Extend: in.ExtendOld,
			Reason: reason, IP: ip,
		}); err != nil {
			return nil, err
		}
		return subs.Get(ctx, in.SellerID)
	}
	prev, _ := subs.subs.GetBySeller(ctx, in.SellerID)
	in.Note = reason
	next, err := subs.AdminAssign(ctx, in)
	if err != nil {
		return nil, err
	}
	s.recordLog(ctx, in.AdminID, "subscription.assign", "seller", in.SellerID, reason,
		subSnapshot(prev), subSnapshot(next), ip)
	return subs.Get(ctx, in.SellerID)
}

// SetSubscriptionActive activates or deactivates a seller's subscription.
func (s *AdminService) SetSubscriptionActive(ctx context.Context, sellerID, adminID, reason, ip string, active bool) (*SubscriptionView, error) {
	subs, err := s.subscriptions()
	if err != nil {
		return nil, err
	}
	if err := s.requireSeller(ctx, sellerID); err != nil {
		return nil, err
	}
	prev, _ := subs.subs.GetBySeller(ctx, sellerID)
	next, err := subs.AdminSetActive(ctx, sellerID, adminID, active)
	if err != nil {
		return nil, err
	}
	action := "subscription.deactivate"
	if active {
		action = "subscription.activate"
	}
	s.recordLog(ctx, adminID, action, "seller", sellerID, strings.TrimSpace(reason),
		subSnapshot(prev), subSnapshot(next), ip)
	return subs.Get(ctx, sellerID)
}

// ExtendSubscription adds days to a seller's subscription.
func (s *AdminService) ExtendSubscription(ctx context.Context, sellerID, adminID, reason, ip string, days int) (*SubscriptionView, error) {
	subs, err := s.subscriptions()
	if err != nil {
		return nil, err
	}
	if err := s.requireSeller(ctx, sellerID); err != nil {
		return nil, err
	}
	prev, _ := subs.subs.GetBySeller(ctx, sellerID)
	next, err := subs.AdminExtend(ctx, sellerID, adminID, days)
	if err != nil {
		return nil, err
	}
	s.recordLog(ctx, adminID, "subscription.extend", "seller", sellerID, strings.TrimSpace(reason),
		subSnapshot(prev), subSnapshot(next), ip)
	return subs.Get(ctx, sellerID)
}

// RemoveSubscription ends a seller's subscription now (back to Free).
func (s *AdminService) RemoveSubscription(ctx context.Context, sellerID, adminID, reason, ip string) (*SubscriptionView, error) {
	subs, err := s.subscriptions()
	if err != nil {
		return nil, err
	}
	if err := s.requireSeller(ctx, sellerID); err != nil {
		return nil, err
	}
	prev, _ := subs.subs.GetBySeller(ctx, sellerID)
	next, err := subs.AdminRemove(ctx, sellerID, adminID)
	if err != nil {
		return nil, err
	}
	s.recordLog(ctx, adminID, "subscription.remove", "seller", sellerID, strings.TrimSpace(reason),
		subSnapshot(prev), subSnapshot(next), ip)
	return subs.Get(ctx, sellerID)
}
