package metrics

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestObserveAndSnapshot(t *testing.T) {
	c := New()
	base := time.Unix(1_700_000_000, 0)
	c.now = func() time.Time { return base }

	for i := 0; i < 8; i++ {
		c.Observe("GET /a", 200, 100, 20*time.Millisecond)
	}
	c.Observe("GET /a", 404, 10, 3*time.Millisecond)
	c.Observe("POST /b", 500, 10, 700*time.Millisecond)

	s := c.Snapshot()
	if s.TotalRequests != 10 || s.Status2xx != 8 || s.Status4xx != 1 || s.Status5xx != 1 {
		t.Fatalf("unexpected totals: %+v", s)
	}
	if s.ErrorRate != 10 {
		t.Errorf("error rate = %v, want 10", s.ErrorRate)
	}
	if len(s.Series) != windowMinutes {
		t.Fatalf("series len = %d", len(s.Series))
	}
	if last := s.Series[len(s.Series)-1]; last.Requests != 10 || last.Errors5 != 1 {
		t.Errorf("current minute = %+v", last)
	}
	if s.Last5Minutes != 10 || s.Last5ErrorRate != 10 {
		t.Errorf("last5 = %d / %v", s.Last5Minutes, s.Last5ErrorRate)
	}
	if s.P50Ms != 25 || s.P95Ms != 1000 {
		t.Errorf("p50=%v p95=%v", s.P50Ms, s.P95Ms)
	}
	if len(s.TopRoutes) != 2 || s.TopRoutes[0].Route != "GET /a" || s.TopRoutes[0].Requests != 9 {
		t.Errorf("top routes = %+v", s.TopRoutes)
	}
}

func TestOldMinutesAreDropped(t *testing.T) {
	c := New()
	now := time.Unix(1_700_000_000, 0)
	c.now = func() time.Time { return now }
	c.Observe("GET /a", 200, 0, time.Millisecond)

	now = now.Add(61 * time.Minute) // the old bucket slot is reused/expired
	s := c.Snapshot()
	for _, p := range s.Series {
		if p.Requests != 0 {
			t.Fatalf("expected empty window, got %+v", p)
		}
	}
}

func TestMiddlewareSkipsProbesAndCountsStatus(t *testing.T) {
	c := New()
	h := Middleware(c, func(*http.Request) string { return "/x/{id}" })(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/boom" {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			_, _ = w.Write([]byte("hello"))
		}))

	for _, p := range []string{"/healthz", "/readyz", "/ok", "/boom"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, p, nil))
	}
	s := c.Snapshot()
	if s.TotalRequests != 2 || s.Status2xx != 1 || s.Status5xx != 1 || s.BytesOut != 5 || s.InFlight != 0 {
		t.Fatalf("unexpected snapshot: %+v", s)
	}
	if s.TopRoutes[0].Route != "GET /x/{id}" {
		t.Errorf("route = %q", s.TopRoutes[0].Route)
	}
}

func TestReadRuntime(t *testing.T) {
	rt := ReadRuntime(time.Now().Add(-time.Minute))
	if rt.Goroutines < 1 || rt.CPUs < 1 || rt.UptimeSeconds < 59 || rt.GoVersion == "" {
		t.Errorf("bad runtime: %+v", rt)
	}
}
