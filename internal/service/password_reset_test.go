package service

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/jobs"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/password"
	"shopbd/backend/internal/repository/memrepo"
)

const resetTestEmail = "buyer@example.com"

// recordingUsers wraps the in-memory users so a test can see which password
// hash was stored; memrepo's own UpdatePassword does nothing.
type recordingUsers struct {
	*memrepo.Users
	mu      sync.Mutex
	updated map[string]string
}

func (r *recordingUsers) UpdatePassword(_ context.Context, userID, hash string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.updated[userID] = hash
	return nil
}

func (r *recordingUsers) hashFor(userID string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.updated[userID]
}

// resetMail is a fake mailer. The first failFirst calls fail like an SMTP
// server that is down; later calls are recorded.
type resetMail struct {
	mu        sync.Mutex
	failFirst int
	calls     int
	to        []string
	text      []string
}

func (m *resetMail) Send(_ context.Context, to, _, _, text string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	if m.calls <= m.failFirst {
		return errors.New("smtp: 550 mailbox unavailable for secret.person@example.com")
	}
	m.to = append(m.to, to)
	m.text = append(m.text, text)
	return nil
}

func (m *resetMail) snapshot() (calls int, to, text []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls, append([]string(nil), m.to...), append([]string(nil), m.text...)
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

type resetFixture struct {
	svc    *PasswordResetService
	mail   *resetMail
	users  *recordingUsers
	resets *memrepo.PasswordResets
	user   *models.User
	code   string
}

// newResetFixtureRaw builds a service and a user but requests no reset yet.
func newResetFixtureRaw(t *testing.T) *resetFixture {
	t.Helper()
	users := &recordingUsers{Users: memrepo.NewUsers(), updated: map[string]string{}}
	u := &models.User{
		ID: models.NewID(), Email: resetTestEmail, Status: models.StatusActive,
		PasswordHash: "old-hash", CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := users.Create(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	f := &resetFixture{users: users, resets: memrepo.NewPasswordResets(), mail: &resetMail{}, user: u, code: "424242"}
	f.svc = NewPasswordResetService(users, f.resets, password.New(password.Params{Memory: 8, Time: 1, Threads: 1, KeyLen: 32, SaltLen: 16}),
		f.mail, []byte("0123456789abcdef0123456789abcdef"))
	f.svc.newCode = func() (string, error) { return f.code, nil }
	return f
}

// newResetFixture is the raw fixture with a reset already requested (sent
// inline, since no queue is set).
func newResetFixture(t *testing.T) *resetFixture {
	t.Helper()
	f := newResetFixtureRaw(t)
	if err := f.svc.RequestReset(context.Background(), resetTestEmail); err != nil {
		t.Fatal(err)
	}
	return f
}

func wantInvalidReset(t *testing.T, err error) {
	t.Helper()
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code != apperr.CodeValidationFailed || ae.Fields["code"] == "" {
		t.Fatalf("expected the generic invalid-code error, got %v", err)
	}
}

func TestResetHappyPathAndSingleUse(t *testing.T) {
	ctx := context.Background()
	f := newResetFixture(t)

	if err := f.svc.ConfirmReset(ctx, resetTestEmail, f.code, "a-new-password"); err != nil {
		t.Fatalf("ConfirmReset: %v", err)
	}
	if f.users.hashFor(f.user.ID) == "" {
		t.Fatal("password was not changed")
	}
	// The same correct code must not work a second time.
	wantInvalidReset(t, f.svc.ConfirmReset(ctx, resetTestEmail, f.code, "another-password"))
}

func TestResetAllowsFiveWrongGuessesThenKillsTheCode(t *testing.T) {
	ctx := context.Background()
	f := newResetFixture(t)

	for i := 1; i <= resetMaxAttempts; i++ {
		wantInvalidReset(t, f.svc.ConfirmReset(ctx, resetTestEmail, "000000", "a-new-password"))
	}
	// The correct code is now refused: the challenge is gone.
	wantInvalidReset(t, f.svc.ConfirmReset(ctx, resetTestEmail, f.code, "a-new-password"))
	if f.users.hashFor(f.user.ID) != "" {
		t.Fatal("password must not change after the attempts ran out")
	}
}

func TestResetCorrectCodeOnFifthAttemptStillWorks(t *testing.T) {
	ctx := context.Background()
	f := newResetFixture(t)
	for i := 1; i < resetMaxAttempts; i++ {
		wantInvalidReset(t, f.svc.ConfirmReset(ctx, resetTestEmail, "000000", "a-new-password"))
	}
	if err := f.svc.ConfirmReset(ctx, resetTestEmail, f.code, "a-new-password"); err != nil {
		t.Fatalf("5th attempt with the right code should succeed: %v", err)
	}
}

func TestResetNewRequestGivesFreshAttempts(t *testing.T) {
	ctx := context.Background()
	f := newResetFixture(t)
	for i := 0; i < resetMaxAttempts; i++ {
		_ = f.svc.ConfirmReset(ctx, resetTestEmail, "000000", "a-new-password")
	}
	f.code = "777777"
	if err := f.svc.RequestReset(ctx, resetTestEmail); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.ConfirmReset(ctx, resetTestEmail, "777777", "a-new-password"); err != nil {
		t.Fatalf("new challenge should work: %v", err)
	}
}

func TestResetParallelGuessesCannotExceedLimit(t *testing.T) {
	ctx := context.Background()
	f := newResetFixture(t)

	var wg sync.WaitGroup
	var evaluated int32
	// 50 parallel wrong guesses: the repository must hand out counts 1..50
	// once each, so at most resetMaxAttempts of them reach the comparison.
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pr, err := f.resets.FindByUserID(ctx, f.user.ID)
			if err != nil {
				return
			}
			if n, err := f.resets.IncrementAttempts(ctx, pr.ID); err == nil && n <= resetMaxAttempts {
				atomic.AddInt32(&evaluated, 1)
			}
		}()
	}
	wg.Wait()
	if evaluated != resetMaxAttempts {
		t.Fatalf("exactly %d guesses may be evaluated, got %d", resetMaxAttempts, evaluated)
	}
}

func TestResetParallelCorrectCodeSucceedsOnce(t *testing.T) {
	ctx := context.Background()
	f := newResetFixture(t)

	var wg sync.WaitGroup
	var ok int32
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if f.svc.ConfirmReset(ctx, resetTestEmail, f.code, "a-new-password") == nil {
				atomic.AddInt32(&ok, 1)
			}
		}()
	}
	wg.Wait()
	if ok != 1 {
		t.Fatalf("a code must work exactly once, %d requests succeeded", ok)
	}
}

func TestResetExpiredCodeIsRefused(t *testing.T) {
	ctx := context.Background()
	f := newResetFixture(t)
	f.svc.now = func() time.Time { return time.Now().Add(resetCodeTTL + time.Second) }
	wantInvalidReset(t, f.svc.ConfirmReset(ctx, resetTestEmail, f.code, "a-new-password"))
}

func TestResetShortPasswordDoesNotRevealCodeValidity(t *testing.T) {
	ctx := context.Background()
	f := newResetFixture(t)
	right := f.svc.ConfirmReset(ctx, resetTestEmail, f.code, "short")
	wrong := f.svc.ConfirmReset(ctx, resetTestEmail, "000000", "short")
	var a, b *apperr.Error
	if !errors.As(right, &a) || !errors.As(wrong, &b) || a.Fields["password"] == "" || b.Fields["password"] == "" {
		t.Fatalf("both must fail the same way on the password: %v / %v", right, wrong)
	}
	// And the short-password attempts must not have used up the real code.
	if err := f.svc.ConfirmReset(ctx, resetTestEmail, f.code, "a-new-password"); err != nil {
		t.Fatalf("code should still be usable: %v", err)
	}
}

func TestResetUnknownEmailAndMalformedCodeLookTheSame(t *testing.T) {
	ctx := context.Background()
	f := newResetFixture(t)
	wantInvalidReset(t, f.svc.ConfirmReset(ctx, "nobody@example.com", "123456", "a-new-password"))
	wantInvalidReset(t, f.svc.ConfirmReset(ctx, resetTestEmail, "12ab", "a-new-password"))
}

func TestResetCodeHashIsKeyedAndBound(t *testing.T) {
	a := newResetFixture(t).svc
	b := NewPasswordResetService(nil, nil, nil, nil, []byte("another-secret-another-secret-12"))
	h := a.hashCode("u1", "r1", "123456")
	if h == b.hashCode("u1", "r1", "123456") {
		t.Fatal("hash must depend on the server key")
	}
	if h == a.hashCode("u2", "r1", "123456") || h == a.hashCode("u1", "r2", "123456") {
		t.Fatal("hash must be bound to the user and the challenge")
	}
}

func TestGenerateCodeIsSixDigits(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		c, err := generateCode()
		if err != nil || !isSixDigits(c) {
			t.Fatalf("bad code %q: %v", c, err)
		}
		seen[c] = true
	}
	if len(seen) < 150 {
		t.Fatalf("codes are not varied enough: %d distinct of 200", len(seen))
	}
}

// ---- account enumeration: unknown, suspended and real accounts ----

func TestRequestResetSendsNothingForUnknownOrSuspendedAccounts(t *testing.T) {
	ctx := context.Background()
	f := newResetFixtureRaw(t)

	if err := f.svc.RequestReset(ctx, "nobody@example.com"); err != nil {
		t.Fatalf("unknown email must not error: %v", err)
	}
	if err := f.svc.RequestReset(ctx, "   "); err != nil {
		t.Fatalf("blank email must not error: %v", err)
	}
	f.users.SetStatus(f.user.ID, models.StatusSuspended)
	if err := f.svc.RequestReset(ctx, resetTestEmail); err != nil {
		t.Fatalf("suspended account must not error: %v", err)
	}
	if calls, _, _ := f.mail.snapshot(); calls != 0 {
		t.Fatalf("no email may be sent, got %d", calls)
	}
	if _, err := f.resets.FindByUserID(ctx, f.user.ID); err == nil {
		t.Fatal("no challenge may be created for a suspended account")
	}
}

func TestRequestResetEmailsTheCodeToTheAccount(t *testing.T) {
	f := newResetFixture(t)
	calls, to, text := f.mail.snapshot()
	if calls != 1 || to[0] != resetTestEmail || !strings.Contains(text[0], f.code) {
		t.Fatalf("expected one email with the code to %s, got calls=%d to=%v", resetTestEmail, calls, to)
	}
}

// ---- background delivery ----

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestAsyncRequestRetriesSMTPFailureWithTheSameCode(t *testing.T) {
	ctx := context.Background()
	f := newResetFixtureRaw(t)
	f.mail.failFirst = 2
	f.svc.retryBackoff = []time.Duration{time.Millisecond}
	q := jobs.New(2, 8, quietLog())
	defer q.Shutdown(ctx)
	f.svc.SetQueue(q, quietLog())

	f.svc.RequestResetAsync(resetTestEmail)

	eventually(t, "the email to arrive after two SMTP failures", func() bool {
		_, to, _ := f.mail.snapshot()
		return len(to) == 1
	})
	calls, _, text := f.mail.snapshot()
	if calls != 3 || !strings.Contains(text[0], f.code) {
		t.Fatalf("calls=%d, email must carry the original code", calls)
	}
	// The code that was finally delivered is the one that works: retries did
	// not issue a second code and invalidate the first.
	if err := f.svc.ConfirmReset(ctx, resetTestEmail, f.code, "a-new-password"); err != nil {
		t.Fatalf("delivered code should work: %v", err)
	}
}

func TestAsyncRequestNeverSurfacesSMTPFailure(t *testing.T) {
	ctx := context.Background()
	f := newResetFixtureRaw(t)
	f.mail.failFirst = 1000 // the mail server is down for good
	f.svc.retryBackoff = []time.Duration{time.Millisecond}
	q := jobs.New(1, 8, quietLog())
	defer q.Shutdown(ctx)
	f.svc.SetQueue(q, quietLog())

	// RequestResetAsync has no return value at all: there is nothing for a
	// caller to see, whatever the mail server does.
	f.svc.RequestResetAsync(resetTestEmail)

	eventually(t, "all delivery attempts", func() bool {
		calls, _, _ := f.mail.snapshot()
		return calls == resetEmailAttempts
	})
	time.Sleep(30 * time.Millisecond)
	if calls, to, _ := f.mail.snapshot(); calls != resetEmailAttempts || len(to) != 0 {
		t.Fatalf("expected exactly %d failed attempts and no delivery, got calls=%d", resetEmailAttempts, calls)
	}
}

type fullQueue struct{ offered int32 }

func (q *fullQueue) Enqueue(jobs.Job) bool { atomic.AddInt32(&q.offered, 1); return false }

func TestAsyncRequestWithFullQueueDoesNotPanicOrBlock(t *testing.T) {
	f := newResetFixtureRaw(t)
	fq := &fullQueue{}
	f.svc.SetQueue(fq, quietLog())
	done := make(chan struct{})
	go func() { f.svc.RequestResetAsync(resetTestEmail); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("RequestResetAsync blocked on a full queue")
	}
	if atomic.LoadInt32(&fq.offered) != 1 {
		t.Fatal("the request should have been offered to the queue once")
	}
}
