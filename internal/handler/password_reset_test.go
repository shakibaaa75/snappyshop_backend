package handler

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"shopbd/backend/internal/jobs"
	"shopbd/backend/internal/mailer"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/password"
	"shopbd/backend/internal/ratelimit"
	"shopbd/backend/internal/repository/memrepo"
	"shopbd/backend/internal/service"
)

// resetStubQueue records jobs and does not run them, so a test can see exactly
// what the request path did and nothing more.
type resetStubQueue struct {
	mu   sync.Mutex
	jobs []jobs.Job
}

func (q *resetStubQueue) Enqueue(j jobs.Job) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.jobs = append(q.jobs, j)
	return true
}

func (q *resetStubQueue) count() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.jobs)
}

type resetHarness struct {
	h      *PasswordResetHandler
	queue  *resetStubQueue
	resets *memrepo.PasswordResets
	user   *models.User
}

func newResetHarness(t *testing.T, lim ratelimit.Limiter) *resetHarness {
	t.Helper()
	users := memrepo.NewUsers()
	u := &models.User{ID: models.NewID(), Email: "real@example.com", Status: models.StatusActive,
		CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err := users.Create(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	resets := memrepo.NewPasswordResets()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := service.NewPasswordResetService(users, resets,
		password.New(password.Params{Memory: 8, Time: 1, Threads: 1, KeyLen: 32, SaltLen: 16}),
		mailer.Noop{}, []byte("0123456789abcdef0123456789abcdef"))
	q := &resetStubQueue{}
	svc.SetQueue(q, log)
	return &resetHarness{
		h:      &PasswordResetHandler{Svc: svc, Log: log, Limiter: lim},
		queue:  q,
		resets: resets,
		user:   u,
	}
}

func postJSON(h http.HandlerFunc, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func TestForgotPasswordLooksIdenticalForKnownAndUnknownEmails(t *testing.T) {
	hn := newResetHarness(t, nil)

	known := postJSON(hn.h.ForgotPassword, `{"email":"real@example.com"}`)
	unknown := postJSON(hn.h.ForgotPassword, `{"email":"nobody@example.com"}`)

	if known.Code != http.StatusNoContent || unknown.Code != http.StatusNoContent {
		t.Fatalf("both must be 204, got %d and %d", known.Code, unknown.Code)
	}
	if known.Body.String() != unknown.Body.String() {
		t.Fatalf("bodies differ: %q vs %q", known.Body.String(), unknown.Body.String())
	}
	kh, uh := known.Header(), unknown.Header()
	if len(kh) != len(uh) {
		t.Fatalf("headers differ: %v vs %v", kh, uh)
	}
	for k := range kh {
		if kh.Get(k) != uh.Get(k) {
			t.Fatalf("header %s differs: %q vs %q", k, kh.Get(k), uh.Get(k))
		}
	}
}

func TestForgotPasswordDoesNoAccountWorkOnTheRequestPath(t *testing.T) {
	hn := newResetHarness(t, nil)

	postJSON(hn.h.ForgotPassword, `{"email":"real@example.com"}`)
	postJSON(hn.h.ForgotPassword, `{"email":"nobody@example.com"}`)

	// Exactly one background job per request, whichever address it was.
	if got := hn.queue.count(); got != 2 {
		t.Fatalf("expected 2 queued jobs, got %d", got)
	}
	// And the job has not run: the handler itself touched no account data, so
	// there is no timing difference for an attacker to measure.
	if _, err := hn.resets.FindByUserID(context.Background(), hn.user.ID); err == nil {
		t.Fatal("a reset challenge was created on the request path")
	}
}

func TestForgotPasswordRateLimitedLooksTheSame(t *testing.T) {
	hn := newResetHarness(t, ratelimit.NewMemory())

	var first, limited *httptest.ResponseRecorder
	for i := 0; i < forgotEmailLimit+1; i++ {
		rec := postJSON(hn.h.ForgotPassword, `{"email":"real@example.com"}`)
		if i == 0 {
			first = rec
		}
		limited = rec
	}
	if limited.Code != first.Code || limited.Body.String() != first.Body.String() {
		t.Fatalf("a limited request must look like a normal one: %d %q vs %d %q",
			limited.Code, limited.Body.String(), first.Code, first.Body.String())
	}
	if got := hn.queue.count(); got != forgotEmailLimit {
		t.Fatalf("the limited request must not be processed: %d jobs, want %d", got, forgotEmailLimit)
	}
}

func TestForgotPasswordStillRejectsAnEmptyEmail(t *testing.T) {
	hn := newResetHarness(t, nil)
	rec := postJSON(hn.h.ForgotPassword, `{"email":""}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("empty email should be 422, got %d", rec.Code)
	}
	if hn.queue.count() != 0 {
		t.Fatal("nothing should be queued")
	}
}

func TestResetPasswordRateLimitedByEmail(t *testing.T) {
	hn := newResetHarness(t, ratelimit.NewMemory())
	body := `{"email":"real@example.com","code":"123456","newPassword":"a-new-password"}`

	var last *httptest.ResponseRecorder
	for i := 0; i < confirmEmailLimit+1; i++ {
		last = postJSON(hn.h.ResetPassword, body)
	}
	if last.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 after %d tries, got %d", confirmEmailLimit, last.Code)
	}
}
