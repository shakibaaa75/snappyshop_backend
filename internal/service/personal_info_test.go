package service

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/mailer"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/otp"
	"shopbd/backend/internal/password"
	"shopbd/backend/internal/redisx"
	"shopbd/backend/internal/repository/memrepo"
	"shopbd/backend/internal/sms"
)

// ---- fakes (names start with "pi" so they never clash with other test files)

type piSent struct{ to, subject, text string }

type piMailer struct {
	mu   sync.Mutex
	sent []piSent
	fail error
	ch   chan piSent
}

func newPiMailer() *piMailer { return &piMailer{ch: make(chan piSent, 32)} }

func (m *piMailer) Send(_ context.Context, to, subject, _ string, text string) error {
	if m.fail != nil {
		return m.fail
	}
	s := piSent{to, subject, text}
	m.mu.Lock()
	m.sent = append(m.sent, s)
	m.mu.Unlock()
	m.ch <- s
	return nil
}

func (m *piMailer) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sent)
}

func (m *piMailer) last() piSent {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sent[len(m.sent)-1]
}

type piSMS struct {
	mu   sync.Mutex
	sent []piSent
	fail error
}

func (s *piSMS) Send(_ context.Context, to, msg string) error {
	if s.fail != nil {
		return s.fail
	}
	s.mu.Lock()
	s.sent = append(s.sent, piSent{to: to, text: msg})
	s.mu.Unlock()
	return nil
}

func (s *piSMS) last() piSent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sent[len(s.sent)-1]
}

func (s *piSMS) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sent)
}

// piBrokenStore fails every call, like Redis being down.
type piBrokenStore struct{ otp.Store }

var errPiDown = errors.New("redis: connection refused")

func (piBrokenStore) Issue(context.Context, otp.Purpose, string, string, string, time.Duration, time.Duration) error {
	return errPiDown
}
func (piBrokenStore) Verify(context.Context, otp.Purpose, string, string, int) (string, error) {
	return "", errPiDown
}
func (piBrokenStore) Cancel(context.Context, otp.Purpose, string) error { return errPiDown }
func (piBrokenStore) Hit(context.Context, string, int, time.Duration) (bool, time.Duration, error) {
	return false, 0, errPiDown
}

// ---- fixture

type piFixture struct {
	svc   *PersonalInfoService
	users *memrepo.Users
	mail  *piMailer
	sms   *piSMS
	clock *time.Time
	u     *models.User // the seller under test (has a password)
}

const piPassword = "correct-horse-1"

var piHasher = password.New(password.Params{Memory: 8, Time: 1, Threads: 1, KeyLen: 32, SaltLen: 16})

func piNewUser(t *testing.T, users *memrepo.Users, id, email string, withPassword bool) *models.User {
	t.Helper()
	return piNewUserV(t, users, id, email, withPassword, true)
}

func piNewUserV(t *testing.T, users *memrepo.Users, id, email string, withPassword, verified bool) *models.User {
	t.Helper()
	u := &models.User{ID: id, Name: "John Smith", Email: email, EmailVerified: verified,
		Role: models.RoleSeller, Status: models.StatusActive, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if withPassword {
		h, err := piHasher.Hash(piPassword)
		if err != nil {
			t.Fatal(err)
		}
		u.PasswordHash = h
	}
	if err := users.Create(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	return u
}

func piNew(t *testing.T) *piFixture {
	t.Helper()
	now := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	f := &piFixture{users: memrepo.NewUsers(), mail: newPiMailer(), sms: &piSMS{}, clock: &now}
	clock := func() time.Time { return *f.clock }
	f.svc = NewPersonalInfoService(f.users, piHasher, otp.NewMemory().WithClock(clock), f.mail, f.sms,
		[]byte(strings.Repeat("k", 40))).WithClock(clock)
	f.svc.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	f.u = piNewUser(t, f.users, "u1", "john@example.com", true)
	return f
}

func (f *piFixture) advance(d time.Duration) { *f.clock = f.clock.Add(d) }

var piCodeRE = regexp.MustCompile(`\b(\d{6})\b`)

func piCodeIn(t *testing.T, text string) string {
	t.Helper()
	m := piCodeRE.FindStringSubmatch(text)
	if m == nil {
		t.Fatalf("no 6-digit code in %q", text)
	}
	return m[1]
}

func piWant(t *testing.T, err error, code string) {
	t.Helper()
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code != code {
		t.Fatalf("want error code %s, got %v", code, err)
	}
}

func piWantField(t *testing.T, err error, field string) {
	t.Helper()
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code != apperr.CodeValidationFailed || ae.Fields[field] == "" {
		t.Fatalf("want validation error on %q, got %v", field, err)
	}
}

var piCtx = context.Background()

// ---- email change

func TestPersonalEmailChangeHappyPath(t *testing.T) {
	f := piNew(t)
	secs, err := f.svc.RequestEmailChange(piCtx, "u1", piPassword, "  New@Example.com ")
	if err != nil || secs != 60 {
		t.Fatalf("request: secs=%d err=%v", secs, err)
	}
	// The code goes to the NEW address only; the account is untouched so far.
	if f.mail.count() != 1 || f.mail.last().to != "new@example.com" {
		t.Fatalf("code must go to the new address, got %+v", f.mail.sent)
	}
	if u, _ := f.users.FindByID(piCtx, "u1"); u.Email != "john@example.com" {
		t.Fatalf("email changed before confirmation: %s", u.Email)
	}
	code := piCodeIn(t, f.mail.last().text)

	info, err := f.svc.ConfirmEmailChange(piCtx, "u1", "new@example.com", code)
	if err != nil {
		t.Fatal(err)
	}
	if info.Email != "new@example.com" || !info.EmailVerified {
		t.Fatalf("not changed/verified: %+v", info)
	}
	if _, err := f.users.FindByEmail(piCtx, "john@example.com"); err == nil {
		t.Fatal("old address still resolves to the account")
	}
	// The previous address is told (asynchronously), with the new one masked.
	select {
	case n := <-f.mail.ch: // the code email
		_ = n
		select {
		case n := <-f.mail.ch:
			if n.to != "john@example.com" || !strings.Contains(n.text, "n***@example.com") || strings.Contains(n.text, "new@example.com") {
				t.Fatalf("bad notice: %+v", n)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("old address was not notified")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no mail")
	}
	// The code works once.
	_, err = f.svc.ConfirmEmailChange(piCtx, "u1", "new@example.com", code)
	piWant(t, err, apperr.CodeCodeExpired)
}

func TestPersonalEmailChangeNeedsCorrectPassword(t *testing.T) {
	f := piNew(t)
	_, err := f.svc.RequestEmailChange(piCtx, "u1", "wrong-password", "new@example.com")
	piWantField(t, err, "currentPassword")
	_, err = f.svc.RequestEmailChange(piCtx, "u1", "", "new@example.com")
	piWantField(t, err, "currentPassword")
	if f.mail.count() != 0 {
		t.Fatal("no code may be sent without reauthentication")
	}
}

func TestPersonalReauthIsRateLimited(t *testing.T) {
	f := piNew(t)
	for i := 0; i < personalReauthLimit; i++ {
		_, err := f.svc.RequestEmailChange(piCtx, "u1", "wrong", "new@example.com")
		piWantField(t, err, "currentPassword")
	}
	// Even the right password is refused now, so the password cannot be guessed here.
	_, err := f.svc.RequestEmailChange(piCtx, "u1", piPassword, "new@example.com")
	piWant(t, err, apperr.CodeRateLimited)
	f.advance(personalReauthWindow + time.Second)
	if _, err := f.svc.RequestEmailChange(piCtx, "u1", piPassword, "new@example.com"); err != nil {
		t.Fatalf("after the window: %v", err)
	}
}

func TestPersonalPasswordlessAccountMustSetPassword(t *testing.T) {
	f := piNew(t)
	piNewUser(t, f.users, "g1", "google@example.com", false)
	_, err := f.svc.RequestEmailChange(piCtx, "g1", "anything", "x@example.com")
	piWant(t, err, apperr.CodePasswordRequired)
	_, err = f.svc.RequestPhoneChange(piCtx, "g1", "anything", "01712345678")
	piWant(t, err, apperr.CodePasswordRequired)
	info, _ := f.svc.Get(piCtx, "g1")
	if info.HasPassword {
		t.Fatal("HasPassword must be false")
	}
}

func TestPersonalEmailChangeValidation(t *testing.T) {
	f := piNew(t)
	piNewUser(t, f.users, "u2", "taken@example.com", true)
	_, err := f.svc.RequestEmailChange(piCtx, "u1", piPassword, "not-an-email")
	piWantField(t, err, "newEmail")
	_, err = f.svc.RequestEmailChange(piCtx, "u1", piPassword, "JOHN@example.com")
	piWantField(t, err, "newEmail") // already yours
	_, err = f.svc.RequestEmailChange(piCtx, "u1", piPassword, "taken@example.com")
	piWant(t, err, apperr.CodeEmailTaken)
	if f.mail.count() != 0 {
		t.Fatal("nothing may be sent for invalid requests")
	}
}

func TestPersonalWrongCodesLockTheChallenge(t *testing.T) {
	f := piNew(t)
	if _, err := f.svc.RequestEmailChange(piCtx, "u1", piPassword, "new@example.com"); err != nil {
		t.Fatal(err)
	}
	good := piCodeIn(t, f.mail.last().text)
	bad := "000000"
	if good == bad {
		bad = "111111"
	}
	for i := 1; i < personalMaxAttempts; i++ {
		_, err := f.svc.ConfirmEmailChange(piCtx, "u1", "new@example.com", bad)
		piWant(t, err, apperr.CodeInvalidCode)
	}
	_, err := f.svc.ConfirmEmailChange(piCtx, "u1", "new@example.com", bad)
	piWant(t, err, apperr.CodeTooManyAttempts)
	// The right code no longer works: a new one has to be requested.
	_, err = f.svc.ConfirmEmailChange(piCtx, "u1", "new@example.com", good)
	piWant(t, err, apperr.CodeCodeExpired)
	if u, _ := f.users.FindByID(piCtx, "u1"); u.Email != "john@example.com" {
		t.Fatal("email must not change")
	}
}

func TestPersonalJunkCodeCostsNoAttempt(t *testing.T) {
	f := piNew(t)
	if _, err := f.svc.RequestEmailChange(piCtx, "u1", piPassword, "new@example.com"); err != nil {
		t.Fatal(err)
	}
	good := piCodeIn(t, f.mail.last().text)
	for i := 0; i < 20; i++ {
		_, err := f.svc.ConfirmEmailChange(piCtx, "u1", "new@example.com", "12ab")
		piWant(t, err, apperr.CodeInvalidCode)
	}
	if _, err := f.svc.ConfirmEmailChange(piCtx, "u1", "new@example.com", good); err != nil {
		t.Fatalf("junk input used up the attempts: %v", err)
	}
}

func TestPersonalCodeIsBoundToTheAddress(t *testing.T) {
	f := piNew(t)
	if _, err := f.svc.RequestEmailChange(piCtx, "u1", piPassword, "new@example.com"); err != nil {
		t.Fatal(err)
	}
	code := piCodeIn(t, f.mail.last().text)
	// Someone who tries to use the code for a different address fails.
	_, err := f.svc.ConfirmEmailChange(piCtx, "u1", "attacker@example.com", code)
	piWant(t, err, apperr.CodeInvalidCode)
}

func TestPersonalCodeIsBoundToTheUser(t *testing.T) {
	f := piNew(t)
	piNewUser(t, f.users, "u2", "other@example.com", true)
	if _, err := f.svc.RequestEmailChange(piCtx, "u1", piPassword, "new@example.com"); err != nil {
		t.Fatal(err)
	}
	code := piCodeIn(t, f.mail.last().text)
	// Another signed-in user has no challenge of their own.
	_, err := f.svc.ConfirmEmailChange(piCtx, "u2", "new@example.com", code)
	piWant(t, err, apperr.CodeCodeExpired)
}

func TestPersonalCodeExpires(t *testing.T) {
	f := piNew(t)
	if _, err := f.svc.RequestEmailChange(piCtx, "u1", piPassword, "new@example.com"); err != nil {
		t.Fatal(err)
	}
	code := piCodeIn(t, f.mail.last().text)
	f.advance(personalCodeTTL + time.Second)
	_, err := f.svc.ConfirmEmailChange(piCtx, "u1", "new@example.com", code)
	piWant(t, err, apperr.CodeCodeExpired)
}

func TestPersonalResendCooldown(t *testing.T) {
	f := piNew(t)
	if _, err := f.svc.RequestEmailChange(piCtx, "u1", piPassword, "new@example.com"); err != nil {
		t.Fatal(err)
	}
	_, err := f.svc.RequestEmailChange(piCtx, "u1", piPassword, "new@example.com")
	piWant(t, err, apperr.CodeRateLimited)
	if f.mail.count() != 1 {
		t.Fatal("a second email went out during the cooldown")
	}
	f.advance(personalCooldown + time.Second)
	if _, err := f.svc.RequestEmailChange(piCtx, "u1", piPassword, "new@example.com"); err != nil {
		t.Fatal(err)
	}
	// The newest code replaces the older one.
	newest := piCodeIn(t, f.mail.last().text)
	if _, err := f.svc.ConfirmEmailChange(piCtx, "u1", "new@example.com", newest); err != nil {
		t.Fatal(err)
	}
}

func TestPersonalSendLimitPerTarget(t *testing.T) {
	f := piNew(t)
	for i := 0; i < personalSendPerTo; i++ {
		if _, err := f.svc.RequestEmailChange(piCtx, "u1", piPassword, "victim@example.com"); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
		f.advance(personalCooldown + time.Second)
	}
	_, err := f.svc.RequestEmailChange(piCtx, "u1", piPassword, "victim@example.com")
	piWant(t, err, apperr.CodeRateLimited)
	if f.mail.count() != personalSendPerTo {
		t.Fatalf("victim received %d emails", f.mail.count())
	}
}

func TestPersonalSendLimitPerUser(t *testing.T) {
	f := piNew(t)
	for i := 0; i < personalSendPerUser; i++ {
		to := "a" + string(rune('a'+i)) + "@example.com"
		if _, err := f.svc.RequestEmailChange(piCtx, "u1", piPassword, to); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
		f.advance(personalCooldown + time.Second)
	}
	_, err := f.svc.RequestEmailChange(piCtx, "u1", piPassword, "zz@example.com")
	piWant(t, err, apperr.CodeRateLimited)
}

func TestPersonalEmailTakenAtConfirmTime(t *testing.T) {
	f := piNew(t)
	if _, err := f.svc.RequestEmailChange(piCtx, "u1", piPassword, "new@example.com"); err != nil {
		t.Fatal(err)
	}
	code := piCodeIn(t, f.mail.last().text)
	piNewUser(t, f.users, "u2", "new@example.com", true) // someone registers it meanwhile
	_, err := f.svc.ConfirmEmailChange(piCtx, "u1", "new@example.com", code)
	piWant(t, err, apperr.CodeEmailTaken)
}

func TestPersonalSendFailureDropsTheCode(t *testing.T) {
	f := piNew(t)
	f.mail.fail = errors.New("smtp down")
	_, err := f.svc.RequestEmailChange(piCtx, "u1", piPassword, "new@example.com")
	piWant(t, err, apperr.CodeSendFailed)
	_, err = f.svc.ConfirmEmailChange(piCtx, "u1", "new@example.com", "123456")
	piWant(t, err, apperr.CodeCodeExpired)
}

func TestPersonalStoreOutageFailsClosed(t *testing.T) {
	f := piNew(t)
	f.svc.codes = piBrokenStore{}
	_, err := f.svc.RequestEmailChange(piCtx, "u1", piPassword, "new@example.com")
	piWant(t, err, apperr.CodeOTPStoreUnavailable)
	_, err = f.svc.ConfirmEmailChange(piCtx, "u1", "new@example.com", "123456")
	piWant(t, err, apperr.CodeOTPStoreUnavailable)
	if f.mail.count() != 0 {
		t.Fatal("must not send anything when the store is down")
	}
}

func TestPersonalSuspendedAccountRefused(t *testing.T) {
	f := piNew(t)
	if err := f.users.UpdateStatus(piCtx, "u1", models.StatusSuspended); err != nil {
		t.Fatal(err)
	}
	_, err := f.svc.RequestEmailChange(piCtx, "u1", piPassword, "new@example.com")
	piWant(t, err, apperr.CodeAccountSuspended)
}

// ---- missing dependencies are named, not hidden

func TestPersonalMissingDependenciesAreReported(t *testing.T) {
	key := []byte(strings.Repeat("k", 40))

	// No SMTP: the real mailer drops everything, so we must refuse.
	noMail := NewPersonalInfoService(memrepo.NewUsers(), piHasher, otp.NewMemory(), mailer.Noop{}, &piSMS{}, key)
	piNewUser(t, noMail.users.(*memrepo.Users), "u1", "john@example.com", true)
	_, err := noMail.RequestEmailChange(piCtx, "u1", piPassword, "new@example.com")
	piWant(t, err, apperr.CodeEmailNotConfigured)
	_, err = noMail.RequestEmailVerify(piCtx, "u1")
	piWant(t, err, apperr.CodeEmailNotConfigured)
	if a := noMail.Availability(); a.Email || !a.Phone {
		t.Fatalf("availability = %+v", a)
	}

	// No SMS provider.
	noSMS := NewPersonalInfoService(memrepo.NewUsers(), piHasher, otp.NewMemory(), newPiMailer(), sms.NotConfigured{}, key)
	piNewUser(t, noSMS.users.(*memrepo.Users), "u1", "john@example.com", true)
	_, err = noSMS.RequestPhoneChange(piCtx, "u1", piPassword, "01712345678")
	piWant(t, err, apperr.CodeSMSNotConfigured)
	if a := noSMS.Availability(); !a.Email || a.Phone {
		t.Fatalf("availability = %+v", a)
	}

	// No Redis.
	noRedis := NewPersonalInfoService(memrepo.NewUsers(), piHasher, nil, newPiMailer(), &piSMS{}, key)
	piNewUser(t, noRedis.users.(*memrepo.Users), "u1", "john@example.com", true)
	_, err = noRedis.RequestEmailChange(piCtx, "u1", piPassword, "new@example.com")
	piWant(t, err, apperr.CodeOTPStoreNotConfigured)
	_, err = noRedis.RequestPhoneChange(piCtx, "u1", piPassword, "01712345678")
	piWant(t, err, apperr.CodeOTPStoreNotConfigured)
	_, err = noRedis.ConfirmEmailChange(piCtx, "u1", "new@example.com", "123456")
	piWant(t, err, apperr.CodeOTPStoreNotConfigured)
	if a := noRedis.Availability(); a.Email || a.Phone {
		t.Fatalf("availability = %+v", a)
	}
	if got := noRedis.MissingDependencies(); len(got) != 1 || !strings.Contains(got[0], "Redis") {
		t.Fatalf("missing = %v", got)
	}

	all := NewPersonalInfoService(memrepo.NewUsers(), piHasher, nil, mailer.Noop{}, sms.NotConfigured{}, key)
	if got := all.MissingDependencies(); len(got) != 3 {
		t.Fatalf("missing = %v", got)
	}
	// Reading personal info never needs any of them.
	piNewUser(t, all.users.(*memrepo.Users), "u1", "john@example.com", true)
	if _, err := all.Get(piCtx, "u1"); err != nil {
		t.Fatal(err)
	}
}

func TestPersonalDevelopmentLogsEmailCodes(t *testing.T) {
	var buf strings.Builder
	users := memrepo.NewUsers()
	piNewUser(t, users, "u1", "john@example.com", true)
	svc := NewPersonalInfoService(users, piHasher, otp.NewMemory(), mailer.Noop{}, &piSMS{}, []byte(strings.Repeat("k", 40)))
	svc.LogCodesTo(slog.New(slog.NewTextHandler(&buf, nil)))
	if _, err := svc.RequestEmailChange(piCtx, "u1", piPassword, "new@example.com"); err != nil {
		t.Fatal(err)
	}
	code := piCodeIn(t, buf.String())
	if _, err := svc.ConfirmEmailChange(piCtx, "u1", "new@example.com", code); err != nil {
		t.Fatal(err)
	}
}

// ---- email verification

func TestPersonalEmailVerify(t *testing.T) {
	f := piNew(t)
	piNewUserV(t, f.users, "uv", "unverified@example.com", true, false)
	if _, err := f.svc.RequestEmailVerify(piCtx, "uv"); err != nil {
		t.Fatal(err)
	}
	if f.mail.last().to != "unverified@example.com" {
		t.Fatalf("code must go to the current address: %+v", f.mail.last())
	}
	code := piCodeIn(t, f.mail.last().text)
	// A change code can't be used here, and the verify code only works for this purpose.
	if _, err := f.svc.ConfirmEmailChange(piCtx, "uv", "unverified@example.com", code); err == nil {
		t.Fatal("verify code accepted as an email-change code")
	}
	info, err := f.svc.ConfirmEmailVerify(piCtx, "uv", code)
	if err != nil || !info.EmailVerified {
		t.Fatalf("verify: %+v %v", info, err)
	}
	_, err = f.svc.RequestEmailVerify(piCtx, "uv")
	piWant(t, err, apperr.CodeAlreadyVerified)
}

// ---- phone change

func TestPersonalPhoneChangeHappyPath(t *testing.T) {
	f := piNew(t)
	secs, err := f.svc.RequestPhoneChange(piCtx, "u1", piPassword, "+880 1712-345678")
	if err != nil || secs != 60 {
		t.Fatalf("request: %d %v", secs, err)
	}
	if f.sms.last().to != "01712345678" {
		t.Fatalf("sms went to %q", f.sms.last().to)
	}
	if u, _ := f.users.FindByID(piCtx, "u1"); u.Phone != "" {
		t.Fatal("phone stored before confirmation")
	}
	code := piCodeIn(t, f.sms.last().text)
	info, err := f.svc.ConfirmPhoneChange(piCtx, "u1", "01712345678", code)
	if err != nil {
		t.Fatal(err)
	}
	if info.Phone != "01712345678" || !info.PhoneVerified {
		t.Fatalf("%+v", info)
	}
	_, err = f.svc.RequestPhoneChange(piCtx, "u1", piPassword, "01712345678")
	piWantField(t, err, "phone") // already your verified number
}

func TestPersonalPhoneChangeRules(t *testing.T) {
	f := piNew(t)
	_, err := f.svc.RequestPhoneChange(piCtx, "u1", piPassword, "12345")
	piWantField(t, err, "phone")
	_, err = f.svc.RequestPhoneChange(piCtx, "u1", "nope", "01712345678")
	piWantField(t, err, "currentPassword")
	if f.sms.count() != 0 {
		t.Fatal("SMS sent without a valid request")
	}
	// Wrong code and wrong number are both refused.
	if _, err := f.svc.RequestPhoneChange(piCtx, "u1", piPassword, "01712345678"); err != nil {
		t.Fatal(err)
	}
	code := piCodeIn(t, f.sms.last().text)
	_, err = f.svc.ConfirmPhoneChange(piCtx, "u1", "01812345678", code)
	piWant(t, err, apperr.CodeInvalidCode)
}

func TestPersonalPhoneTakenIsOnlyRevealedAfterProvingPossession(t *testing.T) {
	f := piNew(t)
	other := piNewUser(t, f.users, "u2", "other@example.com", true)
	if err := f.users.ChangePhone(piCtx, other.ID, "01712345678", true); err != nil {
		t.Fatal(err)
	}
	// Requesting does not tell whether the number is registered elsewhere.
	if _, err := f.svc.RequestPhoneChange(piCtx, "u1", piPassword, "01712345678"); err != nil {
		t.Fatalf("request must not leak ownership: %v", err)
	}
	code := piCodeIn(t, f.sms.last().text)
	_, err := f.svc.ConfirmPhoneChange(piCtx, "u1", "01712345678", code)
	piWant(t, err, apperr.CodePhoneTaken)
	if u, _ := f.users.FindByID(piCtx, "u1"); u.Phone != "" {
		t.Fatal("phone stored despite conflict")
	}
}

func TestPersonalPhoneSendFailureDropsTheCode(t *testing.T) {
	f := piNew(t)
	f.sms.fail = errors.New("gateway 500")
	_, err := f.svc.RequestPhoneChange(piCtx, "u1", piPassword, "01712345678")
	piWant(t, err, apperr.CodeSendFailed)
	f.sms.fail = sms.ErrNotConfigured
	f.advance(personalCooldown + time.Second)
	_, err = f.svc.RequestPhoneChange(piCtx, "u1", piPassword, "01712345678")
	piWant(t, err, apperr.CodeSMSNotConfigured)
}

func TestPersonalPhoneIsSeparateFromStorefrontContact(t *testing.T) {
	// Changing the personal phone only touches user.Phone / PhoneVerified.
	f := piNew(t)
	if _, err := f.svc.RequestPhoneChange(piCtx, "u1", piPassword, "01712345678"); err != nil {
		t.Fatal(err)
	}
	code := piCodeIn(t, f.sms.last().text)
	if _, err := f.svc.ConfirmPhoneChange(piCtx, "u1", "01712345678", code); err != nil {
		t.Fatal(err)
	}
	u, _ := f.users.FindByID(piCtx, "u1")
	if u.Name != "John Smith" || u.Email != "john@example.com" || u.AvatarURL != "" {
		t.Fatalf("other fields changed: %+v", u)
	}
}

// Runs the whole email-change flow against a real Redis. Skipped unless
// REDIS_TEST_URL is set (see RUN-TESTS.txt).
func TestPersonalEmailChangeWithRealRedis(t *testing.T) {
	url := os.Getenv("REDIS_TEST_URL")
	if url == "" {
		t.Skip("REDIS_TEST_URL not set")
	}
	c, err := redisx.Open(redisx.Config{URL: url})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	users, m := memrepo.NewUsers(), newPiMailer()
	piNewUser(t, users, "u1", "john@example.com", true)
	prefix := "test:pi:" + time.Now().Format("150405.000000000") + ":"
	svc := NewPersonalInfoService(users, piHasher, otp.NewRedis(c, prefix, time.Second), m, &piSMS{}, []byte(strings.Repeat("k", 40)))

	if _, err := svc.RequestEmailChange(piCtx, "u1", piPassword, "new@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RequestEmailChange(piCtx, "u1", piPassword, "new@example.com"); err == nil {
		t.Fatal("cooldown not enforced")
	} else {
		piWant(t, err, apperr.CodeRateLimited)
	}
	code := piCodeIn(t, m.last().text)
	if _, err := svc.ConfirmEmailChange(piCtx, "u1", "new@example.com", code); err != nil {
		t.Fatal(err)
	}
	if u, _ := users.FindByID(piCtx, "u1"); u.Email != "new@example.com" || !u.EmailVerified {
		t.Fatalf("%+v", u)
	}
}
