package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestEtagMatches(t *testing.T) {
	cases := []struct {
		header, etag string
		want         bool
	}{
		{"", `"abc"`, false},
		{`"abc"`, `"abc"`, true},
		{`W/"abc"`, `"abc"`, true},
		{`"x", "abc"`, `"abc"`, true},
		{`"x"`, `"abc"`, false},
		{`*`, `"abc"`, true},
	}
	for _, c := range cases {
		if got := etagMatches(c.header, c.etag); got != c.want {
			t.Errorf("etagMatches(%q,%q)=%v want %v", c.header, c.etag, got, c.want)
		}
	}
}

func TestWriteCachedJSON(t *testing.T) {
	body := map[string]string{"a": "b"}

	// First request: 200 with cache headers and an ETag.
	rec := httptest.NewRecorder()
	writeCachedJSON(rec, httptest.NewRequest(http.MethodGet, "/x", nil), body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != storefrontCacheControl {
		t.Fatalf("Cache-Control = %q", got)
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("missing ETag")
	}

	// Same copy again: 304 with no body.
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("If-None-Match", etag)
	rec2 := httptest.NewRecorder()
	writeCachedJSON(rec2, req, body)
	if rec2.Code != http.StatusNotModified {
		t.Fatalf("want 304, got %d", rec2.Code)
	}
	if rec2.Body.Len() != 0 {
		t.Fatal("304 must have no body")
	}

	// Changed content: new ETag, full 200.
	req3 := httptest.NewRequest(http.MethodGet, "/x", nil)
	req3.Header.Set("If-None-Match", etag)
	rec3 := httptest.NewRecorder()
	writeCachedJSON(rec3, req3, map[string]string{"a": "changed"})
	if rec3.Code != http.StatusOK {
		t.Fatalf("want 200 for changed body, got %d", rec3.Code)
	}
}

func TestParsePaging(t *testing.T) {
	cases := []struct {
		url         string
		page, limit int
	}{
		{"/p", 1, publicProductsDefaultLimit},
		{"/p?page=3&limit=48", 3, 48},
		{"/p?page=0&limit=0", 1, publicProductsDefaultLimit},
		{"/p?page=abc&limit=-5", 1, publicProductsDefaultLimit},
		{"/p?limit=9999", 1, publicProductsMaxLimit},
		{"/p?page=999999", publicProductsMaxPage, publicProductsDefaultLimit},
	}
	for _, c := range cases {
		page, limit := parsePaging(httptest.NewRequest(http.MethodGet, c.url, nil))
		if page != c.page || limit != c.limit {
			t.Errorf("%s: got page=%d limit=%d want page=%d limit=%d", c.url, page, limit, c.page, c.limit)
		}
	}
}

func TestStoreDesignTagIsPerStore(t *testing.T) {
	if storeDesignTag("a") == storeDesignTag("b") {
		t.Fatal("two stores must not share a design cache tag")
	}
	if storeDesignTag("a") == slugsTag {
		t.Fatal("design tag must differ from the shared slugs tag")
	}
}
