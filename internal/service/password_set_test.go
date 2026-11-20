package service

import (
	"context"
	"testing"
	"time"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/password"
	"shopbd/backend/internal/repository/memrepo"
	"shopbd/backend/internal/validation"
)

// Google-first users add a password; both ways of signing in then reach the
// same account.
func TestGoogleFirstUserCanSetPasswordAndUseBoth(t *testing.T) {
	ctx := context.Background()
	users := memrepo.NewUsers()
	hasher := password.New(password.Params{Memory: 8, Time: 1, Threads: 1, KeyLen: 32, SaltLen: 16})
	auth, err := NewAuthService(users, memrepo.NewTokens(), hasher, AuthConfig{
		JWTSecret: []byte("0123456789abcdef0123456789abcdef"), AccessTTL: time.Minute, RefreshTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	mail := &resetMail{}
	reset := NewPasswordResetService(users, memrepo.NewPasswordResets(), hasher, mail, []byte("0123456789abcdef0123456789abcdef"))
	reset.newCode = func() (string, error) { return "123456", nil }

	g, _, err := auth.LoginWithSocial(ctx, googleProfile("g-1", "kid@example.com", true))
	if err != nil {
		t.Fatal(err)
	}

	// No code yet, wrong code and a weak password are all refused.
	if err := reset.SetInitialPassword(ctx, g.ID, "123456", "new-password-1"); errCode(t, err) != apperr.CodeValidationFailed {
		t.Fatalf("no challenge should fail as invalid code: %v", err)
	}
	if err := reset.RequestSetPassword(ctx, g.ID); err != nil {
		t.Fatal(err)
	}
	if calls, to, _ := mail.snapshot(); calls != 1 || to[0] != "kid@example.com" {
		t.Fatalf("code must be emailed to the account's own address: %d %v", calls, to)
	}
	if err := reset.SetInitialPassword(ctx, g.ID, "000000", "new-password-1"); errCode(t, err) != apperr.CodeValidationFailed {
		t.Fatalf("wrong code: %v", err)
	}
	if err := reset.SetInitialPassword(ctx, g.ID, "123456", "short"); errCode(t, err) != apperr.CodeValidationFailed {
		t.Fatalf("weak password: %v", err)
	}
	// Another signed-in user cannot use this account's code.
	if err := reset.SetInitialPassword(ctx, "someone-else", "123456", "new-password-1"); err == nil {
		t.Fatal("other user must not use the code")
	}

	if err := reset.SetInitialPassword(ctx, g.ID, "123456", "new-password-1"); err != nil {
		t.Fatalf("set password: %v", err)
	}
	// Code is single use.
	if err := reset.SetInitialPassword(ctx, g.ID, "123456", "other-password-2"); err == nil {
		t.Fatal("code must be single-use")
	}

	// Both methods reach the same account; Google ID is still linked.
	p, _, err := auth.Login(ctx, validation.LoginInput{Email: "kid@example.com", Password: "new-password-1"})
	if err != nil || p.ID != g.ID {
		t.Fatalf("password login: %v", err)
	}
	g2, _, err := auth.LoginWithSocial(ctx, googleProfile("g-1", "kid@example.com", true))
	if err != nil || g2.ID != g.ID || g2.PasswordHash == "" || g2.Role != models.RoleBuyer {
		t.Fatalf("google login after setting password: %v %+v", err, g2)
	}

	// An account that already has a password can't be given one this way, and
	// no new code is sent for it.
	if err := reset.RequestSetPassword(ctx, g.ID); errCode(t, err) != CodePasswordAlreadySet {
		t.Fatalf("expected PASSWORD_ALREADY_SET, got %v", err)
	}
	if err := reset.SetInitialPassword(ctx, g.ID, "123456", "hijack-password-3"); errCode(t, err) != CodePasswordAlreadySet {
		t.Fatalf("must never overwrite an existing password: %v", err)
	}
	if _, _, err := auth.Login(ctx, validation.LoginInput{Email: "kid@example.com", Password: "new-password-1"}); err != nil {
		t.Fatalf("original password must still work: %v", err)
	}
}
