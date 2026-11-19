package service

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/fraud"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/repository"
)

type fraudConfigSource interface {
	Load(ctx context.Context) (models.FraudSettings, error)
	Provider(ctx context.Context) (fraud.Provider, models.FraudSettings, error)
}

type fraudCustomers interface {
	GetCustomer(ctx context.Context, phone string) (*models.FraudCustomer, error)
	SaveExternal(ctx context.Context, phone string, ext *models.FraudExternal, at time.Time) error
}

type fraudOrderLookup interface {
	Get(ctx context.Context, sellerID, id string) (*models.Order, error)
}

// How the third-party part of an answer was obtained.
const (
	ExtNotNeeded   = ""            // our own history was enough
	ExtFresh       = "fresh"       // just asked the provider
	ExtSaved       = "saved"       // reused a recent saved answer
	ExtOff         = "off"         // no provider configured
	ExtLimit       = "limit"       // provider usage limit reached
	ExtUnavailable = "unavailable" // provider down or key problem (admin is told via logs)
)

// FraudInternal is our platform-wide history for the phone. It carries no
// seller names or order details, only counts.
type FraudInternal struct {
	Delivered, Cancelled, Failed, Open int
}

// FraudCheckResult is what the seller sees.
type FraudCheckResult struct {
	Phone          string
	Internal       *FraudInternal // nil = never seen on this platform
	External       *models.FraudExternal
	ExternalStatus string
	Assessment     fraud.Assessment
	CheckedAt      time.Time
}

// FraudCheckService answers "is this customer risky?" for sellers.
type FraudCheckService struct {
	cfg       fraudConfigSource
	customers fraudCustomers
	orders    fraudOrderLookup
	saved     repository.FraudCheckStore // nil = results are not kept
	log       *slog.Logger
	now       func() time.Time
}

func NewFraudCheckService(cfg fraudConfigSource, customers fraudCustomers, orders fraudOrderLookup, log *slog.Logger) *FraudCheckService {
	if log == nil {
		log = slog.Default()
	}
	s := &FraudCheckService{cfg: cfg, customers: customers, orders: orders, log: log,
		now: func() time.Time { return time.Now().UTC() }}
	// The Mongo history repository also keeps each order's last result.
	if st, ok := customers.(repository.FraudCheckStore); ok {
		s.saved = st
	}
	return s
}

// Saved returns the last check result of the seller's order, or nil if it was
// never checked. It costs nothing: no provider call is made.
func (s *FraudCheckService) Saved(ctx context.Context, sellerID, orderID string) (*FraudCheckResult, error) {
	if s.saved == nil {
		return nil, nil
	}
	cfg, err := s.cfg.Load(ctx)
	if err != nil {
		return nil, err
	}
	if !cfg.Enabled {
		return nil, errFraudOff()
	}
	c, err := s.saved.GetCheck(ctx, sellerID, orderID)
	if errors.Is(err, repository.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	res := &FraudCheckResult{
		Phone: c.Phone, External: c.External, ExternalStatus: c.ExternalStatus, CheckedAt: c.CheckedAt,
		Assessment: fraud.Assessment{Level: c.RiskLevel, Sample: c.Sample, SuccessRate: c.SuccessRate, Reasons: c.Reasons},
	}
	if c.Internal != nil {
		res.Internal = &FraudInternal{c.Internal.Delivered, c.Internal.Cancelled, c.Internal.Failed, c.Internal.Open}
	}
	return res, nil
}

func (s *FraudCheckService) save(ctx context.Context, sellerID, orderID string, res *FraudCheckResult) {
	if s.saved == nil {
		return
	}
	c := &models.FraudCheckSnapshot{
		OrderID: orderID, SellerID: sellerID, Phone: res.Phone,
		External: res.External, ExternalStatus: res.ExternalStatus,
		RiskLevel: res.Assessment.Level, Sample: res.Assessment.Sample,
		SuccessRate: res.Assessment.SuccessRate, Reasons: res.Assessment.Reasons,
		CheckedAt: res.CheckedAt,
	}
	if in := res.Internal; in != nil {
		c.Internal = &models.FraudCounts{Delivered: in.Delivered, Cancelled: in.Cancelled, Failed: in.Failed, Open: in.Open}
	}
	if err := s.saved.SaveCheck(ctx, c); err != nil {
		s.log.Error("fraud check result not saved", "order", orderID, "err", err)
	}
}

// Enabled tells the seller dashboard whether to show the feature at all.
func (s *FraudCheckService) Enabled(ctx context.Context) (bool, error) {
	c, err := s.cfg.Load(ctx)
	return c.Enabled, err
}

func errFraudOff() error {
	return apperr.New(http.StatusForbidden, "FRAUD_DISABLED", "Fraud check is not available right now")
}

// CheckOrder checks the customer of one of the seller's own orders. Taking
// the order (not a free phone number) means a seller can only look up people
// who actually ordered from them.
func (s *FraudCheckService) CheckOrder(ctx context.Context, sellerID, orderID string) (*FraudCheckResult, error) {
	cfg, err := s.cfg.Load(ctx)
	if err != nil {
		return nil, err
	}
	if !cfg.Enabled {
		return nil, errFraudOff()
	}
	order, err := s.orders.Get(ctx, sellerID, orderID)
	if err != nil {
		return nil, err
	}
	phone, ok := fraud.NormalizePhone(order.CustomerPhone)
	if !ok {
		return nil, apperr.Validation(map[string]string{"phone": "This order's phone number is not a valid Bangladeshi mobile number"})
	}

	// 1. Our own database first.
	cust, err := s.customers.GetCustomer(ctx, phone)
	if errors.Is(err, repository.ErrNotFound) {
		cust, err = nil, nil
	}
	if err != nil {
		return nil, err
	}

	res := &FraudCheckResult{Phone: phone}
	in := fraud.AssessInput{}
	if cust != nil {
		res.Internal = &FraudInternal{cust.Delivered, cust.Cancelled, cust.Failed, cust.Open}
		in.Delivered, in.Cancelled, in.Failed = cust.Delivered, cust.Cancelled, cust.Failed
	}

	// 2. Enough of our own data? Then we are done and no provider quota is spent.
	if cust == nil || cust.Finished() < cfg.MinOrders {
		// 3. Otherwise a recent saved answer, or ask the provider.
		ext, status := s.external(ctx, cfg, phone, cust)
		res.External, res.ExternalStatus = ext, status
		if ext != nil {
			in.HasExternal = true
			in.ExtTotal, in.ExtSuccess, in.ExtCancel = ext.Total, ext.Success, ext.Cancel
			in.ExtRating, in.ExtRisk = ext.Rating, ext.RiskLevel
		}
	}
	res.Assessment = fraud.Assess(in)
	res.CheckedAt = s.now()
	s.save(ctx, sellerID, orderID, res)
	return res, nil
}

func (s *FraudCheckService) external(ctx context.Context, cfg models.FraudSettings, phone string, cust *models.FraudCustomer) (*models.FraudExternal, string) {
	var saved *models.FraudExternal
	if cust != nil {
		saved = cust.External
	}
	if saved != nil && cfg.CacheDays > 0 && s.now().Sub(saved.CheckedAt) < time.Duration(cfg.CacheDays)*24*time.Hour {
		return saved, ExtSaved
	}

	provider, _, err := s.cfg.Provider(ctx)
	if err != nil {
		return saved, orStatus(saved, ExtOff) // an old saved answer beats none
	}
	callCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	r, err := provider.Check(callCtx, phone)
	if err != nil {
		status := ExtUnavailable
		switch {
		case errors.Is(err, fraud.ErrQuota):
			status = ExtLimit
		case errors.Is(err, fraud.ErrRejected):
			// Sellers can do nothing about this; the admin needs to see it.
			s.log.Error("fraud provider rejected the saved API key")
		}
		return saved, orStatus(saved, status)
	}

	ext := &models.FraudExternal{
		Provider: r.Provider, Total: r.Total, Success: r.Success, Cancel: r.Cancel,
		Rating: r.Rating, RiskLevel: r.RiskLevel, CheckedAt: s.now(),
	}
	if cfg.StoreProviderData {
		if err := s.customers.SaveExternal(ctx, phone, ext, ext.CheckedAt); err != nil {
			s.log.Error("saving fraud provider answer failed", "err", err)
		}
	}
	return ext, ExtFresh
}

// orStatus: when an older saved answer is used instead, say so.
func orStatus(saved *models.FraudExternal, status string) string {
	if saved != nil {
		return ExtSaved
	}
	return status
}
