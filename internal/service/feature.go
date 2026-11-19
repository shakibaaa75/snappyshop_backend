package service

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"time"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/payment/sslcommerz"
	"shopbd/backend/internal/repository"
)

// Gateway callback routes for one-time feature purchases. Register them in
// the router under /api/v1 (outside CSRF, like the subscription callbacks).
const (
	FeatureCallbackSuccessPath = "/api/v1/features/callback/success"
	FeatureCallbackFailPath    = "/api/v1/features/callback/fail"
	FeatureCallbackCancelPath  = "/api/v1/features/callback/cancel"
	FeatureIPNPath             = "/api/v1/features/ipn"
)

// DefaultTryTheLookPrice is the one-time price in whole taka. Override it with
// the TRY_THE_LOOK_PRICE_BDT environment variable (see cmd/api/main.go).
const DefaultTryTheLookPrice int64 = 999

// FeatureCatalog maps a feature key to its display name and one-time price.
type FeatureCatalog map[string]FeatureInfo

type FeatureInfo struct {
	Name  string
	Price int64 // whole taka
}

type FeatureService struct {
	ents    repository.FeatureEntitlementRepository
	pays    repository.FeaturePaymentRepository
	gw      PaymentGateway
	catalog FeatureCatalog
	cfg     SubscriptionConfig
	logs    repository.AdminActivityLogRepository // optional
	log     *slog.Logger
	now     func() time.Time
}

func NewFeatureService(
	ents repository.FeatureEntitlementRepository,
	pays repository.FeaturePaymentRepository,
	gw PaymentGateway,
	catalog FeatureCatalog,
	cfg SubscriptionConfig,
	logs repository.AdminActivityLogRepository,
	log *slog.Logger,
) *FeatureService {
	if log == nil {
		log = slog.Default()
	}
	return &FeatureService{
		ents: ents, pays: pays, gw: gw, catalog: catalog, cfg: cfg,
		logs: logs, log: log, now: time.Now,
	}
}

func featureLocked(name string) error {
	return apperr.New(http.StatusForbidden, "FEATURE_LOCKED",
		name+" is a premium feature. Unlock it to use it.")
}

func (s *FeatureService) info(feature string) (FeatureInfo, error) {
	fi, ok := s.catalog[feature]
	if !models.ValidFeature(feature) || !ok {
		return FeatureInfo{}, apperr.NotFound()
	}
	return fi, nil
}

/* ------------------------------------------------------------------ */
/* Access check (the one place every other part calls)                 */
/* ------------------------------------------------------------------ */

// HasAccess reports whether the seller may use the feature right now:
// bought or admin-granted, and not disabled or revoked.
func (s *FeatureService) HasAccess(ctx context.Context, sellerID, feature string) (bool, error) {
	e, err := s.ents.Get(ctx, sellerID, feature)
	if errors.Is(err, repository.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return e.IsActive(), nil
}

// Require returns a 403 FEATURE_LOCKED error when the seller has no access.
// Call it at the top of every create / publish / edit action of a feature.
func (s *FeatureService) Require(ctx context.Context, sellerID, feature string) error {
	ok, err := s.HasAccess(ctx, sellerID, feature)
	if err != nil {
		return err
	}
	if !ok {
		name := feature
		if fi, found := s.catalog[feature]; found {
			name = fi.Name
		}
		return featureLocked(name)
	}
	return nil
}

/* ------------------------------------------------------------------ */
/* Read                                                                */
/* ------------------------------------------------------------------ */

type FeaturePaymentView struct {
	ID        string    `json:"id"`
	Amount    int64     `json:"amount"`
	Method    string    `json:"method"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"createdAt"`
}

// FeatureView is what the seller dashboard and admin panel read.
type FeatureView struct {
	Feature     string               `json:"feature"`
	Name        string               `json:"name"`
	Price       int64                `json:"price"`  // taka, one time
	Status      string               `json:"status"` // locked | active | disabled | revoked
	Unlocked    bool                 `json:"unlocked"`
	Source      string               `json:"source,omitempty"` // purchased | admin_granted
	Note        string               `json:"note,omitempty"`
	PurchasedAt *time.Time           `json:"purchasedAt,omitempty"`
	GrantedAt   *time.Time           `json:"grantedAt,omitempty"`
	Payments    []FeaturePaymentView `json:"payments"`
}

// Get returns the seller's state for one feature.
func (s *FeatureService) Get(ctx context.Context, sellerID, feature string) (*FeatureView, error) {
	fi, err := s.info(feature)
	if err != nil {
		return nil, err
	}
	view := &FeatureView{
		Feature: feature, Name: fi.Name, Price: fi.Price,
		Status: "locked", Payments: []FeaturePaymentView{},
	}

	e, err := s.ents.Get(ctx, sellerID, feature)
	switch {
	case err == nil:
		view.Status = e.Status
		view.Unlocked = e.IsActive()
		view.Source = e.Source
		view.Note = e.Note
		view.PurchasedAt = e.PurchasedAt
		if e.Source == models.FeatureSourceAdminGrant {
			t := e.CreatedAt
			view.GrantedAt = &t
		}
	case errors.Is(err, repository.ErrNotFound):
		// never bought or granted: stays locked
	default:
		return nil, err
	}

	rows, err := s.pays.ListBySeller(ctx, sellerID, feature, 10)
	if err != nil {
		return nil, err
	}
	for _, p := range rows {
		view.Payments = append(view.Payments, FeaturePaymentView{
			ID: p.ID, Amount: p.Amount, Method: p.Method, Status: p.Status, CreatedAt: p.CreatedAt,
		})
	}
	return view, nil
}

/* ------------------------------------------------------------------ */
/* Checkout                                                            */
/* ------------------------------------------------------------------ */

type FeatureCheckoutInput struct {
	SellerID    string
	SellerName  string
	SellerEmail string
	Feature     string
	Method      string
	Slug        string
}

func (s *FeatureService) callback(path string) string {
	return strings.TrimRight(s.cfg.PublicAPIURL, "/") + path
}

// Checkout records a pending payment (price set by the server) and returns the
// gateway's hosted payment page URL. A seller who already owns the feature
// cannot pay again.
func (s *FeatureService) Checkout(ctx context.Context, in FeatureCheckoutInput) (string, error) {
	fi, err := s.info(in.Feature)
	if err != nil {
		return "", err
	}
	fields := map[string]string{}
	if !validMethods[in.Method] {
		fields["method"] = "must be bkash, nagad, or card"
	}
	if !subscriptionSlugRe.MatchString(in.Slug) {
		fields["slug"] = "invalid store slug"
	}
	if fi.Price <= 0 {
		return "", apperr.BadRequest("this feature is not for sale")
	}
	if len(fields) > 0 {
		return "", apperr.Validation(fields)
	}

	// Already unlocked: nothing to buy. A disabled or revoked row also blocks a
	// new payment, because paying must not override an admin decision silently.
	if e, err := s.ents.Get(ctx, in.SellerID, in.Feature); err == nil {
		if e.IsActive() {
			return "", apperr.New(http.StatusConflict, "ALREADY_UNLOCKED", fi.Name+" is already unlocked.")
		}
		return "", apperr.New(http.StatusConflict, "FEATURE_BLOCKED",
			"Access to "+fi.Name+" was turned off by an administrator. Please contact support.")
	} else if !errors.Is(err, repository.ErrNotFound) {
		return "", err
	}

	tranID, err := newFeatureTranID()
	if err != nil {
		return "", err
	}
	p := &models.FeaturePayment{
		ID: models.NewID(), SellerID: in.SellerID, Feature: in.Feature,
		Method: in.Method, Amount: fi.Price, Status: models.SubPayPending,
		TranID: tranID, Slug: in.Slug, CreatedAt: s.now().UTC(),
	}
	if err := s.pays.Create(ctx, p); err != nil {
		return "", err
	}

	name := strings.TrimSpace(in.SellerName)
	if name == "" {
		name = "ShopBD Seller"
	}
	email := strings.TrimSpace(in.SellerEmail)
	if email == "" {
		email = "noreply@shopbd.local"
	}

	payURL, err := s.gw.Init(ctx, sslcommerz.InitInput{
		TranID:        tranID,
		Amount:        fi.Price,
		SuccessURL:    s.callback(FeatureCallbackSuccessPath),
		FailURL:       s.callback(FeatureCallbackFailPath),
		CancelURL:     s.callback(FeatureCallbackCancelPath),
		IPNURL:        s.callback(FeatureIPNPath),
		ProductName:   "ShopBD " + fi.Name + " (one-time)",
		CustomerName:  name,
		CustomerEmail: email,
	})
	if err != nil {
		s.log.Error("feature: gateway init failed", "error", err.Error(), "tranId", tranID)
		_ = s.pays.MarkNotPaid(ctx, tranID, models.SubPayFailed)
		return "", apperr.New(http.StatusBadGateway, "PAYMENT_GATEWAY_ERROR",
			"The payment service is unavailable. Please try again in a moment.")
	}
	return payURL, nil
}

func newFeatureTranID() (string, error) {
	id, err := newTranID() // "SUB" + 24 hex
	if err != nil {
		return "", err
	}
	return "FTR" + strings.TrimPrefix(id, "SUB"), nil // 27 chars
}

/* ------------------------------------------------------------------ */
/* Confirm / fail (redirect AND IPN call these)                        */
/* ------------------------------------------------------------------ */

// ConfirmPayment verifies the payment with the gateway and unlocks the
// feature. Safe to call any number of times for the same payment. The
// returned payment is non-nil whenever the tranID was found.
func (s *FeatureService) ConfirmPayment(ctx context.Context, tranID, valID string) (*models.FeaturePayment, error) {
	tranID = strings.TrimSpace(tranID)
	if tranID == "" {
		return nil, apperr.BadRequest("tran_id is required")
	}
	p, err := s.pays.FindByTranID(ctx, tranID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, apperr.NotFound()
		}
		return nil, err
	}
	if p.Status == models.SubPayPaid {
		return p, nil
	}

	v, err := s.gw.Validate(ctx, strings.TrimSpace(valID))
	if err != nil {
		s.log.Error("feature: validate failed", "error", err.Error(), "tranId", tranID)
		return p, apperr.BadRequest("payment could not be verified")
	}

	// Trust the gateway's answer, not the callback's form values.
	cur := v.CurrencyType
	if cur == "" {
		cur = v.Currency
	}
	got, amtErr := v.AmountTaka()
	if !v.OK() || v.TranID != tranID || cur != "BDT" ||
		amtErr != nil || math.Abs(got-float64(p.Amount)) > 0.01 {
		s.log.Warn("feature: verification mismatch",
			"tranId", tranID, "status", v.Status, "expected", p.Amount, "got", string(v.Amount))
		return p, apperr.BadRequest("payment could not be verified")
	}
	if v.Risky() {
		s.log.Warn("feature: payment held for review", "tranId", tranID)
		return p, apperr.BadRequest("payment is under review")
	}

	won, err := s.pays.MarkPaid(ctx, tranID, v.ValID, s.now().UTC())
	if err != nil {
		return p, err
	}
	if won {
		if err := s.unlockByPurchase(ctx, p); err != nil {
			// Let a later callback retry instead of losing a paid purchase.
			_ = s.pays.ResetPaid(ctx, tranID)
			s.log.Error("feature: unlock failed", "error", err.Error(), "tranId", tranID)
			return p, err
		}
	}
	p.Status = models.SubPayPaid
	return p, nil
}

// unlockByPurchase stores the purchase. If the seller somehow already owns the
// feature (for example an admin granted it while the payment was open), the
// existing row is kept active and only gains the purchase details.
func (s *FeatureService) unlockByPurchase(ctx context.Context, p *models.FeaturePayment) error {
	now := s.now().UTC()
	e, err := s.ents.Get(ctx, p.SellerID, p.Feature)
	switch {
	case err == nil:
		// keep ID and CreatedAt
	case errors.Is(err, repository.ErrNotFound):
		e = &models.FeatureEntitlement{
			ID: models.NewID(), SellerID: p.SellerID, Feature: p.Feature, CreatedAt: now,
		}
	default:
		return err
	}
	e.Status = models.FeatureActive
	e.Source = models.FeatureSourcePurchase
	e.Amount = p.Amount
	e.GrantedBy = ""
	e.Note = ""
	e.PurchasedAt = &now
	e.UpdatedAt = now
	return s.ents.Upsert(ctx, e)
}

// FailPayment records a failed or cancelled attempt (only if still pending).
func (s *FeatureService) FailPayment(ctx context.Context, tranID, status string) (*models.FeaturePayment, error) {
	tranID = strings.TrimSpace(tranID)
	if tranID == "" {
		return nil, apperr.BadRequest("tran_id is required")
	}
	p, err := s.pays.FindByTranID(ctx, tranID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, apperr.NotFound()
		}
		return nil, err
	}
	if err := s.pays.MarkNotPaid(ctx, tranID, status); err != nil {
		return p, err
	}
	return p, nil
}

/* ------------------------------------------------------------------ */
/* Admin                                                               */
/* ------------------------------------------------------------------ */

// Admin audit actions.
const (
	ActionFeatureGrant   = "feature.grant"
	ActionFeatureDisable = "feature.disable"
	ActionFeatureEnable  = "feature.enable"
	ActionFeatureRevoke  = "feature.revoke"
)

// AdminGet is the same view the seller sees, for the admin seller page.
func (s *FeatureService) AdminGet(ctx context.Context, sellerID, feature string) (*FeatureView, error) {
	return s.Get(ctx, sellerID, feature)
}

// AdminGrant gives the feature to a seller for free, or turns it back on after
// it was disabled or revoked. A feature the seller paid for keeps its
// "purchased" source, so a revoke and re-grant never erases the purchase.
func (s *FeatureService) AdminGrant(ctx context.Context, adminID, sellerID, feature, reason, ip string) (*FeatureView, error) {
	if _, err := s.info(feature); err != nil {
		return nil, err
	}
	now := s.now().UTC()
	prev := "locked"

	e, err := s.ents.Get(ctx, sellerID, feature)
	switch {
	case err == nil:
		prev = e.Status
		if e.IsActive() {
			return s.Get(ctx, sellerID, feature) // already on: nothing to change
		}
	case errors.Is(err, repository.ErrNotFound):
		e = &models.FeatureEntitlement{
			ID: models.NewID(), SellerID: sellerID, Feature: feature,
			Source: models.FeatureSourceAdminGrant, CreatedAt: now,
		}
	default:
		return nil, err
	}
	e.Status = models.FeatureActive
	e.GrantedBy = adminID
	e.Note = strings.TrimSpace(reason)
	e.UpdatedAt = now
	if err := s.ents.Upsert(ctx, e); err != nil {
		return nil, err
	}
	action := ActionFeatureGrant
	if prev == models.FeatureDisabled || prev == models.FeatureRevoked {
		action = ActionFeatureEnable
	}
	s.audit(ctx, adminID, action, sellerID, feature, reason, ip, prev, models.FeatureActive)
	return s.Get(ctx, sellerID, feature)
}

// AdminSetStatus turns a seller's access off (disabled: temporary, revoked:
// withdrawn). It never deletes the row, so purchase history is kept.
func (s *FeatureService) AdminSetStatus(ctx context.Context, adminID, sellerID, feature, status, reason, ip string) (*FeatureView, error) {
	if _, err := s.info(feature); err != nil {
		return nil, err
	}
	if status != models.FeatureDisabled && status != models.FeatureRevoked {
		return nil, apperr.Validation(map[string]string{"status": "must be disabled or revoked"})
	}
	e, err := s.ents.Get(ctx, sellerID, feature)
	if errors.Is(err, repository.ErrNotFound) {
		return nil, apperr.BadRequest("this seller does not have the feature")
	}
	if err != nil {
		return nil, err
	}
	prev := e.Status
	if prev == status {
		return s.Get(ctx, sellerID, feature)
	}
	e.Status = status
	e.GrantedBy = adminID
	e.Note = strings.TrimSpace(reason)
	e.UpdatedAt = s.now().UTC()
	if err := s.ents.Upsert(ctx, e); err != nil {
		return nil, err
	}
	action := ActionFeatureDisable
	if status == models.FeatureRevoked {
		action = ActionFeatureRevoke
	}
	s.audit(ctx, adminID, action, sellerID, feature, reason, ip, prev, status)
	return s.Get(ctx, sellerID, feature)
}

func (s *FeatureService) audit(ctx context.Context, adminID, action, sellerID, feature, reason, ip string, prev, next any) {
	if s.logs == nil {
		return
	}
	_ = s.logs.Create(ctx, &models.AdminActivityLog{
		ID: models.NewID(), AdminID: adminID, Action: action,
		TargetType: "seller", TargetID: sellerID,
		PreviousValue: map[string]any{"feature": feature, "status": prev},
		NewValue:      map[string]any{"feature": feature, "status": next},
		Reason:        reason, IP: ip, CreatedAt: s.now().UTC(),
	})
}
