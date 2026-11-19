package courier

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func creds() map[string]string {
	return map[string]string{"apiKey": "key-ABCD1234", "secretKey": "sec-XYZ"}
}

func serve(t *testing.T, h http.HandlerFunc) *Steadfast {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return NewSteadfast(srv.URL, nil)
}

func TestSteadfastVerifySuccessSendsDocumentedHeaders(t *testing.T) {
	var path, method, k, s string
	p := serve(t, func(w http.ResponseWriter, r *http.Request) {
		path, method, k, s = r.URL.Path, r.Method, r.Header.Get("Api-Key"), r.Header.Get("Secret-Key")
		w.Write([]byte(`{"status":200,"current_balance":0}`))
	})
	if err := p.Verify(context.Background(), creds()); err != nil {
		t.Fatal(err)
	}
	if path != "/get_balance" || method != "GET" || k != "key-ABCD1234" || s != "sec-XYZ" {
		t.Fatalf("unexpected request: %s %s %q %q", method, path, k, s)
	}
}

func TestSteadfastVerifyErrorMapping(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"http 401", 401, `{"message":"Unauthorized"}`, ErrInvalidCredentials},
		{"http 403", 403, ``, ErrInvalidCredentials},
		{"body status 401", 200, `{"status":401,"message":"bad"}`, ErrInvalidCredentials},
		{"http 500", 500, `oops`, ErrUnavailable},
		{"http 429", 429, ``, ErrUnavailable},
		{"unknown 200 shape", 200, `{"hello":"world"}`, ErrUnavailable},
		{"html 200", 200, `<html>`, ErrUnavailable},
		{"200 without balance", 200, `{"status":200}`, ErrUnavailable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := serve(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(c.status)
				w.Write([]byte(c.body))
			})
			if err := p.Verify(context.Background(), creds()); !errors.Is(err, c.want) {
				t.Fatalf("got %v, want %v", err, c.want)
			}
		})
	}
}

func TestSteadfastNetworkFailureAndTimeoutAreUnavailable(t *testing.T) {
	p := NewSteadfast("http://127.0.0.1:1", nil)
	if err := p.Verify(context.Background(), creds()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("got %v", err)
	}
	slow := serve(t, func(w http.ResponseWriter, r *http.Request) { time.Sleep(300 * time.Millisecond) })
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := slow.Verify(ctx, creds()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("got %v", err)
	}
}

func TestSteadfastErrorsNeverContainSecrets(t *testing.T) {
	p := serve(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) })
	err := p.Verify(context.Background(), creds())
	for _, secret := range []string{"key-ABCD1234", "sec-XYZ"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error leaks %q: %v", secret, err)
		}
	}
}

func TestSteadfastDoesNotFollowRedirectsOrSendHeaderInjection(t *testing.T) {
	var hits int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
	}))
	defer target.Close()
	p := serve(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	})
	if err := p.Verify(context.Background(), creds()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("got %v", err)
	}
	if atomic.LoadInt32(&hits) != 0 {
		t.Fatal("followed a redirect with credentials attached")
	}
	bad := map[string]string{"apiKey": "a\r\nX-Evil: 1", "secretKey": "b"}
	if err := p.Verify(context.Background(), bad); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("header injection not rejected: %v", err)
	}
}

func TestSteadfastHintAndIdentity(t *testing.T) {
	p := NewSteadfast("", nil)
	if h := p.Hint(creds()); h != "••••1234" || strings.Contains(h, "key-ABCD") {
		t.Fatalf("hint %q", h)
	}
	if p.Identity(creds()) != "key-ABCD1234" {
		t.Fatal("identity should be the api key")
	}
	if h := p.Hint(map[string]string{"apiKey": "abc"}); h != "••••" {
		t.Fatalf("short key hint %q", h)
	}
}

func TestRegistryOnlyExposesRegistered(t *testing.T) {
	r := NewRegistry(NewSteadfast("", nil))
	if _, ok := r.Get("steadfast"); !ok {
		t.Fatal("steadfast missing")
	}
	if _, ok := r.Get("pathao"); ok {
		t.Fatal("unimplemented provider must not be available")
	}
	if len(r.List()) != 1 {
		t.Fatal("list must contain only implemented providers")
	}
}
