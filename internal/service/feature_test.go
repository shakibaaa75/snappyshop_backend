package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/payment/sslcommerz"
	"shopbd/backend/internal/repository"
)

type fakeEnts struct {
	mu   sync.Mutex
	rows map[string]*models.FeatureEntitlement
}

func (f *fakeEnts) key(s, feat string) string { return s + "|" + feat }
func (f *fakeEnts) Get(_ context.Context, s, feat string) (*models.FeatureEntitlement, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if e, ok := f.rows[f.key(s, feat)]; ok {
		c := *e
		return &c, nil
	}
	return nil, repository.ErrNotFound
}
func (f *fakeEnts) Upsert(_ context.Context, e *models.FeatureEntitlement) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := *e
	f.rows[f.key(e.SellerID, e.Feature)] = &c
	return nil
}
func (f *fakeEnts) ListBySeller(context.Context, string) ([]models.FeatureEntitlement, error) {
	return nil, nil
}

type fakePays struct {
	mu   sync.Mutex
	rows map[string]*models.FeaturePayment
}

func (f *fakePays) Create(_ context.Context, p *models.FeaturePayment) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := *p
	f.rows[p.TranID] = &c
	return nil
}
func (f *fakePays) FindByTranID(_ context.Context, t string) (*models.FeaturePayment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p, ok := f.rows[t]; ok {
		c := *p
		return &c, nil
	}
	return nil, repository.ErrNotFound
}
func (f *fakePays) MarkPaid(_ context.Context, t, val string, at time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.rows[t]
	if !ok || p.Status == models.SubPayPaid {
		return false, nil
	}
	p.Status, p.ValID, p.PaidAt = models.SubPayPaid, val, at
	return true, nil
}
func (f *fakePays) ResetPaid(_ context.Context, t string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p, ok := f.rows[t]; ok && p.Status == models.SubPayPaid {
		p.Status = models.SubPayPending
	}
	return nil
}
func (f *fakePays) MarkNotPaid(_ context.Context, t, st string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p, ok := f.rows[t]; ok && p.Status == models.SubPayPending {
		p.Status = st
	}
	return nil
}
func (f *fakePays) ListBySeller(context.Context, string, string, int) ([]models.FeaturePayment, error) {
	return nil, nil
}

type fakeGW struct{ val *sslcommerz.Validation }

func (g fakeGW) Init(context.Context, sslcommerz.InitInput) (string, error) {
	return "https://pay.example/x", nil
}
func (g fakeGW) Validate(context.Context, string) (*sslcommerz.Validation, error) {
	return g.val, nil
}

func newFeatureSvc(gw PaymentGateway) (*FeatureService, *fakeEnts) {
	ents := &fakeEnts{rows: map[string]*models.FeatureEntitlement{}}
	pays := &fakePays{rows: map[string]*models.FeaturePayment{}}
	svc := NewFeatureService(ents, pays, gw,
		FeatureCatalog{models.FeatureTryTheLook: {Name: "Try the Look", Price: 999}},
		SubscriptionConfig{PublicAPIURL: "http://api", FrontendURL: "http://web"}, nil, nil)
	return svc, ents
}

func code(err error) string {
	var ae *apperr.Error
	if errors.As(err, &ae) {
		return ae.Code
	}
	return ""
}

func TestRequireLockedByDefault(t *testing.T) {
	svc, _ := newFeatureSvc(fakeGW{})
	err := svc.Require(context.Background(), "s1", models.FeatureTryTheLook)
	if code(err) != "FEATURE_LOCKED" {
		t.Fatalf("want FEATURE_LOCKED, got %v", err)
	}
}

func TestAdminGrantDisableEnableRevoke(t *testing.T) {
	ctx := context.Background()
	svc, _ := newFeatureSvc(fakeGW{})
	f := models.FeatureTryTheLook

	if _, err := svc.AdminGrant(ctx, "a1", "s1", f, "promo", ""); err != nil {
		t.Fatal(err)
	}
	if err := svc.Require(ctx, "s1", f); err != nil {
		t.Fatalf("granted seller should have access: %v", err)
	}
	if _, err := svc.AdminSetStatus(ctx, "a1", "s1", f, models.FeatureDisabled, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := svc.Require(ctx, "s1", f); code(err) != "FEATURE_LOCKED" {
		t.Fatalf("disabled seller must be locked, got %v", err)
	}
	if _, err := svc.AdminGrant(ctx, "a1", "s1", f, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := svc.Require(ctx, "s1", f); err != nil {
		t.Fatalf("re-enabled seller should have access: %v", err)
	}
	if _, err := svc.AdminSetStatus(ctx, "a1", "s1", f, models.FeatureRevoked, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := svc.Require(ctx, "s1", f); code(err) != "FEATURE_LOCKED" {
		t.Fatalf("revoked seller must be locked, got %v", err)
	}
	if _, err := svc.AdminSetStatus(ctx, "a1", "s1", f, "bogus", "", ""); err == nil {
		t.Fatal("bogus status must be rejected")
	}
	if _, err := svc.AdminSetStatus(ctx, "a1", "nobody", f, models.FeatureDisabled, "", ""); err == nil {
		t.Fatal("disabling a feature the seller never had must fail")
	}
}

func TestUnknownFeatureIsNotFound(t *testing.T) {
	svc, _ := newFeatureSvc(fakeGW{})
	if _, err := svc.Get(context.Background(), "s1", "nope"); code(err) != apperr.CodeNotFound {
		t.Fatalf("want NOT_FOUND, got %v", err)
	}
}

func TestCheckoutBlocksWhenOwnedOrBlocked(t *testing.T) {
	ctx := context.Background()
	svc, _ := newFeatureSvc(fakeGW{})
	f := models.FeatureTryTheLook
	in := FeatureCheckoutInput{SellerID: "s1", Feature: f, Method: "bkash", Slug: "my-shop"}

	if _, err := svc.Checkout(ctx, in); err != nil {
		t.Fatalf("first checkout should work: %v", err)
	}
	_, _ = svc.AdminGrant(ctx, "a1", "s1", f, "", "")
	if _, err := svc.Checkout(ctx, in); code(err) != "ALREADY_UNLOCKED" {
		t.Fatalf("want ALREADY_UNLOCKED, got %v", err)
	}
	_, _ = svc.AdminSetStatus(ctx, "a1", "s1", f, models.FeatureRevoked, "", "")
	if _, err := svc.Checkout(ctx, in); code(err) != "FEATURE_BLOCKED" {
		t.Fatalf("want FEATURE_BLOCKED, got %v", err)
	}
}

func TestCheckoutValidatesInput(t *testing.T) {
	svc, _ := newFeatureSvc(fakeGW{})
	_, err := svc.Checkout(context.Background(), FeatureCheckoutInput{
		SellerID: "s1", Feature: models.FeatureTryTheLook, Method: "cash", Slug: "BAD SLUG"})
	if code(err) != apperr.CodeValidationFailed {
		t.Fatalf("want VALIDATION_FAILED, got %v", err)
	}
}

func TestTranIDLength(t *testing.T) {
	id, err := newFeatureTranID()
	if err != nil || len(id) != 27 || id[:3] != "FTR" {
		t.Fatalf("bad tran id %q (%v)", id, err)
	}
}

func sslValidation(tran, amount string) *sslcommerz.Validation {
	return &sslcommerz.Validation{
		Status: "VALID", TranID: tran, ValID: "v1",
		Amount: sslcommerz.Flex(amount), Currency: "BDT", CurrencyType: "BDT",
	}
}

// startPayment runs Checkout and returns the tran id it stored.
func startPayment(t *testing.T, svc *FeatureService) string {
	t.Helper()
	if _, err := svc.Checkout(context.Background(), FeatureCheckoutInput{
		SellerID: "s1", Feature: models.FeatureTryTheLook, Method: "card", Slug: "my-shop",
	}); err != nil {
		t.Fatal(err)
	}
	pays := svc.pays.(*fakePays)
	for tran := range pays.rows {
		return tran
	}
	t.Fatal("no payment stored")
	return ""
}

func TestConfirmPaymentUnlocksOnceAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	gw := &swapGW{}
	svc, ents := newFeatureSvc(gw)
	tran := startPayment(t, svc)
	gw.val = sslValidation(tran, "999.00")

	if _, err := svc.ConfirmPayment(ctx, tran, "v1"); err != nil {
		t.Fatal(err)
	}
	e, err := ents.Get(ctx, "s1", models.FeatureTryTheLook)
	if err != nil || !e.IsActive() || e.Source != models.FeatureSourcePurchase || e.Amount != 999 || e.PurchasedAt == nil {
		t.Fatalf("bad entitlement after purchase: %+v (%v)", e, err)
	}
	// Redirect + IPN both arrive: the second call must be a harmless no-op.
	if _, err := svc.ConfirmPayment(ctx, tran, "v1"); err != nil {
		t.Fatal(err)
	}
}

func TestConfirmPaymentRejectsWrongAmountOrTran(t *testing.T) {
	ctx := context.Background()
	gw := &swapGW{}
	svc, _ := newFeatureSvc(gw)
	tran := startPayment(t, svc)

	gw.val = sslValidation(tran, "1.00") // underpaid
	if _, err := svc.ConfirmPayment(ctx, tran, "v1"); err == nil {
		t.Fatal("underpayment must not unlock the feature")
	}
	gw.val = sslValidation("SOMEONE_ELSE", "999.00") // another payment's validation
	if _, err := svc.ConfirmPayment(ctx, tran, "v1"); err == nil {
		t.Fatal("mismatched tran id must not unlock the feature")
	}
	if ok, _ := svc.HasAccess(ctx, "s1", models.FeatureTryTheLook); ok {
		t.Fatal("seller must still be locked")
	}
}

func TestPurchaseKeepsActiveWhenAdminGrantedMeanwhile(t *testing.T) {
	ctx := context.Background()
	gw := &swapGW{}
	svc, ents := newFeatureSvc(gw)
	tran := startPayment(t, svc)
	_, _ = svc.AdminGrant(ctx, "a1", "s1", models.FeatureTryTheLook, "", "")
	gw.val = sslValidation(tran, "999")
	if _, err := svc.ConfirmPayment(ctx, tran, "v1"); err != nil {
		t.Fatal(err)
	}
	e, _ := ents.Get(ctx, "s1", models.FeatureTryTheLook)
	if !e.IsActive() || e.Source != models.FeatureSourcePurchase {
		t.Fatalf("purchase should be recorded on the existing row: %+v", e)
	}
}

// swapGW lets a test change what the gateway answers after checkout.
type swapGW struct{ val *sslcommerz.Validation }

func (g *swapGW) Init(context.Context, sslcommerz.InitInput) (string, error) {
	return "https://pay.example/x", nil
}
func (g *swapGW) Validate(context.Context, string) (*sslcommerz.Validation, error) {
	return g.val, nil
}
