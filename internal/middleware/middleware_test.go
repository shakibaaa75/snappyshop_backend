package middleware

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/httpx"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/service"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

var ok200 = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

func do(h http.Handler, method, path string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// A store on a seller's own domain is allowed through only when the extra
// rule says that origin is a connected custom domain. Every other CSRF rule
// still applies to it.
func TestCSRFWithCustomDomainOrigins(t *testing.T) {
	connected := func(_ context.Context, origin string) bool { return origin == "https://shop.brand.com" }
	h := CSRFWith([]string{"http://localhost:3000"}, connected, discard)(ok200)
	cases := []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{"connected domain with header", map[string]string{"X-Requested-With": "fetch", "Origin": "https://shop.brand.com"}, 200},
		{"connected domain without header", map[string]string{"Origin": "https://shop.brand.com"}, 403},
		{"unknown domain", map[string]string{"X-Requested-With": "fetch", "Origin": "https://evil.example"}, 403},
		{"lookalike of a connected domain", map[string]string{"X-Requested-With": "fetch", "Origin": "https://shop.brand.com.evil.example"}, 403},
		{"fixed origin still works", map[string]string{"X-Requested-With": "fetch", "Origin": "http://localhost:3000"}, 200},
		{"null origin", map[string]string{"X-Requested-With": "fetch", "Origin": "null"}, 403},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := do(h, "POST", "/x", c.headers).Code; got != c.want {
				t.Fatalf("got %d want %d", got, c.want)
			}
		})
	}
}

func TestCSRF(t *testing.T) {
	h := CSRF([]string{"http://localhost:3000"}, discard)(ok200)
	cases := []struct {
		name    string
		method  string
		headers map[string]string
		want    int
	}{
		{"GET needs nothing", "GET", nil, 200},
		{"OPTIONS needs nothing", "OPTIONS", nil, 200},
		{"POST without header", "POST", nil, 403},
		{"POST with header, no origin (curl/server)", "POST", map[string]string{"X-Requested-With": "fetch"}, 200},
		{"POST allowed origin", "POST", map[string]string{"X-Requested-With": "fetch", "Origin": "http://localhost:3000"}, 200},
		{"POST allowed origin case-insensitive", "POST", map[string]string{"X-Requested-With": "fetch", "Origin": "HTTP://LOCALHOST:3000"}, 200},
		{"POST foreign origin", "POST", map[string]string{"X-Requested-With": "fetch", "Origin": "https://evil.example"}, 403},
		{"POST null origin", "POST", map[string]string{"X-Requested-With": "fetch", "Origin": "null"}, 403},
		{"PATCH without header", "PATCH", map[string]string{"Origin": "http://localhost:3000"}, 403},
		{"DELETE foreign origin", "DELETE", map[string]string{"X-Requested-With": "fetch", "Origin": "https://evil.example"}, 403},
		{"blank header value", "POST", map[string]string{"X-Requested-With": "  "}, 403},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := do(h, c.method, "/x", c.headers).Code; got != c.want {
				t.Fatalf("got %d want %d", got, c.want)
			}
		})
	}
}

func TestKeyedLimiter(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l := NewKeyedLimiter(10*time.Second, 2)
	l.now = func() time.Time { return now }

	for i := 0; i < 2; i++ {
		if ok, _ := l.Allow("a"); !ok {
			t.Fatalf("request %d within burst should pass", i+1)
		}
	}
	ok, wait := l.Allow("a")
	if ok || wait <= 0 || wait > 10*time.Second {
		t.Fatalf("third request should be limited with a wait, got ok=%v wait=%v", ok, wait)
	}
	if ok, _ := l.Allow("b"); !ok {
		t.Error("a different key has its own bucket")
	}
	now = now.Add(11 * time.Second)
	if ok, _ := l.Allow("a"); !ok {
		t.Error("bucket should refill after the interval")
	}
}

func TestKeyedLimiterSweepsIdleKeys(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l := NewKeyedLimiter(time.Second, 1)
	l.now = func() time.Time { return now }
	l.lastSweep = now
	l.Allow("old")
	now = now.Add(30 * time.Minute)
	l.Allow("new")
	if _, exists := l.entries["old"]; exists {
		t.Error("idle key should have been swept")
	}
}

func TestRateLimitMiddlewareReturns429WithRetryAfter(t *testing.T) {
	l := NewKeyedLimiter(time.Minute, 1)
	h := RateLimit(l, false, discard)(ok200)
	if got := do(h, "POST", "/x", nil).Code; got != 200 {
		t.Fatalf("first request: %d", got)
	}
	rec := do(h, "POST", "/x", nil)
	if rec.Code != 429 || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("second request: %d retry-after=%q", rec.Code, rec.Header().Get("Retry-After"))
	}
}

func TestClientIP(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "203.0.113.9:5555"
	req.Header.Set("X-Forwarded-For", "6.6.6.6, 198.51.100.7")
	if got := ClientIP(req, false); got != "203.0.113.9" {
		t.Errorf("untrusted proxy: got %s", got)
	}
	if got := ClientIP(req, true); got != "198.51.100.7" {
		t.Errorf("trusted proxy should use the entry appended by the nearest proxy, got %s", got)
	}
}

type fakeAuth struct{}

func (fakeAuth) AuthenticateAccess(token string) (*service.Claims, error) {
	switch token {
	case "buyer":
		return &service.Claims{UserID: "u1", Role: models.RoleBuyer}, nil
	case "seller":
		return &service.Claims{UserID: "u2", Role: models.RoleSeller}, nil
	}
	return nil, apperr.Unauthenticated("bad")
}

func withCookie(h http.Handler, value string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", "/", nil)
	if value != "" {
		req.AddCookie(&http.Cookie{Name: httpx.SessionCookie, Value: value})
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestAuthenticateAndRequireRole(t *testing.T) {
	var gotUser string
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, ok := ClaimsFrom(r.Context())
		if !ok {
			t.Error("claims missing in context")
			return
		}
		gotUser = c.UserID
	})
	h := Authenticate(fakeAuth{}, discard)(RequireRole(discard, models.RoleSeller)(inner))

	if got := withCookie(h, "").Code; got != 401 {
		t.Errorf("no cookie: %d", got)
	}
	if got := withCookie(h, "garbage").Code; got != 401 {
		t.Errorf("bad token: %d", got)
	}
	if got := withCookie(h, "buyer").Code; got != 403 {
		t.Errorf("buyer on seller route: %d", got)
	}
	if got := withCookie(h, "seller").Code; got != 200 || gotUser != "u2" {
		t.Errorf("seller: %d user=%q", got, gotUser)
	}
}

func TestAuthenticateIgnoresAuthorizationHeader(t *testing.T) {
	h := Authenticate(fakeAuth{}, discard)(ok200)
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer seller")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("only the session cookie should authenticate, got %d", rec.Code)
	}
}

func TestRecovererTurnsPanicInto500(t *testing.T) {
	h := Recoverer(discard)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic(errors.New("boom with secret=abc"))
	}))
	rec := do(h, "GET", "/", nil)
	if rec.Code != 500 || rec.Body.Len() == 0 {
		t.Fatalf("got %d", rec.Code)
	}
	if body := rec.Body.String(); contains(body, "secret") || contains(body, "boom") {
		t.Errorf("panic details leaked: %s", body)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestSecurityHeadersAndNoStore(t *testing.T) {
	h := SecurityHeaders(true)(NoStore(ok200))
	rec := do(h, "GET", "/", nil)
	for k, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
		"Cache-Control":          "no-store",
	} {
		if got := rec.Header().Get(k); got != want {
			t.Errorf("%s = %q want %q", k, got, want)
		}
	}
	if rec.Header().Get("Strict-Transport-Security") == "" {
		t.Error("HSTS expected when enabled")
	}
	if do(SecurityHeaders(false)(ok200), "GET", "/", nil).Header().Get("Strict-Transport-Security") != "" {
		t.Error("HSTS must be off when disabled")
	}
}
