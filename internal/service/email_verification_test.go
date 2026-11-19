package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/repository/memrepo"
)

type captureMailer struct {
	n    int
	text string
}

func (c *captureMailer) Send(_ context.Context, _, _, _, text string) error {
	c.n++
	c.text = text
	return nil
}

// code pulls the 6 digits out of "Your ShopBD verification code is 123456."
func (c *captureMailer) code() string {
	i := strings.Index(c.text, " is ")
	return c.text[i+4 : i+10]
}

func newVerifySvc() (*EmailVerificationService, *captureMailer, *memrepo.Users) {
	users := memrepo.NewUsers()
	m := &captureMailer{}
	svc := NewEmailVerificationService(users, memrepo.NewEmailVerifications(), m,
		[]byte("0123456789abcdef0123456789abcdef"))
	return svc, m, users
}

func errCode(t *testing.T, err error) string {
	t.Helper()
	var ae *apperr.Error
	if !errors.As(err, &ae) {
		t.Fatalf("expected *apperr.Error, got %v", err)
	}
	return ae.Code
}

func TestEmailVerificationHappyPath(t *testing.T) {
	ctx := context.Background()
	svc, m, _ := newVerifySvc()

	resend, err := svc.SendCode(ctx, "  A@Example.com ")
	if err != nil || resend != 60 || m.n != 1 {
		t.Fatalf("SendCode: resend=%d n=%d err=%v", resend, m.n, err)
	}

	token, err := svc.VerifyCode(ctx, "a@example.com", m.code())
	if err != nil || token == "" {
		t.Fatalf("VerifyCode: token=%q err=%v", token, err)
	}
	if err := svc.CheckToken(token, "A@EXAMPLE.COM"); err != nil {
		t.Fatalf("token should be valid for the same email: %v", err)
	}
	if err := svc.CheckToken(token, "b@example.com"); errCode(t, err) != apperr.CodeVerificationRequired {
		t.Fatalf("token must not work for another email: %v", err)
	}
	if err := svc.CheckToken("", "a@example.com"); err == nil {
		t.Fatal("empty token must be rejected")
	}

	// A code works once.
	if _, err := svc.VerifyCode(ctx, "a@example.com", m.code()); errCode(t, err) != apperr.CodeCodeExpired {
		t.Fatalf("reusing a code should fail: %v", err)
	}
}

func TestEmailVerificationRejectsTakenEmail(t *testing.T) {
	ctx := context.Background()
	svc, m, users := newVerifySvc()
	if err := users.Create(ctx, &models.User{ID: "u1", Email: "a@example.com"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SendCode(ctx, "a@example.com"); errCode(t, err) != apperr.CodeEmailTaken {
		t.Fatalf("expected EMAIL_TAKEN, got %v", err)
	}
	if m.n != 0 {
		t.Fatal("no email should be sent for a taken address")
	}
}

func TestEmailVerificationLocksAfterWrongGuesses(t *testing.T) {
	ctx := context.Background()
	svc, m, _ := newVerifySvc()
	if _, err := svc.SendCode(ctx, "a@example.com"); err != nil {
		t.Fatal(err)
	}
	wrong := "000000"
	if m.code() == wrong {
		wrong = "111111"
	}

	for i := 1; i < emailMaxAttempts; i++ {
		if _, err := svc.VerifyCode(ctx, "a@example.com", wrong); errCode(t, err) != apperr.CodeInvalidCode {
			t.Fatalf("attempt %d: expected INVALID_CODE, got %v", i, err)
		}
	}
	if _, err := svc.VerifyCode(ctx, "a@example.com", wrong); errCode(t, err) != apperr.CodeTooManyAttempts {
		t.Fatalf("expected TOO_MANY_ATTEMPTS on the last attempt, got %v", err)
	}
	// The code is gone, so even the right one no longer works.
	if _, err := svc.VerifyCode(ctx, "a@example.com", m.code()); errCode(t, err) != apperr.CodeCodeExpired {
		t.Fatalf("expected CODE_EXPIRED after lockout, got %v", err)
	}
}

func TestEmailVerificationCooldownDoesNotResend(t *testing.T) {
	ctx := context.Background()
	svc, m, _ := newVerifySvc()
	if _, err := svc.SendCode(ctx, "a@example.com"); err != nil {
		t.Fatal(err)
	}
	wait, err := svc.SendCode(ctx, "a@example.com")
	if err != nil || wait <= 0 || m.n != 1 {
		t.Fatalf("second call: wait=%d n=%d err=%v", wait, m.n, err)
	}
}

func TestEmailVerificationTokenExpires(t *testing.T) {
	ctx := context.Background()
	svc, m, _ := newVerifySvc()
	if _, err := svc.SendCode(ctx, "a@example.com"); err != nil {
		t.Fatal(err)
	}
	token, err := svc.VerifyCode(ctx, "a@example.com", m.code())
	if err != nil {
		t.Fatal(err)
	}
	svc.now = func() time.Time { return time.Now().Add(emailTokenTTL + time.Minute) }
	if err := svc.CheckToken(token, "a@example.com"); err == nil {
		t.Fatal("expired token must be rejected")
	}
}
