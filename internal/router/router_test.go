package router

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"shopbd/backend/internal/config"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/password"
	"shopbd/backend/internal/repository/memrepo"
	"shopbd/backend/internal/service"
)

const frontendOrigin = "http://localhost:3000"

type env struct {
	srv    *httptest.Server
	client *http.Client
	jar    *cookiejar.Jar
	users  *memrepo.Users
}

func newEnv(t *testing.T, limits *Limits) *env {
	t.Helper()
	users, tokens := memrepo.NewUsers(), memrepo.NewTokens()
	cfg := &config.Config{
		Env:          "test",
		JWTSecret:    []byte(strings.Repeat("k", 40)),
		AccessTTL:    15 * time.Minute,
		RefreshTTL:   24 * time.Hour,
		CORSOrigins:  []string{frontendOrigin},
		CookieSecure: false,
	}
	svc, err := service.NewAuthService(users, tokens,
		password.New(password.Params{Memory: 8, Time: 1, Threads: 1, KeyLen: 32, SaltLen: 16}),
		service.AuthConfig{JWTSecret: cfg.JWTSecret, AccessTTL: cfg.AccessTTL, RefreshTTL: cfg.RefreshTTL})
	if err != nil {
		t.Fatal(err)
	}
	if limits == nil {
		// Generous limits so unrelated tests never trip them.
		limits = &Limits{
			Register:   LimitSpec{Every: time.Millisecond, Burst: 1000},
			Login:      LimitSpec{Every: time.Millisecond, Burst: 1000},
			LoginEmail: LimitSpec{Every: time.Millisecond, Burst: 1000},
			Refresh:    LimitSpec{Every: time.Millisecond, Burst: 1000},
		}
	}
	h := New(Deps{Config: cfg, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Auth: svc, Limits: limits})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	jar, _ := cookiejar.New(nil)
	return &env{srv: srv, client: &http.Client{Jar: jar}, jar: jar, users: users}
}

type resp struct {
	*http.Response
	body []byte
}

func (e *env) do(t *testing.T, method, path string, body any, headers map[string]string) resp {
	t.Helper()
	var rd io.Reader
	if body != nil {
		if s, ok := body.(string); ok {
			rd = strings.NewReader(s)
		} else {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		}
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	r, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	return resp{Response: r, body: b}
}

// post is a state-changing request carrying the headers the frontend sends.
func (e *env) post(t *testing.T, path string, body any) resp {
	return e.do(t, "POST", path, body, map[string]string{"X-Requested-With": "fetch", "Origin": frontendOrigin})
}

func (e *env) get(t *testing.T, path string) resp {
	return e.do(t, "GET", path, nil, map[string]string{"Origin": frontendOrigin})
}

func (e *env) cookie(name, path string) *http.Cookie {
	u, _ := url.Parse(e.srv.URL + path)
	for _, c := range e.jar.Cookies(u) {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func registerBody(email string) map[string]string {
	return map[string]string{"name": "Sakib", "email": email, "password": "password1", "confirmPassword": "password1", "role": "SELLER"}
}

func errCode(t *testing.T, r resp) string {
	t.Helper()
	var b struct {
		Error struct {
			Code   string            `json:"code"`
			Fields map[string]string `json:"fields"`
		} `json:"error"`
	}
	if err := json.Unmarshal(r.body, &b); err != nil {
		t.Fatalf("not an error body: %s", r.body)
	}
	return b.Error.Code
}

func TestRegisterSetsCookiesAndReturnsUser(t *testing.T) {
	e := newEnv(t, nil)
	r := e.post(t, "/api/v1/auth/register", registerBody("Sakib@Example.com"))
	if r.StatusCode != 201 {
		t.Fatalf("status %d body %s", r.StatusCode, r.body)
	}
	var body struct {
		User map[string]any `json:"user"`
	}
	_ = json.Unmarshal(r.body, &body)
	if body.User["email"] != "sakib@example.com" || body.User["role"] != "SELLER" || body.User["id"] == "" {
		t.Errorf("unexpected user: %v", body.User)
	}
	for _, leaked := range []string{"password", "Password", "hash"} {
		if strings.Contains(string(r.body), leaked) {
			t.Errorf("response must not contain %q: %s", leaked, r.body)
		}
	}

	var session, refresh *http.Cookie
	for _, c := range r.Cookies() {
		switch c.Name {
		case "session_token":
			session = c
		case "refresh_token":
			refresh = c
		}
	}
	if session == nil || refresh == nil {
		t.Fatalf("expected session_token and refresh_token cookies, got %v", r.Cookies())
	}
	if !session.HttpOnly || session.SameSite != http.SameSiteLaxMode || session.Path != "/" {
		t.Errorf("session cookie attributes wrong: %+v", session)
	}
	if !refresh.HttpOnly || refresh.SameSite != http.SameSiteLaxMode || refresh.Path != "/api/v1/auth" {
		t.Errorf("refresh cookie attributes wrong: %+v", refresh)
	}
	if r.Header.Get("Cache-Control") != "no-store" {
		t.Error("auth responses must not be cacheable")
	}
}

func TestRegisterErrors(t *testing.T) {
	e := newEnv(t, nil)

	r := e.post(t, "/api/v1/auth/register", map[string]string{"name": "A", "email": "bad", "password": "x", "confirmPassword": "y", "role": ""})
	if r.StatusCode != 422 || errCode(t, r) != "VALIDATION_FAILED" {
		t.Fatalf("validation: %d %s", r.StatusCode, r.body)
	}
	var b struct {
		Error struct{ Fields map[string]string } `json:"error"`
	}
	_ = json.Unmarshal(r.body, &b)
	for _, f := range []string{"name", "email", "password", "confirmPassword", "role"} {
		if b.Error.Fields[f] == "" {
			t.Errorf("missing field error for %s: %s", f, r.body)
		}
	}

	if r := e.post(t, "/api/v1/auth/register", registerBody("dup@example.com")); r.StatusCode != 201 {
		t.Fatalf("first registration: %d", r.StatusCode)
	}
	r = e.post(t, "/api/v1/auth/register", registerBody("DUP@example.com"))
	if r.StatusCode != 409 || errCode(t, r) != "EMAIL_TAKEN" {
		t.Fatalf("duplicate: %d %s", r.StatusCode, r.body)
	}

	// Mass assignment: server-managed fields are rejected, not ignored.
	body := `{"name":"Sakib","email":"x@example.com","password":"password1","confirmPassword":"password1","role":"BUYER","status":"active"}`
	if r := e.post(t, "/api/v1/auth/register", body); r.StatusCode != 422 {
		t.Errorf("unknown field status: %d %s", r.StatusCode, r.body)
	}
	// NID is never collected.
	body = `{"name":"Sakib","email":"y@example.com","password":"password1","confirmPassword":"password1","role":"SELLER","nidNumber":"1234567890"}`
	if r := e.post(t, "/api/v1/auth/register", body); r.StatusCode != 422 {
		t.Errorf("nidNumber must be rejected: %d %s", r.StatusCode, r.body)
	}
	// ADMIN cannot be self-assigned.
	adm := registerBody("z@example.com")
	adm["role"] = "ADMIN"
	if r := e.post(t, "/api/v1/auth/register", adm); r.StatusCode != 422 {
		t.Errorf("ADMIN role must be rejected: %d", r.StatusCode)
	}
	if r := e.post(t, "/api/v1/auth/register", "{"); r.StatusCode != 400 {
		t.Errorf("malformed json: %d", r.StatusCode)
	}
}

func TestMeRequiresAuthAndWorksAfterRegister(t *testing.T) {
	e := newEnv(t, nil)
	if r := e.get(t, "/api/v1/auth/me"); r.StatusCode != 401 {
		t.Fatalf("unauthenticated me: %d", r.StatusCode)
	}
	e.post(t, "/api/v1/auth/register", registerBody("a@example.com"))
	r := e.get(t, "/api/v1/auth/me")
	if r.StatusCode != 200 || !strings.Contains(string(r.body), "a@example.com") {
		t.Fatalf("me: %d %s", r.StatusCode, r.body)
	}
}

func TestLoginLogoutFlow(t *testing.T) {
	e := newEnv(t, nil)
	e.post(t, "/api/v1/auth/register", registerBody("a@example.com"))
	if r := e.post(t, "/api/v1/auth/logout", nil); r.StatusCode != 204 {
		t.Fatalf("logout: %d", r.StatusCode)
	}
	if e.cookie("session_token", "/") != nil || e.cookie("refresh_token", "/api/v1/auth/refresh") != nil {
		t.Error("cookies should be cleared after logout")
	}
	if r := e.get(t, "/api/v1/auth/me"); r.StatusCode != 401 {
		t.Errorf("me after logout: %d", r.StatusCode)
	}

	bad := e.post(t, "/api/v1/auth/login", map[string]string{"email": "a@example.com", "password": "wrong-password"})
	if bad.StatusCode != 401 || errCode(t, bad) != "INVALID_CREDENTIALS" {
		t.Fatalf("wrong password: %d %s", bad.StatusCode, bad.body)
	}
	ghost := e.post(t, "/api/v1/auth/login", map[string]string{"email": "ghost@example.com", "password": "password1"})
	if ghost.StatusCode != 401 || errCode(t, ghost) != "INVALID_CREDENTIALS" {
		t.Fatalf("unknown email must look like wrong password: %d %s", ghost.StatusCode, ghost.body)
	}
	if errMessage(t, ghost) != errMessage(t, bad) {
		t.Error("unknown email and wrong password must produce the same message")
	}

	ok := e.post(t, "/api/v1/auth/login", map[string]string{"email": "A@Example.com", "password": "password1"})
	if ok.StatusCode != 200 {
		t.Fatalf("login: %d %s", ok.StatusCode, ok.body)
	}
	if r := e.get(t, "/api/v1/auth/me"); r.StatusCode != 200 {
		t.Errorf("me after login: %d", r.StatusCode)
	}
}

func TestSuspendedAccountCannotLogin(t *testing.T) {
	e := newEnv(t, nil)
	r := e.post(t, "/api/v1/auth/register", registerBody("a@example.com"))
	var body struct{ User struct{ ID string } }
	_ = json.Unmarshal(r.body, &body)
	e.users.SetStatus(body.User.ID, models.StatusSuspended)
	r = e.post(t, "/api/v1/auth/login", map[string]string{"email": "a@example.com", "password": "password1"})
	if r.StatusCode != 403 || errCode(t, r) != "ACCOUNT_SUSPENDED" {
		t.Fatalf("suspended login: %d %s", r.StatusCode, r.body)
	}
}

func TestRefreshRotationOverHTTP(t *testing.T) {
	e := newEnv(t, nil)
	e.post(t, "/api/v1/auth/register", registerBody("a@example.com"))
	oldRefresh := e.cookie("refresh_token", "/api/v1/auth/refresh")
	if oldRefresh == nil {
		t.Fatal("refresh cookie should be sent to the refresh endpoint")
	}
	if e.cookie("refresh_token", "/api/v1/stores") != nil {
		t.Error("refresh cookie must be scoped to /api/v1/auth only")
	}

	r := e.post(t, "/api/v1/auth/refresh", nil)
	if r.StatusCode != 200 {
		t.Fatalf("refresh: %d %s", r.StatusCode, r.body)
	}
	newRefresh := e.cookie("refresh_token", "/api/v1/auth/refresh")
	if newRefresh == nil || newRefresh.Value == oldRefresh.Value {
		t.Fatal("refresh token must rotate")
	}

	// Replaying the stolen, already-used token revokes the whole family.
	e.jar.SetCookies(mustURL(e.srv.URL+"/api/v1/auth"), []*http.Cookie{{Name: "refresh_token", Value: oldRefresh.Value, Path: "/api/v1/auth"}})
	r = e.post(t, "/api/v1/auth/refresh", nil)
	if r.StatusCode != 401 || errCode(t, r) != "TOKEN_REUSED" {
		t.Fatalf("replay: %d %s", r.StatusCode, r.body)
	}
	if e.cookie("refresh_token", "/api/v1/auth/refresh") != nil {
		t.Error("cookies should be cleared when a session is invalidated")
	}
}

func TestRefreshWithoutCookie(t *testing.T) {
	e := newEnv(t, nil)
	if r := e.post(t, "/api/v1/auth/refresh", nil); r.StatusCode != 401 {
		t.Errorf("refresh without cookie: %d", r.StatusCode)
	}
}

func mustURL(s string) *url.URL { u, _ := url.Parse(s); return u }

func TestCSRFProtection(t *testing.T) {
	e := newEnv(t, nil)
	// No custom header.
	r := e.do(t, "POST", "/api/v1/auth/register", registerBody("a@example.com"), map[string]string{"Origin": frontendOrigin})
	if r.StatusCode != 403 || errCode(t, r) != "CSRF_REJECTED" {
		t.Fatalf("missing header: %d %s", r.StatusCode, r.body)
	}
	// Foreign origin, even with the header.
	r = e.do(t, "POST", "/api/v1/auth/register", registerBody("a@example.com"), map[string]string{"X-Requested-With": "fetch", "Origin": "https://evil.example"})
	if r.StatusCode != 403 {
		t.Fatalf("foreign origin: %d", r.StatusCode)
	}
	// Logout is state-changing too.
	e.post(t, "/api/v1/auth/register", registerBody("b@example.com"))
	r = e.do(t, "POST", "/api/v1/auth/logout", nil, map[string]string{"Origin": "https://evil.example"})
	if r.StatusCode != 403 {
		t.Fatalf("cross-site logout must be rejected: %d", r.StatusCode)
	}
	if r := e.get(t, "/api/v1/auth/me"); r.StatusCode != 200 {
		t.Error("the rejected logout must not have ended the session")
	}
}

func TestCORS(t *testing.T) {
	e := newEnv(t, nil)
	preflight := e.do(t, "OPTIONS", "/api/v1/auth/login", nil, map[string]string{
		"Origin":                         frontendOrigin,
		"Access-Control-Request-Method":  "POST",
		"Access-Control-Request-Headers": "content-type,x-requested-with",
	})
	if preflight.StatusCode != 204 && preflight.StatusCode != 200 {
		t.Fatalf("preflight status %d", preflight.StatusCode)
	}
	if got := preflight.Header.Get("Access-Control-Allow-Origin"); got != frontendOrigin {
		t.Errorf("allow-origin = %q", got)
	}
	if preflight.Header.Get("Access-Control-Allow-Credentials") != "true" {
		t.Error("credentials must be allowed for the frontend origin")
	}

	foreign := e.do(t, "OPTIONS", "/api/v1/auth/login", nil, map[string]string{
		"Origin": "https://evil.example", "Access-Control-Request-Method": "POST",
	})
	if foreign.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Error("foreign origins must not be granted CORS access")
	}

	actual := e.get(t, "/healthz")
	if actual.Header.Get("Access-Control-Allow-Origin") != frontendOrigin || actual.Header.Get("Access-Control-Allow-Credentials") != "true" {
		t.Errorf("actual response missing CORS headers: %v", actual.Header)
	}
}

func TestRateLimiting(t *testing.T) {
	limits := &Limits{
		Register:   LimitSpec{Every: time.Hour, Burst: 2},
		Login:      LimitSpec{Every: time.Hour, Burst: 3},
		LoginEmail: LimitSpec{Every: time.Hour, Burst: 100},
		Refresh:    LimitSpec{Every: time.Hour, Burst: 100},
	}
	e := newEnv(t, limits)
	for i := 0; i < 2; i++ {
		e.post(t, "/api/v1/auth/register", map[string]string{"name": "A"}) // invalid, still counts
	}
	r := e.post(t, "/api/v1/auth/register", registerBody("a@example.com"))
	if r.StatusCode != 429 || errCode(t, r) != "RATE_LIMITED" || r.Header.Get("Retry-After") == "" {
		t.Fatalf("register limit: %d %s", r.StatusCode, r.body)
	}
	for i := 0; i < 3; i++ {
		e.post(t, "/api/v1/auth/login", map[string]string{"email": "a@example.com", "password": "password1"})
	}
	if r := e.post(t, "/api/v1/auth/login", map[string]string{"email": "a@example.com", "password": "password1"}); r.StatusCode != 429 {
		t.Fatalf("login limit: %d", r.StatusCode)
	}
}

func TestPerEmailLoginThrottle(t *testing.T) {
	limits := &Limits{
		Register:   LimitSpec{Every: time.Millisecond, Burst: 1000},
		Login:      LimitSpec{Every: time.Millisecond, Burst: 1000},
		LoginEmail: LimitSpec{Every: time.Hour, Burst: 2},
		Refresh:    LimitSpec{Every: time.Millisecond, Burst: 1000},
	}
	e := newEnv(t, limits)
	try := func(email string) int {
		return e.post(t, "/api/v1/auth/login", map[string]string{"email": email, "password": "wrong-password"}).StatusCode
	}
	if try("victim@example.com") != 401 || try("VICTIM@example.com") != 401 {
		t.Fatal("first two attempts should reach the credential check")
	}
	if got := try("victim@example.com"); got != 429 {
		t.Fatalf("third attempt on the same email (any casing) should be throttled, got %d", got)
	}
	if got := try("someone-else@example.com"); got != 401 {
		t.Errorf("other emails are unaffected, got %d", got)
	}
}

func TestSecurityHeadersAndErrorShapes(t *testing.T) {
	e := newEnv(t, nil)
	r := e.get(t, "/healthz")
	if r.StatusCode != 200 || r.Header.Get("X-Content-Type-Options") != "nosniff" || r.Header.Get("X-Frame-Options") != "DENY" {
		t.Errorf("health/security headers: %d %v", r.StatusCode, r.Header)
	}
	if r := e.get(t, "/readyz"); r.StatusCode != 200 {
		t.Errorf("readyz: %d", r.StatusCode)
	}
	nf := e.get(t, "/api/v1/nope")
	if nf.StatusCode != 404 || errCode(t, nf) != "NOT_FOUND" {
		t.Errorf("404 shape: %d %s", nf.StatusCode, nf.body)
	}
	mna := e.get(t, "/api/v1/auth/login")
	if mna.StatusCode != 405 || errCode(t, mna) != "METHOD_NOT_ALLOWED" {
		t.Errorf("405 shape: %d %s", mna.StatusCode, mna.body)
	}
	var b struct{ Error struct{ RequestID string } }
	_ = json.Unmarshal(nf.body, &b)
	if b.Error.RequestID == "" {
		t.Error("error responses should carry a requestId")
	}
}

func TestWrongContentTypeRejected(t *testing.T) {
	e := newEnv(t, nil)
	req, _ := http.NewRequest("POST", e.srv.URL+"/api/v1/auth/login", strings.NewReader("email=a"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Requested-With", "fetch")
	r, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != 415 {
		t.Errorf("got %d", r.StatusCode)
	}
}

func errMessage(t *testing.T, r resp) string {
	t.Helper()
	var b struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(r.body, &b); err != nil {
		t.Fatalf("not an error body: %s", r.body)
	}
	return b.Error.Message
}
