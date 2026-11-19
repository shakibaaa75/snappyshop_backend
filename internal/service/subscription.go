package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"regexp"
	"strings"
	"time"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/payment/sslcommerz"
	"shopbd/backend/internal/plans"
	"shopbd/backend/internal/repository"
)

// Callback routes the gateway calls. Register them in the router under /api/v1.
const (
	CallbackSuccessPath = "/api/v1/subscription/callback/success"
	CallbackFailPath    = "/api/v1/subscription/callback/fail"
	CallbackCancelPath  = "/api/v1/subscription/callback/cancel"
	IPNPath             = "/api/v1/subscription/ipn"
)

type PaymentGateway interface {
	Init(ctx context.Context, in sslcommerz.InitInput) (string, error)
	Validate(ctx context.Context, valID string) (*sslcommerz.Validation, error)
}

type SubscriptionConfig struct {
	PublicAPIURL string // base URL the gateway can reach, e.g. https://api.example.com
	FrontendURL  string // base URL of the Next.js app
}

type SubscriptionService struct {
	subs repository.SubscriptionRepository
	pays repository.SubscriptionPaymentRepository
	gw   PaymentGateway
	cfg  SubscriptionConfig
	log  *slog.Logger

	// grants lists free-subscription offers waiting to be claimed. Optional;
	// set with SetGrants.
	grants *SubscriptionGrantService
}

// SetGrants lets Get include the seller's unclaimed free-subscription offers.
func (s *SubscriptionService) SetGrants(g *SubscriptionGrantService) { s.grants = g }

func NewSubscriptionService(
	subs repository.SubscriptionRepository,
	pays repository.SubscriptionPaymentRepository,
	gw PaymentGateway,
	cfg SubscriptionConfig,
	log *slog.Logger,
) *SubscriptionService {
	if log == nil {
		log = slog.Default()
	}
	return &SubscriptionService{subs: subs, pays: pays, gw: gw, cfg: cfg, log: log}
}

/* ------------------------------------------------------------------ */
/* Entitlements                                                        */
/* ------------------------------------------------------------------ */

func freePlan() plans.Plan {
	p, _ := plans.Get(plans.Free)
	return p
}

// PlanFor returns the plan a seller is entitled to right now. A missing or
// expired subscription means Free; nothing has to be updated when time passes.
func (s *SubscriptionService) PlanFor(ctx context.Context, sellerID string) (plans.Plan, error) {
	sub, err := s.subs.GetBySeller(ctx, sellerID)
	if errors.Is(err, repository.ErrNotFound) {
		return freePlan(), nil
	}
	if err != nil {
		return plans.Plan{}, err
	}
	if !sub.IsActive(time.Now()) {
		return freePlan(), nil
	}
	p, ok := plans.Get(sub.Plan)
	if !ok {
		return freePlan(), nil
	}
	return p, nil
}

func planLimit(msg string) error {
	return apperr.New(http.StatusForbidden, "PLAN_LIMIT", msg)
}

// CheckProductLimit: call from product create with the seller's current count.
func (s *SubscriptionService) CheckProductLimit(ctx context.Context, sellerID string, current int) error {
	p, err := s.PlanFor(ctx, sellerID)
	if err != nil {
		return err
	}
	if current >= p.MaxProducts {
		return planLimit(fmt.Sprintf(
			"Your %s plan allows up to %d products. Upgrade to add more.", p.Name, p.MaxProducts))
	}
	return nil
}

// RequireAnalytics: call at the top of the analytics overview.
func (s *SubscriptionService) RequireAnalytics(ctx context.Context, sellerID string) error {
	p, err := s.PlanFor(ctx, sellerID)
	if err != nil {
		return err
	}
	if !p.Analytics {
		return planLimit("Analytics is available on the Pro and Business plans.")
	}
	return nil
}

// RequireCustomDomain: call when a seller sets a custom domain.
func (s *SubscriptionService) RequireCustomDomain(ctx context.Context, sellerID string) error {
	p, err := s.PlanFor(ctx, sellerID)
	if err != nil {
		return err
	}
	if !p.CustomDomain {
		return planLimit("Custom domains are available on the Business plan.")
	}
	return nil
}

// RequireMetaPixel: call before saving or enabling Meta Pixel settings.
func (s *SubscriptionService) RequireMetaPixel(ctx context.Context, sellerID string) error {
	p, err := s.PlanFor(ctx, sellerID)
	if err != nil {
		return err
	}
	if !p.MetaPixel {
		return planLimit("Meta Pixel is locked on the Free plan. Upgrade to use it.")
	}
	return nil
}

// RequirePyxel: call before saving or enabling Pyxel settings.
func (s *SubscriptionService) RequirePyxel(ctx context.Context, sellerID string) error {
	p, err := s.PlanFor(ctx, sellerID)
	if err != nil {
		return err
	}
	if !p.Pyxel {
		return planLimit("Pyxel is locked on the Free plan. Upgrade to use it.")
	}
	return nil
}

// RequireAutoCourier: call before connecting, configuring or using an
// automatic courier integration.
func (s *SubscriptionService) RequireAutoCourier(ctx context.Context, sellerID string) error {
	p, err := s.PlanFor(ctx, sellerID)
	if err != nil {
		return err
	}
	if !p.AutoCourier {
		return planLimit("Automatic courier integration is locked on the Free plan. Upgrade to use it.")
	}
	return nil
}

// CheckTemplateChoice: call when saving a store design. Empty ID is skipped.
func (s *SubscriptionService) CheckTemplateChoice(ctx context.Context, sellerID, templateID string) error {
	p, err := s.PlanFor(ctx, sellerID)
	if err != nil {
		return err
	}
	if templateID != "" && !p.TemplateAllowed(templateID) {
		return planLimit("This template isn't included in your plan. Upgrade to use it.")
	}
	return nil
}

// CheckDesignChoice: call when saving a store design. Empty IDs are skipped.
func (s *SubscriptionService) CheckDesignChoice(ctx context.Context, sellerID, themeID, cardID string) error {
	p, err := s.PlanFor(ctx, sellerID)
	if err != nil {
		return err
	}
	// "custom" is what the editor saves for templates and hand-picked colors;
	// it is not one of the numbered store themes, so it is never locked.
	if themeID != "" && themeID != "custom" && !p.ThemeAllowed(themeID) {
		return planLimit("This theme isn't included in your plan. Upgrade to use it.")
	}
	if cardID != "" && !p.CardAllowed(cardID) {
		return planLimit("This card design isn't included in your plan. Upgrade to use it.")
	}
	return nil
}

// CheckPremiumDesign: call when saving a store design. Free sellers cannot
// save the Gallery grid card or an enabled Image gallery section.
func (s *SubscriptionService) CheckPremiumDesign(ctx context.Context, sellerID string, design map[string]any) error {
	p, err := s.PlanFor(ctx, sellerID)
	if err != nil {
		return err
	}
	if p.PremiumDesign {
		return nil
	}
	if v, _ := design["cardVariant"].(string); strings.TrimSpace(v) == "grid" {
		return planLimit("The Gallery grid card is a premium feature. Upgrade to use it.")
	}
	if secs, ok := design["sections"].([]any); ok {
		for _, raw := range secs {
			sec, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if id, _ := sec["id"].(string); id != "gallery" {
				continue
			}
			if on, ok := sec["enabled"].(bool); ok && !on {
				continue
			}
			return planLimit("The Image gallery section is a premium feature. Upgrade to use it.")
		}
	}
	return nil
}

// sectionIDs returns the homepage section IDs of a design, in order.
func sectionIDs(design map[string]any) []string {
	secs, _ := design["sections"].([]any)
	out := make([]string, 0, len(secs))
	for _, raw := range secs {
		sec, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if id, _ := sec["id"].(string); id != "" {
			out = append(out, id)
		}
	}
	return out
}

// sectionOrderChanged reports whether the sections both designs have in
// common appear in a different order. Sections only one side has (a section
// added later by the app) are ignored, so they never count as a re-arrangement.
func sectionOrderChanged(prev, next []string) bool {
	inPrev := make(map[string]bool, len(prev))
	for _, id := range prev {
		inPrev[id] = true
	}
	inNext := make(map[string]bool, len(next))
	for _, id := range next {
		inNext[id] = true
	}
	var a, b []string
	for _, id := range prev {
		if inNext[id] {
			a = append(a, id)
		}
	}
	for _, id := range next {
		if inPrev[id] {
			b = append(b, id)
		}
	}
	if len(a) != len(b) {
		return true
	}
	for i := range a {
		if a[i] != b[i] {
			return true
		}
	}
	return false
}

// CheckSectionOrder: call when saving a store design, with the seller's
// stored draft (nil on the first save). Arranging the homepage sections is a
// premium feature, so a Free seller cannot save a different order.
//
// Choosing a different template is allowed to reset the order, because every
// template ships its own section order. Showing or hiding a section never
// counts as arranging.
func (s *SubscriptionService) CheckSectionOrder(ctx context.Context, sellerID string, design, previous map[string]any) error {
	if previous == nil {
		return nil
	}
	p, err := s.PlanFor(ctx, sellerID)
	if err != nil {
		return err
	}
	if p.PremiumDesign {
		return nil
	}
	newTemplate, _ := design["template"].(string)
	oldTemplate, _ := previous["template"].(string)
	if strings.TrimSpace(newTemplate) != strings.TrimSpace(oldTemplate) {
		return nil
	}
	if sectionOrderChanged(sectionIDs(previous), sectionIDs(design)) {
		return planLimit("Arranging your homepage sections is a premium feature. Upgrade to use it.")
	}
	return nil
}

/* ------------------------------------------------------------------ */
/* Read                                                                */
/* ------------------------------------------------------------------ */

type SubscriptionPaymentView struct {
	ID        string    `json:"id"`
	Plan      string    `json:"plan"`
	Interval  string    `json:"interval"`
	Amount    int64     `json:"amount"`
	Method    string    `json:"method"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"createdAt"`
	// Price breakdown (absent on payments made before discounts existed).
	Months      int   `json:"months,omitempty"`
	ListAmount  int64 `json:"listAmount,omitempty"`
	DiscountPct int   `json:"discountPct,omitempty"`
}

// SubscriptionView matches the frontend's SubscriptionInfo type.
type SubscriptionView struct {
	Plan             string                    `json:"plan"`
	Status           string                    `json:"status"` // active | expired | none
	Interval         string                    `json:"interval,omitempty"`
	CurrentPeriodEnd *time.Time                `json:"currentPeriodEnd,omitempty"`
	Source           string                    `json:"source,omitempty"` // payment | admin_free | admin_paid
	Entitlements     Entitlements              `json:"entitlements"`
	Payments         []SubscriptionPaymentView `json:"payments"`
	// PendingGrants are free subscriptions an admin offered that the seller
	// has not claimed yet. They give NO access until claimed.
	PendingGrants []GrantView `json:"pendingGrants"`
	// FirstPurchaseEligible is true until the seller completes a first paid
	// purchase. The first-purchase discount applies while it is true.
	FirstPurchaseEligible bool `json:"firstPurchaseEligible"`
}

// Entitlements is what the seller's CURRENT plan allows. The frontend uses it
// to draw locks; the backend enforces the same rules independently.
type Entitlements struct {
	Plan         string   `json:"plan"`
	MaxProducts  int      `json:"maxProducts"`
	Themes       []string `json:"themes"`    // allowed theme IDs; null = all themes
	Cards        []string `json:"cards"`     // allowed card IDs; null = all
	Templates    []string `json:"templates"` // allowed store template IDs; null = all
	Analytics    bool     `json:"analytics"`
	CustomDomain bool     `json:"customDomain"`
	MetaPixel    bool     `json:"metaPixel"`
	Pyxel        bool     `json:"pyxel"`
	AutoCourier  bool     `json:"autoCourier"`
	// PremiumDesign unlocks the Gallery grid card, the Image gallery section and
	// arranging the homepage sections.
	PremiumDesign bool `json:"premiumDesign"`
	// Extras are the keys of the extra features an admin added to this plan
	// (Admin > Plans). The app can gate a screen with extras.includes("key").
	Extras []string `json:"extras"`
}

func entitlementsOf(p plans.Plan) Entitlements {
	return Entitlements{
		Plan: p.ID, MaxProducts: p.MaxProducts, Themes: p.FreeThemes, Cards: p.FreeCards, Templates: p.FreeTemplates,
		Analytics: p.Analytics, CustomDomain: p.CustomDomain,
		MetaPixel: p.MetaPixel, Pyxel: p.Pyxel, AutoCourier: p.AutoCourier,
		PremiumDesign: p.PremiumDesign,
		Extras:        p.ExtraKeys(),
	}
}

// Entitlements returns the seller's current entitlements.
func (s *SubscriptionService) Entitlements(ctx context.Context, sellerID string) (Entitlements, error) {
	p, err := s.PlanFor(ctx, sellerID)
	if err != nil {
		return Entitlements{}, err
	}
	return entitlementsOf(p), nil
}

func (s *SubscriptionService) Get(ctx context.Context, sellerID string) (*SubscriptionView, error) {
	view := &SubscriptionView{
		Plan:          plans.Free,
		Status:        "none",
		Payments:      []SubscriptionPaymentView{},
		PendingGrants: []GrantView{},
	}
	view.Entitlements = entitlementsOf(freePlan())

	sub, err := s.subs.GetBySeller(ctx, sellerID)
	switch {
	case err == nil:
		end := sub.CurrentPeriodEnd
		view.Plan = sub.Plan
		view.Interval = sub.Interval
		view.CurrentPeriodEnd = &end
		view.Source = sub.Source
		switch {
		case sub.Status == models.SubStatusInactive:
			view.Status = "inactive"
		case end.After(time.Now()):
			view.Status = "active"
		default:
			view.Status = "expired"
		}
		if sub.IsActive(time.Now()) {
			if pl, ok := plans.Get(sub.Plan); ok {
				view.Entitlements = entitlementsOf(pl)
			}
		}
	case errors.Is(err, repository.ErrNotFound):
		// never paid: stays Free / none
	default:
		return nil, err
	}

	if s.grants != nil {
		pending, err := s.grants.pendingViews(ctx, sellerID)
		if err != nil {
			return nil, err
		}
		view.PendingGrants = pending
	}

	rows, err := s.pays.ListBySeller(ctx, sellerID, 20)
	if err != nil {
		return nil, err
	}
	for _, p := range rows {
		view.Payments = append(view.Payments, SubscriptionPaymentView{
			ID: p.ID, Plan: p.Plan, Interval: p.Interval, Amount: p.Amount,
			Method: p.Method, Status: p.Status, CreatedAt: p.CreatedAt,
			Months: p.Months, ListAmount: p.ListAmount, DiscountPct: p.DiscountPct,
		})
	}

	paid, err := s.pays.HasPaid(ctx, sellerID)
	if err != nil {
		return nil, err
	}
	view.FirstPurchaseEligible = !paid
	return view, nil
}

/* ------------------------------------------------------------------ */
/* Checkout                                                            */
/* ------------------------------------------------------------------ */

type CheckoutInput struct {
	SellerID    string
	SellerName  string // optional, for the gateway receipt
	SellerEmail string // optional, for the gateway receipt
	Plan        string
	Interval    string
	Method      string
	Slug        string
}

var (
	subscriptionSlugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	validMethods       = map[string]bool{"bkash": true, "nagad": true, "card": true}
)

func newTranID() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "SUB" + hex.EncodeToString(b), nil // 27 chars, the gateway allows 30
}

func (s *SubscriptionService) callback(path string) string {
	return strings.TrimRight(s.cfg.PublicAPIURL, "/") + path
}

// Checkout records a pending payment (price set by the server) and returns
// the gateway's hosted payment page URL.
func (s *SubscriptionService) Checkout(ctx context.Context, in CheckoutInput) (string, error) {
	fields := map[string]string{}

	// The first paid purchase gets an extra discount. Decided here, on the
	// server, never from anything the browser sends.
	paid, err := s.pays.HasPaid(ctx, in.SellerID)
	if err != nil {
		return "", err
	}
	quote, ok := plans.QuoteFor(in.Plan, in.Interval, !paid)
	if !ok {
		fields["plan"] = "choose a paid plan and a period of 1, 6 or 12 months"
	}
	if !validMethods[in.Method] {
		fields["method"] = "must be bkash, nagad, or card"
	}
	if !subscriptionSlugRe.MatchString(in.Slug) {
		fields["slug"] = "invalid store slug"
	}
	if len(fields) > 0 {
		return "", apperr.Validation(fields)
	}

	tranID, err := newTranID()
	if err != nil {
		return "", err
	}
	amount := quote.Amount
	p := &models.SubscriptionPayment{
		ID:            models.NewID(),
		SellerID:      in.SellerID,
		Plan:          in.Plan,
		Interval:      in.Interval,
		Method:        in.Method,
		Amount:        amount,
		Months:        quote.Months,
		ListAmount:    quote.ListAmount,
		DiscountPct:   quote.TotalPct,
		FirstPurchase: quote.FirstPurchasePct > 0,
		Status:        models.SubPayPending,
		TranID:        tranID,
		Slug:          in.Slug,
		CreatedAt:     time.Now().UTC(),
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
		email = "noreply@shopbd.local" // TODO: wire the seller's real email
	}
	planName := in.Plan
	if pl, ok := plans.Get(in.Plan); ok {
		planName = pl.Name
	}

	payURL, err := s.gw.Init(ctx, sslcommerz.InitInput{
		TranID:        tranID,
		Amount:        amount,
		SuccessURL:    s.callback(CallbackSuccessPath),
		FailURL:       s.callback(CallbackFailPath),
		CancelURL:     s.callback(CallbackCancelPath),
		IPNURL:        s.callback(IPNPath),
		ProductName:   fmt.Sprintf("ShopBD %s (%d months)", planName, quote.Months),
		CustomerName:  name,
		CustomerEmail: email,
	})
	if err != nil {
		s.log.Error("subscription: gateway init failed", "error", err.Error(), "tranId", tranID)
		_ = s.pays.MarkNotPaid(ctx, tranID, models.SubPayFailed)
		return "", apperr.New(http.StatusBadGateway, "PAYMENT_GATEWAY_ERROR",
			"The payment service is unavailable. Please try again in a moment.")
	}
	return payURL, nil
}

/* ------------------------------------------------------------------ */
/* Confirm / fail (called by the redirect AND the IPN)                 */
/* ------------------------------------------------------------------ */

// ConfirmPayment verifies a payment with the gateway and activates the plan.
// It is safe to call any number of times for the same payment. The returned
// payment is non-nil whenever the tranID was found, even alongside an error,
// so callers can still redirect the seller to the right store page.
func (s *SubscriptionService) ConfirmPayment(ctx context.Context, tranID, valID string) (*models.SubscriptionPayment, error) {
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
		s.log.Error("subscription: validate failed", "error", err.Error(), "tranId", tranID)
		return p, apperr.BadRequest("payment could not be verified")
	}

	// Trust the gateway's answer, not the callback's query/form values.
	cur := v.CurrencyType
	if cur == "" {
		cur = v.Currency
	}
	got, amtErr := v.AmountTaka()
	if !v.OK() || v.TranID != tranID || cur != "BDT" ||
		amtErr != nil || math.Abs(got-float64(p.Amount)) > 0.01 {
		s.log.Warn("subscription: verification mismatch",
			"tranId", tranID, "status", v.Status, "expected", p.Amount, "got", string(v.Amount))
		return p, apperr.BadRequest("payment could not be verified")
	}
	if v.Risky() {
		s.log.Warn("subscription: payment held for review", "tranId", tranID)
		return p, apperr.BadRequest("payment is under review")
	}

	won, err := s.pays.MarkPaid(ctx, tranID, v.ValID, time.Now().UTC())
	if err != nil {
		return p, err
	}
	if won {
		if err := s.extend(ctx, p); err != nil {
			// Let a later callback retry instead of losing a paid period.
			_ = s.pays.ResetPaid(ctx, tranID)
			s.log.Error("subscription: activation failed", "error", err.Error(), "tranId", tranID)
			return p, err
		}
	}
	p.Status = models.SubPayPaid
	return p, nil
}

// extend adds one period to the seller's subscription.
// Same plan and still active: the new period starts at the current end date.
// Otherwise (new plan or expired): it starts now. No proration; switching
// plans mid-period forfeits the remaining time on the old plan.
func (s *SubscriptionService) extend(ctx context.Context, p *models.SubscriptionPayment) error {
	now := time.Now().UTC()
	base := now

	sub, err := s.subs.GetBySeller(ctx, p.SellerID)
	switch {
	case err == nil:
		if sub.Plan == p.Plan && sub.IsActive(now) {
			base = sub.CurrentPeriodEnd
		}
	case errors.Is(err, repository.ErrNotFound):
		sub = &models.Subscription{ID: models.NewID(), SellerID: p.SellerID}
	default:
		return err
	}

	sub.Plan = p.Plan
	sub.Interval = p.Interval
	sub.CurrentPeriodEnd = plans.AddPeriod(base, p.Interval)
	sub.UpdatedAt = now
	sub.Status = models.SubStatusActive
	sub.Source = models.SubSourcePayment
	sub.GrantedBy = ""
	sub.Note = ""
	if sub.StartedAt.IsZero() || base.Equal(now) {
		sub.StartedAt = now
	}
	return s.subs.Upsert(ctx, sub)
}

// FailPayment records a failed or cancelled attempt (only if still pending).
func (s *SubscriptionService) FailPayment(ctx context.Context, tranID, status string) (*models.SubscriptionPayment, error) {
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
