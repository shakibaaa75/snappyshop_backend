package service

import (
	"context"
	"crypto/subtle"
	"errors"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/customdomain"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/repository"
)

const (
	// customDomainCacheTTL is how long a host lookup is remembered. Every
	// request from a custom domain resolves its host, so this keeps the
	// database out of the hot path. A change made on another server instance
	// can take this long to show up.
	customDomainCacheTTL = 30 * time.Second
	// customDomainCacheMax bounds the cache. Anyone can send any Host header,
	// so without a bound random names could grow it forever.
	customDomainCacheMax = 10000
	// customDomainCheckGap stops a seller hammering the DNS check button.
	customDomainCheckGap = 5 * time.Second
)

// CustomDomainGate is satisfied by *SubscriptionService.
type CustomDomainGate interface {
	RequireCustomDomain(ctx context.Context, sellerID string) error
}

// customDomainSlugs is satisfied by *StoreDesignService.
type customDomainSlugs interface {
	CurrentSlugBySeller(ctx context.Context, sellerID string) (string, error)
}

// customDomainChecker is satisfied by *customdomain.Checker.
type customDomainChecker interface {
	Check(ctx context.Context, domain, token string) customdomain.Result
}

// CustomDomainConfig is the platform-side setup sellers point their DNS at.
type CustomDomainConfig struct {
	// PlatformHosts are the platform's own hosts. A seller cannot claim them
	// or anything under them.
	PlatformHosts []string
	// CNAMETarget is what a subdomain's CNAME record should point to.
	CNAMETarget string
	// IPs are what a root domain's A records should point to.
	IPs []string
	// AskToken, when set, must accompany the certificate "ask" request.
	AskToken string
	// Dev is for local testing only: origins with a port (http://shop.com:3000)
	// are accepted. It is never turned on in production.
	Dev bool
}

// CustomDomainRecord is one DNS record the seller must add.
type CustomDomainRecord struct {
	Type    string `json:"type"`    // TXT, CNAME or A
	Name    string `json:"name"`    // the host name the record goes on
	Value   string `json:"value"`   // what to put in it
	Purpose string `json:"purpose"` // "ownership" or "routing"
	Note    string `json:"note,omitempty"`
}

// CustomDomainView is a seller's domain as the API shows it.
type CustomDomainView struct {
	Domain        string               `json:"domain"` // "" when none is set
	Status        string               `json:"status"` // "", "pending" or "verified"
	Verified      bool                 `json:"verified"`
	DNSOK         bool                 `json:"dnsOk"`
	LastError     string               `json:"lastError,omitempty"`
	VerifiedAt    *time.Time           `json:"verifiedAt,omitempty"`
	LastCheckedAt *time.Time           `json:"lastCheckedAt,omitempty"`
	URL           string               `json:"url,omitempty"` // set once verified
	Records       []CustomDomainRecord `json:"records"`
	// PlanAllowed is false when the seller's plan does not include custom
	// domains; PlanMessage says why.
	PlanAllowed bool   `json:"planAllowed"`
	PlanMessage string `json:"planMessage,omitempty"`
}

// CustomDomainTarget is the store a custom domain serves.
type CustomDomainTarget struct {
	Slug   string `json:"slug"`
	Domain string `json:"domain"`
}

type cdCacheEntry struct {
	target CustomDomainTarget
	ok     bool
	exp    time.Time
}

// CustomDomainService lets a seller connect their own domain to their store.
type CustomDomainService struct {
	repo    repository.CustomDomainRepository
	slugs   customDomainSlugs
	checker customDomainChecker
	cfg     CustomDomainConfig
	now     func() time.Time

	mu    sync.RWMutex
	gate  CustomDomainGate // nil = no plan checks
	cache map[string]cdCacheEntry
}

func NewCustomDomainService(repo repository.CustomDomainRepository, slugs customDomainSlugs, checker customDomainChecker, cfg CustomDomainConfig) *CustomDomainService {
	return &CustomDomainService{
		repo: repo, slugs: slugs, checker: checker, cfg: cfg,
		now:   func() time.Time { return time.Now().UTC() },
		cache: make(map[string]cdCacheEntry),
	}
}

// SetGate turns on plan checks.
func (s *CustomDomainService) SetGate(g CustomDomainGate) {
	s.mu.Lock()
	s.gate = g
	s.mu.Unlock()
}

func (s *CustomDomainService) requirePlan(ctx context.Context, sellerID string) error {
	s.mu.RLock()
	g := s.gate
	s.mu.RUnlock()
	if g == nil {
		return nil
	}
	return g.RequireCustomDomain(ctx, sellerID)
}

// Get returns the seller's domain and the DNS records they still need to add.
func (s *CustomDomainService) Get(ctx context.Context, sellerID string) (CustomDomainView, error) {
	d, err := s.repo.GetBySeller(ctx, sellerID)
	if errors.Is(err, repository.ErrNotFound) {
		return s.view(ctx, sellerID, nil), nil
	}
	if err != nil {
		return CustomDomainView{}, err
	}
	return s.view(ctx, sellerID, d), nil
}

// Set connects a domain (replacing any earlier one). The new domain starts
// as pending until the seller proves they own it.
func (s *CustomDomainService) Set(ctx context.Context, sellerID, input string) (CustomDomainView, error) {
	if err := s.requirePlan(ctx, sellerID); err != nil {
		return CustomDomainView{}, err
	}
	domain, err := customdomain.Normalize(input)
	if err != nil {
		return CustomDomainView{}, apperr.Validation(map[string]string{"domain": err.Error()})
	}
	if customdomain.IsPlatformHost(domain, s.cfg.PlatformHosts) {
		return CustomDomainView{}, apperr.Validation(map[string]string{"domain": "That address belongs to this platform and cannot be used as your own domain"})
	}

	old, err := s.repo.GetBySeller(ctx, sellerID)
	if err != nil && !errors.Is(err, repository.ErrNotFound) {
		return CustomDomainView{}, err
	}
	if old != nil && old.Domain == domain {
		return s.view(ctx, sellerID, old), nil // nothing to change
	}

	if taken, err := s.repo.GetVerifiedByDomain(ctx, domain); err == nil && taken.SellerID != sellerID {
		return CustomDomainView{}, apperr.Conflict(apperr.CodeDomainTaken, "This domain is already connected to another store", "domain")
	} else if err != nil && !errors.Is(err, repository.ErrNotFound) {
		return CustomDomainView{}, err
	}

	token, err := customdomain.NewToken()
	if err != nil {
		return CustomDomainView{}, err
	}
	now := s.now()
	d := &models.CustomDomain{
		SellerID: sellerID, Domain: domain, Status: models.DomainPending,
		Token: token, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.repo.Save(ctx, d); err != nil {
		return CustomDomainView{}, err
	}
	if old != nil {
		s.forget(old.Domain)
	}
	s.forget(domain)
	return s.view(ctx, sellerID, d), nil
}

// Verify checks the seller's DNS records. When the ownership record is found
// the domain becomes verified and starts serving the store.
func (s *CustomDomainService) Verify(ctx context.Context, sellerID string) (CustomDomainView, error) {
	if err := s.requirePlan(ctx, sellerID); err != nil {
		return CustomDomainView{}, err
	}
	d, err := s.repo.GetBySeller(ctx, sellerID)
	if errors.Is(err, repository.ErrNotFound) {
		return CustomDomainView{}, apperr.BadRequest("Add a domain first")
	}
	if err != nil {
		return CustomDomainView{}, err
	}
	now := s.now()
	if d.LastCheckedAt != nil && now.Sub(*d.LastCheckedAt) < customDomainCheckGap {
		return s.view(ctx, sellerID, d), nil // checked a moment ago
	}

	res := s.checker.Check(ctx, d.Domain, d.Token)

	// The check took a while. Make sure the seller did not change or remove the
	// domain meanwhile, so we never save a result onto the wrong domain.
	fresh, err := s.repo.GetBySeller(ctx, sellerID)
	if errors.Is(err, repository.ErrNotFound) {
		return CustomDomainView{}, apperr.BadRequest("Add a domain first")
	}
	if err != nil {
		return CustomDomainView{}, err
	}
	if fresh.Domain != d.Domain || fresh.Token != d.Token {
		return s.view(ctx, sellerID, fresh), nil
	}
	d = fresh

	d.LastCheckedAt = &now
	d.UpdatedAt = now
	d.DNSOK = res.Routed
	switch {
	case d.Status == models.DomainVerified:
		d.LastError = "" // ownership was proven once; the TXT record is no longer needed
	case res.Owned:
		d.Status = models.DomainVerified
		d.VerifiedAt = &now
		d.LastError = ""
	default:
		d.LastError = res.Message
	}
	if err := s.repo.Save(ctx, d); err != nil {
		if errors.Is(err, repository.ErrDomainTaken) {
			return CustomDomainView{}, apperr.Conflict(apperr.CodeDomainTaken, "This domain is already connected to another store", "domain")
		}
		return CustomDomainView{}, err
	}
	s.forget(d.Domain)
	return s.view(ctx, sellerID, d), nil
}

// Remove disconnects the seller's domain. Removing when none is set is fine.
func (s *CustomDomainService) Remove(ctx context.Context, sellerID string) error {
	old, err := s.repo.GetBySeller(ctx, sellerID)
	if errors.Is(err, repository.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := s.repo.Delete(ctx, sellerID); err != nil {
		return err
	}
	s.forget(old.Domain)
	return nil
}

// ResolveHost returns the store a request's Host belongs to. Unknown,
// unverified and platform hosts, and sellers whose plan no longer includes
// custom domains, are a 404.
func (s *CustomDomainService) ResolveHost(ctx context.Context, host string) (CustomDomainTarget, error) {
	host = strings.ToLower(strings.TrimSpace(host))
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(host, ".")
	if _, err := customdomain.Normalize(host); err != nil || customdomain.IsPlatformHost(host, s.cfg.PlatformHosts) {
		return CustomDomainTarget{}, apperr.NotFound()
	}

	now := s.now()
	s.mu.RLock()
	e, hit := s.cache[host]
	s.mu.RUnlock()
	if hit && now.Before(e.exp) {
		if e.ok {
			return e.target, nil
		}
		return CustomDomainTarget{}, apperr.NotFound()
	}

	target, ok, err := s.lookup(ctx, host)
	if err != nil {
		return CustomDomainTarget{}, err // not cached: may be a passing database problem
	}
	s.remember(host, target, ok)
	if !ok {
		return CustomDomainTarget{}, apperr.NotFound()
	}
	return target, nil
}

func (s *CustomDomainService) lookup(ctx context.Context, host string) (CustomDomainTarget, bool, error) {
	d, err := s.repo.GetVerifiedByDomain(ctx, host)
	if errors.Is(err, repository.ErrNotFound) {
		// www.<root domain>: the seller proved the root domain, so its www
		// belongs to them too. The target reports the root domain as the
		// canonical host, so the frontend redirects www to it.
		if bare := strings.TrimPrefix(host, "www."); bare != host && customdomain.IsApex(bare) {
			d, err = s.repo.GetVerifiedByDomain(ctx, bare)
		}
	}
	if errors.Is(err, repository.ErrNotFound) {
		return CustomDomainTarget{}, false, nil
	}
	if err != nil {
		return CustomDomainTarget{}, false, err
	}
	if err := s.requirePlan(ctx, d.SellerID); err != nil {
		var ae *apperr.Error
		if errors.As(err, &ae) {
			return CustomDomainTarget{}, false, nil // plan does not include it (any more)
		}
		return CustomDomainTarget{}, false, err
	}
	slug, err := s.slugs.CurrentSlugBySeller(ctx, d.SellerID)
	if err != nil {
		return CustomDomainTarget{}, false, err
	}
	if slug == "" {
		return CustomDomainTarget{}, false, nil
	}
	return CustomDomainTarget{Slug: slug, Domain: d.Domain}, true, nil
}

// OriginAllowed reports whether an Origin header belongs to a connected custom
// domain. The CSRF and CORS checks use it so a store on its own domain can
// place orders and log buyers in. Only a bare https/http origin on the default
// port qualifies.
func (s *CustomDomainService) OriginAllowed(ctx context.Context, origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return false
	}
	if (u.Port() != "" && !s.cfg.Dev) || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	_, err = s.ResolveHost(ctx, u.Hostname())
	return err == nil
}

// TLSAllowed answers the certificate server's question "may I issue a
// certificate for this domain?". Only connected, verified domains qualify, so
// nobody can make us request certificates for names we do not serve.
func (s *CustomDomainService) TLSAllowed(ctx context.Context, domain, token string) bool {
	if s.cfg.AskToken != "" && subtle.ConstantTimeCompare([]byte(token), []byte(s.cfg.AskToken)) != 1 {
		return false
	}
	_, err := s.ResolveHost(ctx, domain)
	return err == nil
}

func (s *CustomDomainService) remember(host string, t CustomDomainTarget, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.cache) >= customDomainCacheMax {
		s.cache = make(map[string]cdCacheEntry)
	}
	s.cache[host] = cdCacheEntry{target: t, ok: ok, exp: s.now().Add(customDomainCacheTTL)}
}

func (s *CustomDomainService) forget(host string) {
	s.mu.Lock()
	delete(s.cache, host)
	s.mu.Unlock()
}

func (s *CustomDomainService) view(ctx context.Context, sellerID string, d *models.CustomDomain) CustomDomainView {
	v := CustomDomainView{Records: []CustomDomainRecord{}, PlanAllowed: true}
	if err := s.requirePlan(ctx, sellerID); err != nil {
		v.PlanAllowed = false
		var ae *apperr.Error
		if errors.As(err, &ae) {
			v.PlanMessage = ae.Message
		}
	}
	if d == nil {
		return v
	}
	v.Domain = d.Domain
	v.Status = d.Status
	v.Verified = d.Status == models.DomainVerified
	v.DNSOK = d.DNSOK
	v.LastError = d.LastError
	v.VerifiedAt = d.VerifiedAt
	v.LastCheckedAt = d.LastCheckedAt
	if v.Verified {
		v.URL = "https://" + d.Domain
	}
	v.Records = s.records(d)
	return v
}

func (s *CustomDomainService) records(d *models.CustomDomain) []CustomDomainRecord {
	var out []CustomDomainRecord
	if d.Status != models.DomainVerified {
		out = append(out, CustomDomainRecord{
			Type: "TXT", Name: customdomain.TXTName(d.Domain), Value: customdomain.TXTValue(d.Token),
			Purpose: "ownership", Note: "Proves the domain is yours. Add this first.",
		})
	}
	target := strings.TrimSuffix(strings.TrimSpace(s.cfg.CNAMETarget), ".")
	apex := customdomain.IsApex(d.Domain)
	// A root domain cannot have a CNAME, so it only gets A records. A
	// subdomain gets the CNAME.
	if target != "" && !apex {
		out = append(out, CustomDomainRecord{
			Type: "CNAME", Name: d.Domain, Value: target, Purpose: "routing",
			Note: "Use this for a subdomain such as shop or www.",
		})
	}
	for _, ip := range s.cfg.IPs {
		out = append(out, CustomDomainRecord{
			Type: "A", Name: d.Domain, Value: ip, Purpose: "routing",
			Note: "Use this for a root domain such as yourbrand.com.",
		})
	}
	// Root domains also answer on www, which redirects to the root domain.
	if target != "" && apex {
		out = append(out, CustomDomainRecord{
			Type: "CNAME", Name: "www." + d.Domain, Value: target, Purpose: "routing",
			Note: "Optional. Makes www." + d.Domain + " open your store too.",
		})
	}
	if out == nil {
		out = []CustomDomainRecord{}
	}
	return out
}
