package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"shopbd/backend/internal/httpx"
	"shopbd/backend/internal/mailer"
	"shopbd/backend/internal/middleware"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/otp"
	"shopbd/backend/internal/password"
	"shopbd/backend/internal/repository/memrepo"
	"shopbd/backend/internal/service"
	"shopbd/backend/internal/sms"
	"shopbd/backend/internal/storage"
	"shopbd/backend/internal/uploads"
)

// Profile-picture endpoints, with the fakes from upload_security_test.go.

type avEnv struct {
	srv   http.Handler
	r2    *fakeR2
	store *fakeStore
	users *memrepo.Users
}

func newAvEnv(t *testing.T, withStorage bool) *avEnv {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	e := &avEnv{r2: newFakeR2(), store: newFakeStore(), users: memrepo.NewUsers()}
	for _, id := range []string{"sellerA", "sellerB"} {
		if err := e.users.Create(context.Background(), &models.User{
			ID: id, Name: id, Email: id + "@example.com", Role: models.RoleSeller, Status: models.StatusActive,
		}); err != nil {
			t.Fatal(err)
		}
	}
	svc := service.NewPersonalInfoService(e.users, password.New(password.Params{Memory: 8, Time: 1, Threads: 1, KeyLen: 32, SaltLen: 16}),
		otp.NewMemory(), mailer.Noop{}, sms.NotConfigured{}, []byte(strings.Repeat("k", 40)))
	h := &PersonalInfoHandler{Svc: svc, Log: log}
	if withStorage {
		h.R2, h.Uploads = e.r2, e.store
	}
	auth := fakeAuth{
		tokA: {UserID: "sellerA", Role: models.RoleSeller},
		tokB: {UserID: "sellerB", Role: models.RoleSeller},
	}
	r := chi.NewRouter()
	r.Route("/api/v1/seller", func(s chi.Router) {
		s.Use(middleware.CSRF([]string{origin}, log))
		s.Use(middleware.Authenticate(auth, log))
		s.Put("/personal/avatar", h.SetAvatar)
		s.Delete("/personal/avatar", h.RemoveAvatar)
		s.Get("/personal", h.Get)
	})
	e.srv = r
	return e
}

// stored puts an approved (or other) upload record + R2 object in place.
func (e *avEnv) stored(t *testing.T, owner, id, kind, status string) string {
	t.Helper()
	key, err := storage.BuildKey(storage.Kinds[kind], owner, "webp")
	if err != nil {
		t.Fatal(err)
	}
	_ = e.store.Insert(context.Background(), &uploads.Upload{ID: id, OwnerID: owner, Kind: kind, Key: key, Status: status})
	_ = e.r2.PutObject(context.Background(), key, "image/webp", []byte("x"))
	return key
}

func (e *avEnv) call(t *testing.T, method, tok string, body any) (int, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, "/api/v1/seller/personal/avatar", rd)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", origin)
	req.Header.Set(middleware.CSRFHeader, "XMLHttpRequest")
	req.AddCookie(&http.Cookie{Name: httpx.SessionCookie, Value: tok})
	rec := httptest.NewRecorder()
	e.srv.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func avatarOf(out map[string]any) string {
	p, _ := out["personal"].(map[string]any)
	s, _ := p["avatarUrl"].(string)
	return s
}

func TestAvatarSetReplaceRemove(t *testing.T) {
	e := newAvEnv(t, true)
	k1 := e.stored(t, "sellerA", "up1", "avatar", uploads.StatusApproved)

	code, out := e.call(t, "PUT", tokA, map[string]string{"uploadId": "up1"})
	if code != 200 || avatarOf(out) != "https://cdn.test/"+k1 {
		t.Fatalf("set = %d %v", code, out)
	}
	if !strings.HasPrefix(k1, "seller_avatar/") {
		t.Fatalf("avatar must use its own folder, got %s", k1)
	}

	// A second picture replaces the first and the old file is deleted.
	k2 := e.stored(t, "sellerA", "up2", "avatar", uploads.StatusApproved)
	code, out = e.call(t, "PUT", tokA, map[string]string{"uploadId": "up2"})
	if code != 200 || avatarOf(out) != "https://cdn.test/"+k2 {
		t.Fatalf("replace = %d %v", code, out)
	}
	if _, ok := e.r2.objs[k1]; ok {
		t.Fatal("old avatar object was not deleted")
	}
	if _, err := e.store.GetOwned(context.Background(), "sellerA", "up1"); err == nil {
		t.Fatal("old upload record was not deleted")
	}
	if _, ok := e.r2.objs[k2]; !ok {
		t.Fatal("new avatar object was deleted")
	}

	// Remove clears it and deletes the file.
	code, out = e.call(t, "DELETE", tokA, nil)
	if code != 200 || avatarOf(out) != "" {
		t.Fatalf("remove = %d %v", code, out)
	}
	if _, ok := e.r2.objs[k2]; ok {
		t.Fatal("removed avatar object still in R2")
	}
	if u, _ := e.users.FindByID(context.Background(), "sellerA"); u.AvatarURL != "" {
		t.Fatalf("stored avatar = %q", u.AvatarURL)
	}
}

func TestAvatarRejectsOtherPeoplesAndWrongKindUploads(t *testing.T) {
	e := newAvEnv(t, true)
	e.stored(t, "sellerB", "theirs", "avatar", uploads.StatusApproved)
	e.stored(t, "sellerA", "storefront", "seller_profile", uploads.StatusApproved) // the STORE image, not a personal one
	e.stored(t, "sellerA", "pending", "avatar", uploads.StatusProcessing)
	e.stored(t, "sellerA", "refused", "avatar", uploads.StatusRejected)

	for id, want := range map[string]int{
		"theirs":     http.StatusNotFound,   // someone else's upload looks like a missing one
		"storefront": http.StatusBadRequest, // other kinds can't be reused
		"pending":    http.StatusBadRequest,
		"refused":    http.StatusBadRequest,
		"missing":    http.StatusNotFound,
		"../x":       http.StatusBadRequest,
	} {
		if code, out := e.call(t, "PUT", tokA, map[string]string{"uploadId": id}); code != want {
			t.Fatalf("%s: got %d want %d (%v)", id, code, want, out)
		}
	}
	if u, _ := e.users.FindByID(context.Background(), "sellerA"); u.AvatarURL != "" {
		t.Fatal("avatar changed by a rejected request")
	}
}

func TestAvatarCannotTakeAURL(t *testing.T) {
	e := newAvEnv(t, true)
	code, _ := e.call(t, "PUT", tokA, map[string]string{"avatarUrl": "https://evil.example/x.png"})
	if code != http.StatusBadRequest && code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown field accepted: %d", code)
	}
	code, _ = e.call(t, "PUT", tokA, map[string]string{"uploadId": ""})
	if code != http.StatusBadRequest {
		t.Fatalf("empty id = %d", code)
	}
}

func TestAvatarOffWithoutStorage(t *testing.T) {
	e := newAvEnv(t, false)
	code, out := e.call(t, "PUT", tokA, map[string]string{"uploadId": "x"})
	if code != http.StatusServiceUnavailable {
		t.Fatalf("got %d %v", code, out)
	}
	// Removing still works when nothing was ever stored.
	if code, _ := e.call(t, "DELETE", tokA, nil); code != 200 {
		t.Fatalf("remove without storage = %d", code)
	}
}

func TestAvatarNeverDeletesAFileOutsideTheAvatarFolder(t *testing.T) {
	e := newAvEnv(t, true)
	// The account somehow points at a product image of the same owner.
	prod := e.stored(t, "sellerA", "p1", "product", uploads.StatusApproved)
	_ = e.users.SetAvatar(context.Background(), "sellerA", "https://cdn.test/"+prod)
	code, _ := e.call(t, "DELETE", tokA, nil)
	if code != 200 {
		t.Fatal(code)
	}
	if _, ok := e.r2.objs[prod]; !ok {
		t.Fatal("a product image was deleted through the avatar endpoint")
	}
	// ...or at somebody else's avatar folder.
	other, _ := storage.BuildKey(storage.Kinds["avatar"], "sellerB", "webp")
	_ = e.r2.PutObject(context.Background(), other, "image/webp", []byte("x"))
	_ = e.users.SetAvatar(context.Background(), "sellerA", "https://cdn.test/"+other)
	e.call(t, "DELETE", tokA, nil)
	if _, ok := e.r2.objs[other]; !ok {
		t.Fatal("another seller's avatar was deleted")
	}
}
