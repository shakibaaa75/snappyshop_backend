package httpx

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"shopbd/backend/internal/apperr"
)

type sample struct {
	Name string `json:"name"`
	Age  int    `json:"age"`
}

func decode(t *testing.T, contentType, body string, max int64) (sample, error) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	var s sample
	err := DecodeJSON(httptest.NewRecorder(), req, &s, max)
	return s, err
}

func codeOf(err error) string {
	var ae *apperr.Error
	if errors.As(err, &ae) {
		return ae.Code
	}
	return ""
}

func TestDecodeJSONHappyPath(t *testing.T) {
	s, err := decode(t, "application/json; charset=utf-8", `{"name":"x","age":3}`, 1024)
	if err != nil || s.Name != "x" || s.Age != 3 {
		t.Fatalf("got %+v %v", s, err)
	}
}

func TestDecodeJSONRejections(t *testing.T) {
	cases := []struct {
		name, ct, body, code string
	}{
		{"wrong content type", "text/plain", `{}`, apperr.CodeUnsupportedMedia},
		{"missing content type", "", `{}`, apperr.CodeUnsupportedMedia},
		{"empty body", "application/json", ``, apperr.CodeBadRequest},
		{"malformed", "application/json", `{"name":`, apperr.CodeBadRequest},
		{"trailing data", "application/json", `{"name":"x"} {"name":"y"}`, apperr.CodeBadRequest},
		{"unknown field", "application/json", `{"name":"x","role":"ADMIN"}`, apperr.CodeValidationFailed},
		{"wrong type", "application/json", `{"name":5}`, apperr.CodeValidationFailed},
		{"too large", "application/json", `{"name":"` + strings.Repeat("a", 200) + `"}`, apperr.CodeBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := decode(t, c.ct, c.body, 64)
			if codeOf(err) != c.code {
				t.Fatalf("want %s, got %v", c.code, err)
			}
		})
	}
}

func TestUnknownFieldNamedInError(t *testing.T) {
	_, err := decode(t, "application/json", `{"role":"ADMIN"}`, 1024)
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Fields["role"] == "" {
		t.Fatalf("field name should be reported, got %v", err)
	}
}

func TestWriteErrorFormat(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	WriteError(rec, req, apperr.Validation(map[string]string{"email": "bad"}), nil)
	if rec.Code != 422 {
		t.Errorf("status %d", rec.Code)
	}
	var body struct {
		Error struct {
			Code    string            `json:"code"`
			Message string            `json:"message"`
			Fields  map[string]string `json:"fields"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != "VALIDATION_FAILED" || body.Error.Fields["email"] != "bad" {
		t.Errorf("unexpected body: %s", rec.Body.String())
	}
}

func TestWriteErrorHidesInternalDetails(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteError(rec, httptest.NewRequest(http.MethodGet, "/", nil), errors.New("mongo: connection string mongodb://u:secret@host"), nil)
	if rec.Code != 500 || strings.Contains(rec.Body.String(), "secret") || strings.Contains(rec.Body.String(), "mongo") {
		t.Fatalf("internal error leaked: %d %s", rec.Code, rec.Body.String())
	}
}

func TestRateLimitedSetsRetryAfter(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteError(rec, httptest.NewRequest(http.MethodGet, "/", nil), apperr.RateLimited(2500*time.Millisecond), nil)
	if rec.Code != 429 || rec.Header().Get("Retry-After") != "3" {
		t.Fatalf("status=%d retry-after=%q", rec.Code, rec.Header().Get("Retry-After"))
	}
}

func TestCookiesAttributes(t *testing.T) {
	cs := CookieSettings{Secure: true, Domain: "example.com"}
	rec := httptest.NewRecorder()
	cs.SetSession(rec, "tok", 15*time.Minute)
	cs.SetRefresh(rec, "ref", time.Hour)
	cookies := rec.Result().Cookies()
	if len(cookies) != 2 {
		t.Fatalf("got %d cookies", len(cookies))
	}
	for _, c := range cookies {
		if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || c.Domain != "example.com" {
			t.Errorf("cookie %s has weak attributes: %+v", c.Name, c)
		}
	}
	if cookies[0].Name != "session_token" || cookies[0].Path != "/" || cookies[0].MaxAge != 900 {
		t.Errorf("session cookie wrong: %+v", cookies[0])
	}
	if cookies[1].Name != "refresh_token" || cookies[1].Path != "/api/v1/auth" {
		t.Errorf("refresh cookie wrong: %+v", cookies[1])
	}

	rec = httptest.NewRecorder()
	cs.Clear(rec)
	for _, c := range rec.Result().Cookies() {
		if c.MaxAge >= 0 || c.Value != "" {
			t.Errorf("cookie %s not cleared: %+v", c.Name, c)
		}
	}
}
