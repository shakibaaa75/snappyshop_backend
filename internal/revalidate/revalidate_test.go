package revalidate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestNilClientIsSafe(t *testing.T) {
	var c *Client
	c.Start(context.Background())
	c.Notify("a", "b") // must not panic
}

func TestNewIsOffWithoutSecretOrURL(t *testing.T) {
	if New("", "secret", nil) != nil || New("http://x", "", nil) != nil {
		t.Fatal("client must be nil when URL or secret is missing")
	}
}

func TestNotifySendsOneBatchedCall(t *testing.T) {
	var mu sync.Mutex
	var got []string
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != Path || r.Method != http.MethodPost {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer s3cret" || r.Header.Get("x-revalidate-secret") != "s3cret" {
			t.Errorf("secret header missing")
		}
		var body struct {
			Slugs []string `json:"slugs"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		calls++
		got = append(got, body.Slugs...)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(srv.URL, "s3cret", nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx)
	c.Notify("Shop-A", "shop-b", "shop-a", "  ", "")

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("want 1 batched call, got %d", calls)
	}
	seen := map[string]bool{}
	for _, s := range got {
		seen[s] = true
	}
	if len(seen) != 2 || !seen["shop-a"] || !seen["shop-b"] {
		t.Fatalf("slugs = %v", got)
	}
}
