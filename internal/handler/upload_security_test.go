package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.mongodb.org/mongo-driver/bson"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/httpx"
	"shopbd/backend/internal/middleware"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/plans"
	"shopbd/backend/internal/service"
	"shopbd/backend/internal/storage"
	"shopbd/backend/internal/uploads"
)

/* ----------------------------- fakes ----------------------------------- */

type fakeR2 struct {
	mu      sync.Mutex
	objs    map[string][]byte
	types   map[string]string
	putGate chan struct{} // when set, PutObject blocks until it is closed
	putErr  error
}

func newFakeR2() *fakeR2 { return &fakeR2{objs: map[string][]byte{}, types: map[string]string{}} }
func (f *fakeR2) PutObject(ctx context.Context, key, ct string, b []byte) error {
	if f.putGate != nil {
		<-f.putGate
	}
	if f.putErr != nil {
		return f.putErr
	}
	if !storage.ValidKey(key) {
		return errors.New("bad key")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objs[key], f.types[key] = append([]byte{}, b...), ct
	return nil
}
func (f *fakeR2) DeleteObject(ctx context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.objs, key)
	return nil
}
func (f *fakeR2) PublicURL(key string) string { return "https://cdn.test/" + key }
func (f *fakeR2) PresignPut(key, ct string, ttl time.Duration) string {
	return "https://r2.test/" + key + "?sig=1"
}
func (f *fakeR2) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.objs) }

// fakeStore mirrors the owner-scoped filters of uploads.Store.
type fakeStore struct {
	mu               sync.Mutex
	recs             map[string]*uploads.Upload
	usageBytes       int64
	usageFiles       int
	usageRecent      int64
	rejectedLastHour int64
}

func newFakeStore() *fakeStore { return &fakeStore{recs: map[string]*uploads.Upload{}} }
func (s *fakeStore) Insert(_ context.Context, u *uploads.Upload) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := *u
	s.recs[u.ID] = &c
	return nil
}
func (s *fakeStore) Finish(_ context.Context, id, st, reason string, set bson.M) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.recs[id]
	if r == nil || r.Status != uploads.StatusProcessing {
		return uploads.ErrNotFound
	}
	r.Status, r.Reason = st, reason
	if v, ok := set["bytes"].(int64); ok {
		r.Bytes = v
	}
	return nil
}
func (s *fakeStore) GetOwned(_ context.Context, owner, id string) (*uploads.Upload, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.recs[id]; r != nil && r.OwnerID == owner {
		c := *r
		return &c, nil
	}
	return nil, uploads.ErrNotFound
}
func (s *fakeStore) GetByKeyOwned(_ context.Context, owner, key string) (*uploads.Upload, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.recs {
		if r.Key == key && r.OwnerID == owner && r.Status == uploads.StatusApproved {
			c := *r
			return &c, nil
		}
	}
	return nil, uploads.ErrNotFound
}
func (s *fakeStore) Delete(_ context.Context, owner, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.recs[id]; r != nil && r.OwnerID == owner {
		delete(s.recs, id)
		return nil
	}
	return uploads.ErrNotFound
}
func (s *fakeStore) Usage(context.Context, string, time.Time) (int64, int, int64, error) {
	return s.usageBytes, s.usageFiles, s.usageRecent, nil
}
func (s *fakeStore) RejectedSince(context.Context, string, time.Time) (int64, error) {
	return s.rejectedLastHour, nil
}
func (s *fakeStore) status(st string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.recs {
		if r.Status == st {
			n++
		}
	}
	return n
}

type fakeAuth map[string]*service.Claims

func (a fakeAuth) AuthenticateAccess(tok string) (*service.Claims, error) {
	if c, ok := a[tok]; ok {
		return c, nil
	}
	return nil, apperr.Unauthenticated("Invalid or expired session")
}

type fakeProducts map[string]string // productID -> sellerID

func (p fakeProducts) FindByID(_ context.Context, seller, id string) (*models.Product, error) {
	if p[id] == seller {
		return &models.Product{}, nil
	}
	return nil, errors.New("not found")
}

type fakeScanner struct {
	clean bool
	err   error
}

func (f fakeScanner) Scan(context.Context, []byte) (bool, string, error) {
	return f.clean, "Eicar-Test-Signature", f.err
}

/* ----------------------------- harness --------------------------------- */

const (
	tokA, tokB, tokBuyer = "TOKEN-SELLER-A-SECRET", "TOKEN-SELLER-B-SECRET", "TOKEN-BUYER-SECRET"
	origin               = "https://shop.example.com"
)

type env struct {
	srv   http.Handler
	r2    *fakeR2
	store *fakeStore
	h     *UploadHandler
	logs  *bytes.Buffer
}

type planFn func(context.Context, string) (plans.Plan, error)

func (f planFn) PlanFor(c context.Context, id string) (plans.Plan, error) { return f(c, id) }

func newEnv(t *testing.T) *env {
	t.Helper()
	logs := &bytes.Buffer{}
	log := slog.New(slog.NewTextHandler(logs, nil))
	e := &env{r2: newFakeR2(), store: newFakeStore(), logs: logs}
	e.h = &UploadHandler{
		R2: e.r2, Store: e.store, Log: log,
		Products:  fakeProducts{"prodA": "sellerA", "prodB": "sellerB"},
		UserLimit: middleware.NewKeyedLimiter(time.Millisecond, 1000),
	}
	auth := fakeAuth{
		tokA:     {UserID: "sellerA", Role: models.RoleSeller},
		tokB:     {UserID: "sellerB", Role: models.RoleSeller},
		tokBuyer: {UserID: "buyer1", Role: models.RoleBuyer},
	}
	r := chi.NewRouter()
	r.Route("/api/v1", func(api chi.Router) {
		api.Use(middleware.CSRF([]string{origin}, log)) // same wiring as router.go
		api.Route("/seller", func(s chi.Router) {
			s.Use(middleware.Authenticate(auth, log))
			s.Use(middleware.RequireRole(log, models.RoleSeller))
			s.Post("/uploads", e.h.Upload)
			s.Delete("/uploads/{id}", e.h.Delete)
			s.Delete("/uploads", e.h.DeleteByKey)
			s.Post("/uploads/presign", e.h.Presign)
		})
	})
	e.srv = r
	return e
}

func pngData(t testing.TB, w, h int) []byte {
	im := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			im.Set(x, y, color.NRGBA{uint8(x), uint8(y), 99, 255})
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, im); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

type form struct {
	fields          map[string]string
	filename, ctype string
	data            []byte
}

func (f form) body() (*bytes.Buffer, string) {
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	for k, v := range f.fields {
		_ = mw.WriteField(k, v)
	}
	h := textproto.MIMEHeader{}
	fn := f.filename
	if fn == "" {
		fn = "photo.png"
	}
	h.Set("Content-Disposition", `form-data; name="file"; filename="`+fn+`"`)
	if f.ctype != "" {
		h.Set("Content-Type", f.ctype)
	}
	p, _ := mw.CreatePart(h)
	p.Write(f.data)
	mw.Close()
	return &b, mw.FormDataContentType()
}

type opts struct {
	token, origin string
	noCSRFHeader  bool
}

func (e *env) do(t *testing.T, method, path string, body io.Reader, ctype string, o opts) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, body)
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	if o.token != "" {
		req.AddCookie(&http.Cookie{Name: httpx.SessionCookie, Value: o.token})
	}
	if o.origin != "" {
		req.Header.Set("Origin", o.origin)
	}
	if !o.noCSRFHeader {
		req.Header.Set(middleware.CSRFHeader, "XMLHttpRequest")
	}
	rec := httptest.NewRecorder()
	e.srv.ServeHTTP(rec, req)
	return rec
}

func (e *env) upload(t *testing.T, f form, o opts) *httptest.ResponseRecorder {
	if f.fields == nil {
		f.fields = map[string]string{"kind": "product"}
	}
	b, ct := f.body()
	return e.do(t, http.MethodPost, "/api/v1/seller/uploads", b, ct, o)
}

func okA() opts { return opts{token: tokA, origin: origin} }
func okB() opts { return opts{token: tokB, origin: origin} }

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	var m map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	return m
}

/* ------------------------------ tests ---------------------------------- */

func TestUploadHappyPathStoresCleanWebPUnderRandomOwnerKey(t *testing.T) {
	e := newEnv(t)
	// Hostile filename and part content type must be ignored.
	rec := e.upload(t, form{data: pngData(t, 50, 40), filename: "../../etc/evil.php", ctype: "text/html"}, okA())
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	m := decode(t, rec)
	key := m["key"].(string)
	if !strings.HasPrefix(key, "products_images/sellerA/") || !strings.HasSuffix(key, ".webp") || !storage.ValidKey(key) {
		t.Fatalf("bad key %q", key)
	}
	if strings.Contains(key, "evil") || strings.Contains(key, "photo") {
		t.Fatal("client filename leaked into key")
	}
	stored := e.r2.objs[key]
	if string(stored[:4]) != "RIFF" || string(stored[8:12]) != "WEBP" || e.r2.types[key] != "image/webp" {
		t.Fatal("stored object is not a WebP with image/webp content type")
	}
	if bytes.Contains(stored, pngData(t, 50, 40)[:16]) {
		t.Fatal("original bytes uploaded")
	}
	if e.store.status(uploads.StatusApproved) != 1 || m["publicUrl"] != "https://cdn.test/"+key {
		t.Fatal("record/response wrong")
	}
	// Keys are unique and unguessable.
	rec2 := e.upload(t, form{data: pngData(t, 50, 40)}, okA())
	if decode(t, rec2)["key"] == key {
		t.Fatal("key reused")
	}
}

func TestMaliciousUploadsAreRejectedAndNeverStored(t *testing.T) {
	e := newEnv(t)
	hdrWebP := append([]byte("RIFF\x00\x00\x00\x00WEBPVP8 "), []byte(`<script>alert(document.cookie)</script>`)...)
	cases := []struct {
		name  string
		data  []byte
		ctype string
		fn    string
		want  int
	}{
		{"svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"></svg>`), "image/svg+xml", "a.svg", 415},
		{"svg-as-png-mime", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>1</script></svg>`), "image/png", "a.png", 415},
		{"html-as-webp", []byte(`<html><script>alert(1)</script></html>`), "image/webp", "a.webp", 415},
		{"php-renamed-jpg", []byte("<?php system($_GET[0]);"), "image/jpeg", "shell.php.jpg", 415},
		{"exe-renamed-png", append([]byte("MZ\x90\x00"), make([]byte, 200)...), "image/png", "setup.png", 415},
		{"elf", append([]byte("\x7fELF"), make([]byte, 200)...), "image/png", "x.png", 415},
		{"fake-webp-html", hdrWebP, "image/webp", "a.webp", 400},
		{"truncated-png", pngData(t, 80, 80)[:100], "image/png", "a.png", 400},
		{"gif", []byte("GIF89a\x01\x00\x01\x00\x00\x00\x00;"), "image/gif", "a.gif", 415},
	}
	for _, c := range cases {
		rec := e.upload(t, form{data: c.data, ctype: c.ctype, filename: c.fn}, okA())
		if rec.Code != c.want {
			t.Errorf("%s: status %d want %d (%s)", c.name, rec.Code, c.want, rec.Body)
		}
		if strings.Contains(rec.Body.String(), "<script") {
			t.Errorf("%s: response echoes payload", c.name)
		}
	}
	if e.r2.count() != 0 {
		t.Fatalf("%d hostile objects reached R2", e.r2.count())
	}
	if e.store.status(uploads.StatusRejected) != len(cases) || e.store.status(uploads.StatusApproved) != 0 {
		t.Fatalf("rejected=%d approved=%d", e.store.status(uploads.StatusRejected), e.store.status(uploads.StatusApproved))
	}
	if !strings.Contains(e.logs.String(), "security: upload_rejected") {
		t.Fatal("no security log for rejected uploads")
	}
}

func TestOversizeAndBombRejectedBeforeProcessing(t *testing.T) {
	e := newEnv(t)
	// 6 MB with a Content-Length: refused before reading.
	big := append(pngData(t, 10, 10), make([]byte, 6<<20)...)
	if rec := e.upload(t, form{data: big}, okA()); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("6MB: %d", rec.Code)
	}
	// Same, but sent chunked with no Content-Length: cap enforced while reading.
	b, ct := form{fields: map[string]string{"kind": "product"}, data: big}.body()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/seller/uploads", io.NopCloser(b))
	req.ContentLength = -1
	req.Header.Set("Content-Type", ct)
	req.Header.Set("Origin", origin)
	req.Header.Set(middleware.CSRFHeader, "x")
	req.AddCookie(&http.Cookie{Name: httpx.SessionCookie, Value: tokA})
	rec := httptest.NewRecorder()
	e.srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("chunked 6MB: %d", rec.Code)
	}
	// Per-kind limit (seller_profile = 2 MB).
	two := append(pngData(t, 10, 10), make([]byte, 3<<20)...)
	if rec := e.upload(t, form{fields: map[string]string{"kind": "seller_profile"}, data: two}, okA()); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("kind limit: %d", rec.Code)
	}
	if e.r2.count() != 0 {
		t.Fatal("oversized file reached R2")
	}
}

func TestAuthRoleAndCSRF(t *testing.T) {
	e := newEnv(t)
	f := form{data: pngData(t, 20, 20)}
	if rec := e.upload(t, f, opts{origin: origin}); rec.Code != 401 {
		t.Errorf("no cookie: %d", rec.Code)
	}
	if rec := e.upload(t, f, opts{token: "forged", origin: origin}); rec.Code != 401 {
		t.Errorf("forged token: %d", rec.Code)
	}
	if rec := e.upload(t, f, opts{token: tokBuyer, origin: origin}); rec.Code != 403 {
		t.Errorf("buyer role: %d", rec.Code)
	}
	if rec := e.upload(t, f, opts{token: tokA, origin: "https://evil.example.com"}); rec.Code != 403 {
		t.Errorf("cross-site origin: %d", rec.Code)
	}
	if rec := e.upload(t, f, opts{token: tokA, origin: origin, noCSRFHeader: true}); rec.Code != 403 {
		t.Errorf("missing CSRF header: %d", rec.Code)
	}
	// A plain HTML form post from another site cannot add the header either.
	if rec := e.upload(t, f, opts{token: tokA, origin: "null", noCSRFHeader: true}); rec.Code != 403 {
		t.Errorf("null origin: %d", rec.Code)
	}
	if e.r2.count() != 0 || len(e.store.recs) != 0 {
		t.Fatal("unauthorized request created data")
	}
	// Wrong content type / not multipart.
	if rec := e.do(t, http.MethodPost, "/api/v1/seller/uploads", strings.NewReader(`{"kind":"product"}`), "application/json", okA()); rec.Code != 415 {
		t.Errorf("json body: %d", rec.Code)
	}
}

func TestCrossSellerDeleteAndTraversal(t *testing.T) {
	e := newEnv(t)
	rec := e.upload(t, form{data: pngData(t, 20, 20)}, okA())
	m := decode(t, rec)
	id, key := m["id"].(string), m["key"].(string)

	// Seller B cannot delete A's image by id or by key.
	if r := e.do(t, "DELETE", "/api/v1/seller/uploads/"+id, nil, "", okB()); r.Code != 404 {
		t.Errorf("B deletes A by id: %d", r.Code)
	}
	if r := e.do(t, "DELETE", "/api/v1/seller/uploads?key="+key, nil, "", okB()); r.Code != 404 {
		t.Errorf("B deletes A by key: %d", r.Code)
	}
	if e.r2.count() != 1 || len(e.store.recs) != 1 {
		t.Fatal("cross-seller delete succeeded")
	}
	// Traversal / odd keys.
	for _, k := range []string{"../sellerA/x", "products_images/sellerB/../sellerA/" + key[strings.LastIndex(key, "/")+1:], "%2e%2e/%2e%2e/x", "products_images/sellerA/" + strings.Repeat("a", 32) + ".webp%00.php"} {
		if r := e.do(t, "DELETE", "/api/v1/seller/uploads?key="+k, nil, "", okA()); r.Code != 404 {
			t.Errorf("traversal key %q: %d", k, r.Code)
		}
	}
	if r := e.do(t, "DELETE", "/api/v1/seller/uploads/..%2f..%2fx", nil, "", okA()); r.Code != 404 {
		t.Errorf("traversal id: %d", r.Code)
	}
	// Owner can delete; object and record disappear.
	if r := e.do(t, "DELETE", "/api/v1/seller/uploads/"+id, nil, "", okA()); r.Code != 204 {
		t.Fatalf("owner delete: %d", r.Code)
	}
	if e.r2.count() != 0 || len(e.store.recs) != 0 {
		t.Fatal("owner delete left data behind")
	}
}

func TestLegacyKeyDeleteOnlyInOwnFolder(t *testing.T) {
	e := newEnv(t)
	own := "products_images/sellerA/" + strings.Repeat("a", 32) + ".jpg"
	other := "products_images/sellerB/" + strings.Repeat("b", 32) + ".jpg"
	e.r2.objs[own], e.r2.objs[other] = []byte("x"), []byte("y")
	if r := e.do(t, "DELETE", "/api/v1/seller/uploads?key="+other, nil, "", okA()); r.Code != 404 {
		t.Fatalf("A deletes B's legacy key: %d", r.Code)
	}
	if r := e.do(t, "DELETE", "/api/v1/seller/uploads?key="+own, nil, "", okA()); r.Code != 204 {
		t.Fatalf("A deletes own legacy key: %d", r.Code)
	}
	if _, ok := e.r2.objs[other]; !ok {
		t.Fatal("other seller's object deleted")
	}
}

func TestProductOwnershipAndKinds(t *testing.T) {
	e := newEnv(t)
	d := pngData(t, 20, 20)
	if r := e.upload(t, form{fields: map[string]string{"kind": "product", "productId": "prodB"}, data: d}, okA()); r.Code != 404 {
		t.Errorf("attach to other seller's product: %d", r.Code)
	}
	if r := e.upload(t, form{fields: map[string]string{"kind": "product", "productId": "prodA"}, data: d}, okA()); r.Code != 201 {
		t.Errorf("own product: %d %s", r.Code, r.Body)
	}
	for _, pid := range []string{"../../x", "a b", strings.Repeat("a", 65)} {
		if r := e.upload(t, form{fields: map[string]string{"kind": "product", "productId": pid}, data: d}, okA()); r.Code != 400 {
			t.Errorf("productId %q: %d", pid, r.Code)
		}
	}
	for _, k := range []string{"../x", "banner_video", "nope", ""} {
		if r := e.upload(t, form{fields: map[string]string{"kind": k}, data: d}, okA()); r.Code != 400 {
			t.Errorf("kind %q: %d", k, r.Code)
		}
	}
	// Unknown extra form field is refused.
	if r := e.upload(t, form{fields: map[string]string{"kind": "product", "key": "products_images/sellerB/x.webp"}, data: d}, okA()); r.Code != 400 {
		t.Errorf("extra field: %d", r.Code)
	}
}

func TestPresignNoLongerServesImages(t *testing.T) {
	e := newEnv(t)
	body := `{"kind":"product","contentType":"image/png","size":1000}`
	if r := e.do(t, "POST", "/api/v1/seller/uploads/presign", strings.NewReader(body), "application/json", okA()); r.Code != http.StatusGone {
		t.Fatalf("image presign must be gone, got %d", r.Code)
	}
	body = `{"kind":"banner_video","contentType":"video/mp4","size":1000}`
	if r := e.do(t, "POST", "/api/v1/seller/uploads/presign", strings.NewReader(body), "application/json", okA()); r.Code != 200 {
		t.Fatalf("video presign: %d", r.Code)
	}
}

func TestQuotaAndAbuseLimits(t *testing.T) {
	d := pngData(t, 20, 20)
	e := newEnv(t)
	e.h.Plans = planFn(func(context.Context, string) (plans.Plan, error) { return plans.Plan{ID: plans.Free}, nil })
	e.store.usageBytes = uploads.QuotaFor(plans.Free).Bytes
	if r := e.upload(t, form{data: d}, okA()); r.Code != 403 || decode(t, r)["error"].(map[string]any)["code"] != "PLAN_LIMIT" {
		t.Errorf("storage quota: %d %s", r.Code, r.Body)
	}
	e.store.usageBytes, e.store.usageFiles = 0, uploads.QuotaFor(plans.Free).Files
	if r := e.upload(t, form{data: d}, okA()); r.Code != 403 {
		t.Errorf("file-count quota: %d", r.Code)
	}
	e.store.usageFiles = 0
	e.store.usageRecent = uploads.MaxPerDay
	if r := e.upload(t, form{data: d}, okA()); r.Code != 429 {
		t.Errorf("daily limit: %d", r.Code)
	}
	e.store.usageRecent, e.store.rejectedLastHour = 0, uploads.MaxRejectedHr
	if r := e.upload(t, form{data: d}, okA()); r.Code != 429 {
		t.Errorf("probing block: %d", r.Code)
	}
	e.store.rejectedLastHour = 0
	if r := e.upload(t, form{data: d}, okA()); r.Code != 201 {
		t.Errorf("under limits must work: %d %s", r.Code, r.Body)
	}
	// A bigger plan has a bigger quota.
	e.h.Plans = planFn(func(context.Context, string) (plans.Plan, error) { return plans.Plan{ID: plans.Pro}, nil })
	e.store.usageBytes = uploads.QuotaFor(plans.Free).Bytes
	if r := e.upload(t, form{data: d}, okA()); r.Code != 201 {
		t.Errorf("pro quota: %d", r.Code)
	}
	// Per-user rate limiter.
	e.h.UserLimit = middleware.NewKeyedLimiter(time.Hour, 2)
	codes := []int{}
	for i := 0; i < 4; i++ {
		codes = append(codes, e.upload(t, form{data: d}, okA()).Code)
	}
	if codes[2] != 429 || codes[3] != 429 {
		t.Errorf("per-user rate limit: %v", codes)
	}
	// Another user is unaffected by A's limiter.
	if r := e.upload(t, form{data: d}, okB()); r.Code != 201 {
		t.Errorf("seller B throttled by A: %d", r.Code)
	}
}

func TestConcurrentUploadAbuseIsCappedPerUser(t *testing.T) {
	e := newEnv(t)
	e.r2.putGate = make(chan struct{})
	d := pngData(t, 20, 20)
	var wg sync.WaitGroup
	codes := make(chan int, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); codes <- e.upload(t, form{data: d}, okA()).Code }()
	}
	// Wait until the over-limit requests have been refused, then release the rest.
	deadline := time.Now().Add(5 * time.Second)
	refused := 0
	var got []int
	for refused < 6 && time.Now().Before(deadline) {
		select {
		case c := <-codes:
			got = append(got, c)
			if c == 429 {
				refused++
			}
		case <-time.After(20 * time.Millisecond):
		}
	}
	close(e.r2.putGate)
	wg.Wait()
	close(codes)
	for c := range codes {
		got = append(got, c)
	}
	ok, rl := 0, 0
	for _, c := range got {
		switch c {
		case 201:
			ok++
		case 429:
			rl++
		default:
			t.Errorf("unexpected status %d", c)
		}
	}
	if ok != uploads.MaxInFlight || rl != 8-uploads.MaxInFlight {
		t.Fatalf("8 parallel uploads -> %d ok, %d refused (want %d ok)", ok, rl, uploads.MaxInFlight)
	}
	e.h.mu.Lock()
	left := len(e.h.inflight)
	e.h.mu.Unlock()
	if left != 0 {
		t.Fatal("in-flight counter leaked")
	}
	// B was never blocked by A's concurrency.
	if r := e.upload(t, form{data: d}, okB()); r.Code != 201 {
		t.Fatalf("seller B: %d", r.Code)
	}
}

func TestMalwareScannerFailsClosed(t *testing.T) {
	e := newEnv(t)
	d := pngData(t, 20, 20)
	e.h.Scanner = fakeScanner{clean: false}
	if r := e.upload(t, form{data: d}, okA()); r.Code != 400 {
		t.Errorf("infected: %d", r.Code)
	}
	e.h.Scanner = fakeScanner{err: errors.New("down")}
	if r := e.upload(t, form{data: d}, okA()); r.Code != 503 {
		t.Errorf("scanner down must refuse the upload: %d", r.Code)
	}
	if e.r2.count() != 0 {
		t.Fatal("unscanned/infected file stored")
	}
	e.h.Scanner = fakeScanner{clean: true}
	if r := e.upload(t, form{data: d}, okA()); r.Code != 201 {
		t.Errorf("clean: %d", r.Code)
	}
	if !strings.Contains(e.logs.String(), "security: upload_malware") {
		t.Error("malware detection not logged")
	}
}

func TestR2FailureLeavesNothingServable(t *testing.T) {
	e := newEnv(t)
	e.r2.putErr = errors.New("r2 down: https://x?X-Amz-Signature=SECRETSIG")
	r := e.upload(t, form{data: pngData(t, 20, 20)}, okA())
	if r.Code != http.StatusBadGateway || strings.Contains(r.Body.String(), "SECRETSIG") {
		t.Fatalf("%d %s", r.Code, r.Body)
	}
	if e.store.status(uploads.StatusApproved) != 0 || e.r2.count() != 0 {
		t.Fatal("failed upload left approved record/object")
	}
}

func TestLogsNeverContainCredentials(t *testing.T) {
	e := newEnv(t)
	e.upload(t, form{data: []byte("<?php evil ?>")}, okA())
	e.upload(t, form{data: pngData(t, 20, 20)}, opts{token: "forged-SECRETVALUE", origin: origin})
	e.upload(t, form{data: pngData(t, 20, 20)}, okB())
	for _, s := range []string{tokA, tokB, tokBuyer, "forged-SECRETVALUE", "evil"} {
		if strings.Contains(e.logs.String(), s) {
			t.Errorf("log contains %q", s)
		}
	}
}
