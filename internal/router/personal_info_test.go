package router

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"shopbd/backend/internal/config"
	"shopbd/backend/internal/mailer"
	"shopbd/backend/internal/otp"
	"shopbd/backend/internal/password"
	"shopbd/backend/internal/repository/memrepo"
	"shopbd/backend/internal/service"
	"shopbd/backend/internal/sms"
)

type routeMail struct {
	mu   sync.Mutex
	sent []string // "to|text"
}

func (m *routeMail) Send(_ context.Context, to, _, _, text string) error {
	m.mu.Lock()
	m.sent = append(m.sent, to+"|"+text)
	m.mu.Unlock()
	return nil
}

func (m *routeMail) last() (to, text string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	parts := strings.SplitN(m.sent[len(m.sent)-1], "|", 2)
	return parts[0], parts[1]
}

type routeSMS struct {
	mu   sync.Mutex
	last string
}

func (s *routeSMS) Send(_ context.Context, _, msg string) error {
	s.mu.Lock()
	s.last = msg
	s.mu.Unlock()
	return nil
}

func (s *routeSMS) message() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

func newPersonalEnv(t *testing.T, withPersonal bool, m *routeMail, s sms.Sender) *env {
	t.Helper()
	users, tokens := memrepo.NewUsers(), memrepo.NewTokens()
	cfg := &config.Config{
		Env: "test", JWTSecret: []byte(strings.Repeat("k", 40)),
		AccessTTL: 15 * time.Minute, RefreshTTL: 24 * time.Hour,
		CORSOrigins: []string{frontendOrigin},
	}
	hasher := password.New(password.Params{Memory: 8, Time: 1, Threads: 1, KeyLen: 32, SaltLen: 16})
	auth, err := service.NewAuthService(users, tokens, hasher,
		service.AuthConfig{JWTSecret: cfg.JWTSecret, AccessTTL: cfg.AccessTTL, RefreshTTL: cfg.RefreshTTL})
	if err != nil {
		t.Fatal(err)
	}
	big := LimitSpec{Every: time.Millisecond, Burst: 1000}
	limits := &Limits{Register: big, Login: big, LoginEmail: big, Refresh: big, PersonalSend: big, PersonalConfirm: big}
	d := Deps{Config: cfg, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Auth: auth, Limits: limits}
	if withPersonal {
		d.PersonalInfo = service.NewPersonalInfoService(users, hasher, otp.NewMemory(), m, s, cfg.JWTSecret)
	}
	srv := httptest.NewServer(New(d))
	t.Cleanup(srv.Close)
	jar, _ := cookiejar.New(nil)
	return &env{srv: srv, client: &http.Client{Jar: jar}, jar: jar, users: users}
}

type personalBody struct {
	Personal struct {
		Name          string `json:"name"`
		Email         string `json:"email"`
		EmailVerified bool   `json:"emailVerified"`
		Phone         string `json:"phone"`
		PhoneVerified bool   `json:"phoneVerified"`
		HasPassword   bool   `json:"hasPassword"`
		Availability  struct {
			Email bool `json:"email"`
			Phone bool `json:"phone"`
		} `json:"availability"`
	} `json:"personal"`
	ResendAfter int `json:"resendAfter"`
}

func decodePersonal(t *testing.T, r resp) personalBody {
	t.Helper()
	var b personalBody
	if err := json.Unmarshal(r.body, &b); err != nil {
		t.Fatalf("bad body %s: %v", r.body, err)
	}
	return b
}

var routeCodeRE = regexp.MustCompile(`\b(\d{6})\b`)

func TestPersonalInfoRoutes(t *testing.T) {
	m, s := &routeMail{}, &routeSMS{}
	e := newPersonalEnv(t, true, m, s)

	// Signed out: refused.
	if r := e.get(t, "/api/v1/seller/personal"); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("signed out GET = %d", r.StatusCode)
	}

	if r := e.post(t, "/api/v1/auth/register", registerBody("john@example.com")); r.StatusCode != http.StatusCreated {
		t.Fatalf("register = %d %s", r.StatusCode, r.body)
	}

	// Read.
	r := e.get(t, "/api/v1/seller/personal")
	if r.StatusCode != 200 {
		t.Fatalf("GET = %d %s", r.StatusCode, r.body)
	}
	b := decodePersonal(t, r)
	if b.Personal.Email != "john@example.com" || b.Personal.Name != "Sakib" || !b.Personal.HasPassword ||
		!b.Personal.Availability.Email || !b.Personal.Availability.Phone || b.Personal.Phone != "" {
		t.Fatalf("personal = %+v", b.Personal)
	}

	// Wrong password -> 422 on the field, NOT 401 (that would look like a dead session).
	r = e.post(t, "/api/v1/seller/personal/email/change", map[string]string{"newEmail": "new@example.com", "currentPassword": "nope"})
	if r.StatusCode != http.StatusUnprocessableEntity || errCode(t, r) != "VALIDATION_FAILED" {
		t.Fatalf("wrong password = %d %s", r.StatusCode, r.body)
	}
	if e.get(t, "/api/v1/seller/personal").StatusCode != 200 {
		t.Fatal("session must survive a wrong password")
	}

	// Request -> code to the new address.
	r = e.post(t, "/api/v1/seller/personal/email/change", map[string]string{"newEmail": "new@example.com", "currentPassword": "password1"})
	if r.StatusCode != 200 || decodePersonal(t, r).ResendAfter != 60 {
		t.Fatalf("request = %d %s", r.StatusCode, r.body)
	}
	to, text := m.last()
	if to != "new@example.com" {
		t.Fatalf("code went to %s", to)
	}
	code := routeCodeRE.FindString(text)

	// Second request inside the cooldown -> 429 with Retry-After.
	r = e.post(t, "/api/v1/seller/personal/email/change", map[string]string{"newEmail": "new@example.com", "currentPassword": "password1"})
	if r.StatusCode != http.StatusTooManyRequests || r.Header.Get("Retry-After") == "" {
		t.Fatalf("cooldown = %d %v", r.StatusCode, r.Header)
	}

	// Wrong code -> 400 INVALID_CODE; right code -> changed.
	r = e.post(t, "/api/v1/seller/personal/email/change/confirm", map[string]string{"newEmail": "new@example.com", "code": "000000"})
	if r.StatusCode != 400 || errCode(t, r) != "INVALID_CODE" {
		t.Fatalf("wrong code = %d %s", r.StatusCode, r.body)
	}
	r = e.post(t, "/api/v1/seller/personal/email/change/confirm", map[string]string{"newEmail": "new@example.com", "code": code})
	if r.StatusCode != 200 {
		t.Fatalf("confirm = %d %s", r.StatusCode, r.body)
	}
	if b := decodePersonal(t, r); b.Personal.Email != "new@example.com" || !b.Personal.EmailVerified {
		t.Fatalf("after confirm: %+v", b.Personal)
	}
	// Signing in with the old address no longer works; the new one does.
	e.post(t, "/api/v1/auth/logout", nil)
	if r := e.post(t, "/api/v1/auth/login", map[string]string{"email": "john@example.com", "password": "password1"}); r.StatusCode != 401 {
		t.Fatalf("old email login = %d", r.StatusCode)
	}
	if r := e.post(t, "/api/v1/auth/login", map[string]string{"email": "new@example.com", "password": "password1"}); r.StatusCode != 200 {
		t.Fatalf("new email login = %d %s", r.StatusCode, r.body)
	}

	// Phone.
	r = e.post(t, "/api/v1/seller/personal/phone/change", map[string]string{"phone": "+8801712345678", "currentPassword": "password1"})
	if r.StatusCode != 200 {
		t.Fatalf("phone request = %d %s", r.StatusCode, r.body)
	}
	pcode := routeCodeRE.FindString(s.message())
	r = e.post(t, "/api/v1/seller/personal/phone/change/confirm", map[string]string{"phone": "01712345678", "code": pcode})
	if r.StatusCode != 200 {
		t.Fatalf("phone confirm = %d %s", r.StatusCode, r.body)
	}
	if b := decodePersonal(t, r); b.Personal.Phone != "01712345678" || !b.Personal.PhoneVerified {
		t.Fatalf("after phone confirm: %+v", b.Personal)
	}

	// Unknown JSON fields are rejected (no mass assignment).
	r = e.post(t, "/api/v1/seller/personal/email/change", `{"newEmail":"x@example.com","currentPassword":"password1","emailVerified":true}`)
	if r.StatusCode != 400 && r.StatusCode != 422 {
		t.Fatalf("unknown field = %d %s", r.StatusCode, r.body)
	}
}

func TestPersonalInfoRoutesAreSellerOnly(t *testing.T) {
	e := newPersonalEnv(t, true, &routeMail{}, &routeSMS{})
	body := registerBody("buyer@example.com")
	body["role"] = "BUYER"
	if r := e.post(t, "/api/v1/auth/register", body); r.StatusCode != http.StatusCreated {
		t.Fatalf("register = %d %s", r.StatusCode, r.body)
	}
	if r := e.get(t, "/api/v1/seller/personal"); r.StatusCode != http.StatusForbidden {
		t.Fatalf("buyer GET = %d", r.StatusCode)
	}
	r := e.post(t, "/api/v1/seller/personal/email/change", map[string]string{"newEmail": "x@example.com", "currentPassword": "password1"})
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("buyer POST = %d", r.StatusCode)
	}
}

func TestPersonalInfoCSRFApplies(t *testing.T) {
	e := newPersonalEnv(t, true, &routeMail{}, &routeSMS{})
	e.post(t, "/api/v1/auth/register", registerBody("john@example.com"))
	r := e.do(t, "POST", "/api/v1/seller/personal/email/change",
		map[string]string{"newEmail": "x@example.com", "currentPassword": "password1"},
		map[string]string{"Origin": "https://evil.example"})
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin POST = %d %s", r.StatusCode, r.body)
	}
}

func TestPersonalInfoMissingProvidersGiveDistinct503s(t *testing.T) {
	// No SMTP (Noop-like mailer is simulated by the real Noop), no SMS provider.
	users, tokens := memrepo.NewUsers(), memrepo.NewTokens()
	cfg := &config.Config{Env: "test", JWTSecret: []byte(strings.Repeat("k", 40)),
		AccessTTL: 15 * time.Minute, RefreshTTL: 24 * time.Hour, CORSOrigins: []string{frontendOrigin}}
	hasher := password.New(password.Params{Memory: 8, Time: 1, Threads: 1, KeyLen: 32, SaltLen: 16})
	auth, _ := service.NewAuthService(users, tokens, hasher,
		service.AuthConfig{JWTSecret: cfg.JWTSecret, AccessTTL: cfg.AccessTTL, RefreshTTL: cfg.RefreshTTL})
	big := LimitSpec{Every: time.Millisecond, Burst: 1000}
	build := func(store otp.Store, mail mailer.Mailer, s sms.Sender) *env {
		d := Deps{Config: cfg, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Auth: auth,
			Limits:       &Limits{Register: big, Login: big, LoginEmail: big, Refresh: big, PersonalSend: big, PersonalConfirm: big},
			PersonalInfo: service.NewPersonalInfoService(users, hasher, store, mail, s, cfg.JWTSecret)}
		srv := httptest.NewServer(New(d))
		t.Cleanup(srv.Close)
		jar, _ := cookiejar.New(nil)
		return &env{srv: srv, client: &http.Client{Jar: jar}, jar: jar, users: users}
	}

	e := build(otp.NewMemory(), mailer.Noop{}, sms.NotConfigured{})
	e.post(t, "/api/v1/auth/register", registerBody("john@example.com"))
	b := decodePersonal(t, e.get(t, "/api/v1/seller/personal"))
	if b.Personal.Availability.Email || b.Personal.Availability.Phone {
		t.Fatalf("availability = %+v", b.Personal.Availability)
	}
	r := e.post(t, "/api/v1/seller/personal/email/change", map[string]string{"newEmail": "n@example.com", "currentPassword": "password1"})
	if r.StatusCode != 503 || errCode(t, r) != "EMAIL_NOT_CONFIGURED" {
		t.Fatalf("email = %d %s", r.StatusCode, r.body)
	}
	r = e.post(t, "/api/v1/seller/personal/phone/change", map[string]string{"phone": "01712345678", "currentPassword": "password1"})
	if r.StatusCode != 503 || errCode(t, r) != "SMS_NOT_CONFIGURED" {
		t.Fatalf("sms = %d %s", r.StatusCode, r.body)
	}

	e2 := build(nil, &routeMail{}, &routeSMS{})
	e2.post(t, "/api/v1/auth/login", map[string]string{"email": "john@example.com", "password": "password1"})
	r = e2.post(t, "/api/v1/seller/personal/email/change", map[string]string{"newEmail": "n@example.com", "currentPassword": "password1"})
	if r.StatusCode != 503 || errCode(t, r) != "OTP_STORE_NOT_CONFIGURED" {
		t.Fatalf("redis = %d %s", r.StatusCode, r.body)
	}
}

func TestPersonalInfoRoutesAbsentWhenServiceNotWired(t *testing.T) {
	e := newPersonalEnv(t, false, nil, nil)
	e.post(t, "/api/v1/auth/register", registerBody("john@example.com"))
	if r := e.get(t, "/api/v1/seller/personal"); r.StatusCode != http.StatusNotFound {
		t.Fatalf("GET = %d", r.StatusCode)
	}
}
