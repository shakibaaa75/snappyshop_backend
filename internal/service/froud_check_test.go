package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/fraud"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/repository"
)

type stubCfg struct {
	s        models.FraudSettings
	provider fraud.Provider
}

func (c stubCfg) Load(context.Context) (models.FraudSettings, error) { return c.s, nil }
func (c stubCfg) Provider(context.Context) (fraud.Provider, models.FraudSettings, error) {
	if c.provider == nil {
		return nil, c.s, ErrFraudProviderOff
	}
	return c.provider, c.s, nil
}

type stubProvider struct {
	calls int
	res   *fraud.ProviderResult
	err   error
}

func (p *stubProvider) ID() string { return "stub" }
func (p *stubProvider) Check(context.Context, string) (*fraud.ProviderResult, error) {
	p.calls++
	return p.res, p.err
}

type stubCustomers struct {
	c     *models.FraudCustomer
	saved int
}

func (s *stubCustomers) GetCustomer(context.Context, string) (*models.FraudCustomer, error) {
	if s.c == nil {
		return nil, repository.ErrNotFound
	}
	return s.c, nil
}
func (s *stubCustomers) SaveExternal(_ context.Context, p string, e *models.FraudExternal, _ time.Time) error {
	s.saved++
	if s.c == nil {
		s.c = &models.FraudCustomer{Phone: p}
	}
	s.c.External = e
	return nil
}

type stubOrders struct{ phone string }

func (o stubOrders) Get(_ context.Context, seller, id string) (*models.Order, error) {
	if seller != "s1" {
		return nil, apperr.NotFound()
	}
	return &models.Order{ID: id, SellerID: seller, CustomerPhone: o.phone}, nil
}

func newCheck(cfg stubCfg, cust *stubCustomers, phone string) *FraudCheckService {
	return NewFraudCheckService(cfg, cust, stubOrders{phone}, nil)
}

func on() models.FraudSettings {
	return models.FraudSettings{Enabled: true, ProviderEnabled: true, MinOrders: 3, CacheDays: 7, StoreProviderData: true}
}

func TestCheckUsesOwnDataWhenEnough(t *testing.T) {
	p := &stubProvider{}
	cust := &stubCustomers{c: &models.FraudCustomer{Phone: "01712345678", Delivered: 5, Cancelled: 1}}
	res, err := newCheck(stubCfg{on(), p}, cust, "01712345678").CheckOrder(context.Background(), "s1", "o1")
	if err != nil || p.calls != 0 || res.ExternalStatus != ExtNotNeeded || res.Assessment.Level != fraud.RiskLow {
		t.Fatalf("%v calls=%d %+v", err, p.calls, res)
	}
}

func TestCheckAsksProviderThenReusesSavedAnswer(t *testing.T) {
	p := &stubProvider{res: &fraud.ProviderResult{Provider: "fraudbd", Total: 10, Success: 9, Cancel: 1}}
	cust := &stubCustomers{}
	svc := newCheck(stubCfg{on(), p}, cust, "+8801712345678")
	r1, err := svc.CheckOrder(context.Background(), "s1", "o1")
	if err != nil || r1.ExternalStatus != ExtFresh || cust.saved != 1 || r1.Assessment.Level != fraud.RiskLow {
		t.Fatalf("first: %v %+v saved=%d", err, r1, cust.saved)
	}
	r2, _ := svc.CheckOrder(context.Background(), "s1", "o1")
	if r2.ExternalStatus != ExtSaved || p.calls != 1 {
		t.Fatalf("second should reuse saved answer: %+v calls=%d", r2, p.calls)
	}
}

func TestCheckDoesNotSaveWhenStorageOff(t *testing.T) {
	cfg := on()
	cfg.StoreProviderData = false
	p := &stubProvider{res: &fraud.ProviderResult{Total: 5, Success: 5}}
	cust := &stubCustomers{}
	if _, err := newCheck(stubCfg{cfg, p}, cust, "01712345678").CheckOrder(context.Background(), "s1", "o1"); err != nil || cust.saved != 0 {
		t.Fatalf("err=%v saved=%d", err, cust.saved)
	}
}

func TestCheckProviderFailuresStillAnswer(t *testing.T) {
	cases := map[error]string{fraud.ErrQuota: ExtLimit, fraud.ErrRejected: ExtUnavailable, fraud.ErrUnavailable: ExtUnavailable}
	for perr, want := range cases {
		res, err := newCheck(stubCfg{on(), &stubProvider{err: perr}}, &stubCustomers{}, "01712345678").CheckOrder(context.Background(), "s1", "o1")
		if err != nil || res.ExternalStatus != want || res.Assessment.Level != fraud.RiskUnknown {
			t.Errorf("%v: %v %+v", perr, err, res)
		}
	}
	res, _ := newCheck(stubCfg{on(), nil}, &stubCustomers{}, "01712345678").CheckOrder(context.Background(), "s1", "o1")
	if res.ExternalStatus != ExtOff {
		t.Errorf("no provider: %+v", res)
	}
}

func TestCheckFallsBackToStaleSavedAnswer(t *testing.T) {
	old := &models.FraudExternal{Total: 10, Success: 9, Cancel: 1, CheckedAt: time.Now().Add(-30 * 24 * time.Hour)}
	cust := &stubCustomers{c: &models.FraudCustomer{Phone: "01712345678", External: old}}
	res, _ := newCheck(stubCfg{on(), &stubProvider{err: fraud.ErrUnavailable}}, cust, "01712345678").CheckOrder(context.Background(), "s1", "o1")
	if res.ExternalStatus != ExtSaved || res.External == nil {
		t.Fatalf("%+v", res)
	}
}

func TestCheckGuards(t *testing.T) {
	ctx := context.Background()
	off := on()
	off.Enabled = false
	var ae *apperr.Error
	if _, err := newCheck(stubCfg{off, nil}, &stubCustomers{}, "01712345678").CheckOrder(ctx, "s1", "o1"); !errors.As(err, &ae) || ae.Code != "FRAUD_DISABLED" {
		t.Errorf("disabled: %v", err)
	}
	if _, err := newCheck(stubCfg{on(), nil}, &stubCustomers{}, "123").CheckOrder(ctx, "s1", "o1"); !isValidation(err, "phone") {
		t.Errorf("bad phone: %v", err)
	}
	if _, err := newCheck(stubCfg{on(), nil}, &stubCustomers{}, "01712345678").CheckOrder(ctx, "other-seller", "o1"); !errors.As(err, &ae) || ae.Status != 404 {
		t.Errorf("foreign order: %v", err)
	}
}
