package customdomain

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestAttacher(h http.HandlerFunc) (*VercelAttacher, *httptest.Server) {
	srv := httptest.NewServer(h)
	v := NewVercelAttacher("tok", "prj", "team_1")
	v.BaseURL = srv.URL
	return v, srv
}

func TestVercelAttachRequest(t *testing.T) {
	var uri, auth, body string
	v, srv := newTestAttacher(func(w http.ResponseWriter, r *http.Request) {
		uri, auth = r.URL.RequestURI(), r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		_, _ = w.Write([]byte(`{}`))
	})
	defer srv.Close()

	if err := v.Attach(context.Background(), "shop.example.com"); err != nil {
		t.Fatal(err)
	}
	if uri != "/v10/projects/prj/domains?teamId=team_1" {
		t.Errorf("uri = %q", uri)
	}
	if auth != "Bearer tok" {
		t.Errorf("auth = %q", auth)
	}
	if !strings.Contains(body, "shop.example.com") {
		t.Errorf("body = %q", body)
	}
}

func TestVercelAttachConflict(t *testing.T) {
	onProject := true
	v, srv := newTestAttacher(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":{"code":"domain_already_in_use","message":"in use"}}`))
			return
		}
		if onProject {
			_, _ = w.Write([]byte(`{}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	defer srv.Close()

	if err := v.Attach(context.Background(), "a.example.com"); err != nil {
		t.Fatalf("already on this project should succeed: %v", err)
	}
	onProject = false
	if err := v.Attach(context.Background(), "a.example.com"); err == nil {
		t.Fatal("domain used by another project should fail")
	}
}

func TestVercelDetachMissingIsFine(t *testing.T) {
	v, srv := newTestAttacher(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	defer srv.Close()
	if err := v.Detach(context.Background(), "a.example.com"); err != nil {
		t.Fatal(err)
	}
}
