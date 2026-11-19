package service

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/customdomain"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/repository"
)

/* ---- fakes ---- */

// memDomains behaves like the Mongo repository, including the rule that only
// one seller can hold a VERIFIED row for a domain.
type memDomains struct {
	mu   sync.Mutex
	rows map[string]models.CustomDomain
}

func newMemDomains() *memDomains { return &memDomains{rows: map[string]models.CustomDomain{}} }

func (m *memDomains) GetBySeller(_ context.Context, id string) (*models.CustomDomain, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[id]
	if !ok {
		return nil, repository.ErrNotFound
	}
	return &r, nil
}

func (m *memDomains) GetVerifiedByDomain(_ context.Context, d string) (*models.CustomDomain, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.rows {
		if r.Domain == d && r.Status == models.DomainVerified {
			c := r
			return &c, nil
		}
	}
	return nil, repository.ErrNotFound
}

func (m *memDomains) Save(_ context.Context, d *models.CustomDomain) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if d.Status == models.DomainVerified {
		for id, r := range m.rows {
			if id != d.SellerID && r.Domain == d.Domain && r.Status == models.DomainVerified {
				return repository.ErrDomainTaken
			}
		}
	}
	m.rows[d.SellerID] = *d
	return nil
}

func (m *memDomains) Delete(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.rows, id)
	return nil
}

type fakeChecker struct {
	owned, routed bool
	calls         int
}

func (f *fakeChecker) Check(context.Context, string, string) customdomain.Result {
	f.calls++
	r := customdomain.Result{Owned: f.owned, Routed: f.routed}
	if !f.owned {
		r.Message = "not found yet"
	}
	return r
}

type fakeSlugs map[string]string

func (f fakeSlugs) CurrentSlugBySeller(_ context.Context, id string) (string, error) {
	return f[id], nil
}

type fakeGate struct{ deny map[string]bool }

func (g *fakeGate) RequireCustomDomain(_ context.Context, id string) error {
	if g.deny[id] {
		return apperr.New(http.StatusForbidden, "PLAN_LIMIT", "Custom domains are available on the Business plan.")
	}
	return nil
}

type cdEnv struct {
	svc   *CustomDomainService
	repo  *memDomains
	chk   *fakeChecker
	gate  *fakeGate
	clock *time.Time
}

func newCDEnv() *cdEnv {
	clock := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	e := &cdEnv{
		repo:  newMemDomains(),
		chk:   &fakeChecker{},
		gate:  &fakeGate{deny: map[string]bool{}},
		clock: &clock,
	}
	e.svc = NewCustomDomainService(e.repo, fakeSlugs{"s1": "alpha", "s2": "beta"}, e.chk, CustomDomainConfig{
		PlatformHosts: []string{"shopbd.com"},
		CNAMETarget:   "stores.shopbd.com.",
		IPs:           []string{"198.51.100.7"},
		AskToken:      "secret",
	})
	e.svc.SetGate(e.gate)
	e.svc.now = func() time.Time { return *e.clock }
	return e
}

func (e *cdEnv) advance(d time.Duration) { *e.clock = e.clock.Add(d) }

func isCode(err error, status int, code string) bool {
	var ae *apperr.Error
	return errors.As(err, &ae) && ae.Status == status && ae.Code == code
}

func (e *cdEnv) verified(t *testing.T, seller, domain string) {
	t.Helper()
	ctx := context.Background()
	if _, err := e.svc.Set(ctx, seller, domain); err != nil {
		t.Fatalf("Set: %v", err)
	}
	e.chk.owned, e.chk.routed = true, true
	v, err := e.svc.Verify(ctx, seller)
	if err != nil || !v.Verified {
		t.Fatalf("Verify: %+v, %v", v, err)
	}
	e.advance(time.Minute)
}

/* ---- tests ---- */

func TestSetStartsPendingWithRecords(t *testing.T) {
	e := newCDEnv()
	v, err := e.svc.Set(context.Background(), "s1", "https://Shop.Brand.com/")
	if err != nil {
		t.Fatal(err)
	}
	if v.Domain != "shop.brand.com" || v.Status != models.DomainPending || v.Verified || v.URL != "" {
		t.Fatalf("unexpected view: %+v", v)
	}
	var txt, cname, a *CustomDomainRecord
	for i := range v.Records {
		switch v.Records[i].Type {
		case "TXT":
			txt = &v.Records[i]
		case "CNAME":
			cname = &v.Records[i]
		case "A":
			a = &v.Records[i]
		}
	}
	if txt == nil || txt.Name != "_shopbd-verify.shop.brand.com" || txt.Purpose != "ownership" {
		t.Errorf("TXT record wrong: %+v", txt)
	}
	if cname == nil || cname.Value != "stores.shopbd.com" {
		t.Errorf("CNAME record wrong (trailing dot must go): %+v", cname)
	}
	if a == nil || a.Value != "198.51.100.7" {
		t.Errorf("A record wrong: %+v", a)
	}
}

func TestSetRejectsBadAndPlatformDomains(t *testing.T) {
	e := newCDEnv()
	for _, in := range []string{"", "localhost", "10.0.0.1", "shopbd.com", "www.shopbd.com", "my_shop.com"} {
		_, err := e.svc.Set(context.Background(), "s1", in)
		if !isCode(err, http.StatusUnprocessableEntity, apperr.CodeValidationFailed) {
			t.Errorf("Set(%q) err = %v; want a validation error", in, err)
		}
	}
}

func TestSetRequiresPlan(t *testing.T) {
	e := newCDEnv()
	e.gate.deny["s1"] = true
	_, err := e.svc.Set(context.Background(), "s1", "shop.brand.com")
	if !isCode(err, http.StatusForbidden, "PLAN_LIMIT") {
		t.Fatalf("err = %v; want PLAN_LIMIT", err)
	}
	// Reading still works and explains the lock.
	v, err := e.svc.Get(context.Background(), "s1")
	if err != nil || v.PlanAllowed || v.PlanMessage == "" {
		t.Fatalf("Get on locked plan: %+v, %v", v, err)
	}
}

func TestGetWithNothingSet(t *testing.T) {
	e := newCDEnv()
	v, err := e.svc.Get(context.Background(), "s1")
	if err != nil || v.Domain != "" || v.Records == nil || !v.PlanAllowed {
		t.Fatalf("Get = %+v, %v", v, err)
	}
}

func TestVerifyFlow(t *testing.T) {
	e := newCDEnv()
	ctx := context.Background()
	if _, err := e.svc.Verify(ctx, "s1"); !isCode(err, http.StatusBadRequest, apperr.CodeBadRequest) {
		t.Fatalf("Verify with no domain: %v", err)
	}
	e.svc.Set(ctx, "s1", "shop.brand.com")

	// Record not there yet.
	v, err := e.svc.Verify(ctx, "s1")
	if err != nil || v.Verified || v.LastError == "" || v.LastCheckedAt == nil {
		t.Fatalf("first check: %+v, %v", v, err)
	}
	if e.chk.calls != 1 {
		t.Fatalf("calls = %d", e.chk.calls)
	}

	// Pressing again straight away does not query DNS again.
	if _, err := e.svc.Verify(ctx, "s1"); err != nil || e.chk.calls != 1 {
		t.Fatalf("cooldown not applied: calls=%d err=%v", e.chk.calls, err)
	}

	// Record appears.
	e.advance(10 * time.Second)
	e.chk.owned, e.chk.routed = true, false
	v, err = e.svc.Verify(ctx, "s1")
	if err != nil || !v.Verified || v.URL != "https://shop.brand.com" || v.LastError != "" || v.VerifiedAt == nil {
		t.Fatalf("after record: %+v, %v", v, err)
	}
	if v.DNSOK {
		t.Error("DNSOK should follow the routing check")
	}
	for _, r := range v.Records {
		if r.Type == "TXT" {
			t.Error("the ownership record is no longer needed once verified")
		}
	}

	// Removing the TXT record later does not un-verify it.
	e.advance(10 * time.Second)
	e.chk.owned = false
	if v, _ = e.svc.Verify(ctx, "s1"); !v.Verified {
		t.Error("a verified domain must stay verified")
	}
}

func TestOnlyOneSellerCanOwnADomain(t *testing.T) {
	e := newCDEnv()
	ctx := context.Background()

	// Both sellers add it; nobody is blocked while pending.
	if _, err := e.svc.Set(ctx, "s1", "shop.brand.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Set(ctx, "s2", "shop.brand.com"); err != nil {
		t.Fatalf("a pending claim must not block another seller: %v", err)
	}

	// s2 proves ownership first.
	e.chk.owned = true
	if v, err := e.svc.Verify(ctx, "s2"); err != nil || !v.Verified {
		t.Fatalf("s2 verify: %+v, %v", v, err)
	}
	// s1 can no longer verify it.
	e.advance(time.Minute)
	if _, err := e.svc.Verify(ctx, "s1"); !isCode(err, http.StatusConflict, apperr.CodeDomainTaken) {
		t.Fatalf("s1 verify err = %v; want DOMAIN_TAKEN", err)
	}
	// And a new claim is refused outright.
	if _, err := e.svc.Set(ctx, "s3", "shop.brand.com"); !isCode(err, http.StatusConflict, apperr.CodeDomainTaken) {
		t.Fatalf("Set on a taken domain err = %v", err)
	}
}

func TestResolveHost(t *testing.T) {
	e := newCDEnv()
	ctx := context.Background()

	if _, err := e.svc.ResolveHost(ctx, "shop.brand.com"); !isCode(err, http.StatusNotFound, apperr.CodeNotFound) {
		t.Fatalf("unknown host: %v", err)
	}
	e.svc.Set(ctx, "s1", "shop.brand.com")
	if _, err := e.svc.ResolveHost(ctx, "shop.brand.com"); err == nil {
		t.Fatal("a pending domain must not serve the store")
	}
	e.advance(time.Minute)
	e.chk.owned = true
	e.svc.Verify(ctx, "s1")

	for _, h := range []string{"shop.brand.com", "SHOP.Brand.com", "shop.brand.com:443", "shop.brand.com."} {
		got, err := e.svc.ResolveHost(ctx, h)
		if err != nil || got.Slug != "alpha" || got.Domain != "shop.brand.com" {
			t.Errorf("ResolveHost(%q) = %+v, %v", h, got, err)
		}
	}
	for _, h := range []string{"", "127.0.0.1", "localhost", "shopbd.com", "x.shopbd.com", "other.brand.com"} {
		if _, err := e.svc.ResolveHost(ctx, h); err == nil {
			t.Errorf("ResolveHost(%q) should fail", h)
		}
	}
}

func TestResolveFollowsChanges(t *testing.T) {
	e := newCDEnv()
	ctx := context.Background()
	e.verified(t, "s1", "shop.brand.com")
	if _, err := e.svc.ResolveHost(ctx, "shop.brand.com"); err != nil {
		t.Fatal(err)
	}

	// Switching domains takes the old one offline at once (cache is cleared).
	e.svc.Set(ctx, "s1", "new.brand.com")
	if _, err := e.svc.ResolveHost(ctx, "shop.brand.com"); err == nil {
		t.Error("old domain still serves after the seller changed it")
	}

	// Removing it works the same way.
	e.verified(t, "s1", "back.brand.com")
	if _, err := e.svc.ResolveHost(ctx, "back.brand.com"); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Remove(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.ResolveHost(ctx, "back.brand.com"); err == nil {
		t.Error("removed domain still serves")
	}
	if err := e.svc.Remove(ctx, "s1"); err != nil {
		t.Errorf("removing twice should be fine: %v", err)
	}
}

func TestResolveStopsWhenPlanLapses(t *testing.T) {
	e := newCDEnv()
	ctx := context.Background()
	e.verified(t, "s1", "shop.brand.com")
	if _, err := e.svc.ResolveHost(ctx, "shop.brand.com"); err != nil {
		t.Fatal(err)
	}
	e.gate.deny["s1"] = true
	e.advance(customDomainCacheTTL + time.Second) // cached answer expires
	if _, err := e.svc.ResolveHost(ctx, "shop.brand.com"); err == nil {
		t.Error("a seller who dropped the plan must stop serving the domain")
	}
	// The domain is kept, so upgrading again brings it back.
	e.gate.deny["s1"] = false
	e.advance(customDomainCacheTTL + time.Second)
	if _, err := e.svc.ResolveHost(ctx, "shop.brand.com"); err != nil {
		t.Errorf("domain should return after upgrading: %v", err)
	}
}

func TestResolveNeedsAStore(t *testing.T) {
	e := newCDEnv()
	e.verified(t, "s9", "shop.brand.com") // s9 has no store address
	if _, err := e.svc.ResolveHost(context.Background(), "shop.brand.com"); err == nil {
		t.Error("a seller without a store cannot serve a domain")
	}
}

func TestOriginAllowed(t *testing.T) {
	e := newCDEnv()
	ctx := context.Background()
	e.verified(t, "s1", "shop.brand.com")

	yes := []string{"https://shop.brand.com", "http://shop.brand.com", "https://shop.brand.com/", "https://SHOP.brand.com"}
	no := []string{
		"", "null", "https://evil.com", "https://shop.brand.com.evil.com",
		"https://shop.brand.com:8443", "https://user@shop.brand.com", "https://shop.brand.com/path",
		"ftp://shop.brand.com", "https://shopbd.com",
	}
	for _, o := range yes {
		if !e.svc.OriginAllowed(ctx, o) {
			t.Errorf("OriginAllowed(%q) = false", o)
		}
	}
	for _, o := range no {
		if e.svc.OriginAllowed(ctx, o) {
			t.Errorf("OriginAllowed(%q) = true", o)
		}
	}
}

func TestTLSAllowed(t *testing.T) {
	e := newCDEnv()
	ctx := context.Background()
	e.verified(t, "s1", "shop.brand.com")

	if !e.svc.TLSAllowed(ctx, "shop.brand.com", "secret") {
		t.Error("verified domain with the right token must be allowed")
	}
	if e.svc.TLSAllowed(ctx, "shop.brand.com", "") || e.svc.TLSAllowed(ctx, "shop.brand.com", "wrong") {
		t.Error("a missing or wrong token must be refused")
	}
	if e.svc.TLSAllowed(ctx, "random.brand.com", "secret") {
		t.Error("an unknown domain must be refused")
	}

	// Without a configured token the check is the domain alone.
	e.svc.cfg.AskToken = ""
	if !e.svc.TLSAllowed(ctx, "shop.brand.com", "") {
		t.Error("no token configured: verified domain should pass")
	}
}

func TestCacheStaysBounded(t *testing.T) {
	e := newCDEnv()
	for i := 0; i < customDomainCacheMax+50; i++ {
		e.svc.remember("h"+time.Duration(i).String(), CustomDomainTarget{}, false)
	}
	e.svc.mu.RLock()
	n := len(e.svc.cache)
	e.svc.mu.RUnlock()
	if n > customDomainCacheMax {
		t.Fatalf("cache grew to %d", n)
	}
}
