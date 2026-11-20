package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"shopbd/backend/internal/ratelimit"
)

type stubBackend struct {
	err   error
	calls atomic.Int64
	cap   int64
}

func (s *stubBackend) Take(context.Context, string, ratelimit.Spec) (bool, time.Duration, error) {
	n := s.calls.Add(1)
	if s.err != nil {
		return false, 0, s.err
	}
	if n > s.cap {
		return false, 20 * time.Second, nil
	}
	return true, 0, nil
}

func serve(h http.Handler) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/login", nil)
	req.RemoteAddr = "203.0.113.9:4444"
	h.ServeHTTP(rec, req)
	return rec
}

var ok200 = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

func TestSharedRateLimit_429WithRetryAfter(t *testing.T) {
	g := ratelimit.NewGuard(&stubBackend{cap: 1}, nil, ratelimit.GuardConfig{}, nil)
	h := SharedRateLimit(g, "login", ratelimit.Spec{Capacity: 1, Every: time.Minute}, false, nil)(ok200)
	if rec := serve(h); rec.Code != 200 {
		t.Fatalf("first request: %d", rec.Code)
	}
	rec := serve(h)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "20" {
		t.Fatalf("code=%d retry-after=%q", rec.Code, rec.Header().Get("Retry-After"))
	}
}

func TestSharedRateLimit_RedisDownFailClosed_503(t *testing.T) {
	g := ratelimit.NewGuard(&stubBackend{err: errors.New("down")}, nil, ratelimit.GuardConfig{Policy: ratelimit.FallbackClosed}, nil)
	h := SharedRateLimit(g, "login", ratelimit.Spec{Capacity: 5, Every: time.Second}, false, nil)(ok200)
	rec := serve(h)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("code=%d retry-after=%q", rec.Code, rec.Header().Get("Retry-After"))
	}
}

func TestSharedRateLimit_RedisDownLocalFallbackStillLimits(t *testing.T) {
	g := ratelimit.NewGuard(&stubBackend{err: errors.New("down")}, nil, ratelimit.GuardConfig{LocalDivisor: 2}, nil)
	h := SharedRateLimit(g, "login", ratelimit.Spec{Capacity: 4, Every: time.Minute}, false, nil)(ok200)
	codes := []int{serve(h).Code, serve(h).Code, serve(h).Code}
	if codes[0] != 200 || codes[1] != 200 || codes[2] != http.StatusTooManyRequests {
		t.Fatalf("local fallback must still limit (4/2=2 allowed): %v", codes)
	}
}
