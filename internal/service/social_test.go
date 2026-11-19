package service

import (
	"context"
	"testing"
	"time"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/password"
	"shopbd/backend/internal/validation"
	"shopbd/backend/internal/repository/memrepo"
)

func newSocialSvc(t *testing.T) (*AuthService, *memrepo.Users) {
	t.Helper()
	users := memrepo.NewUsers()
	svc, err := NewAuthService(users, memrepo.NewTokens(), password.New(password.DefaultParams), AuthConfig{
		JWTSecret:  []byte("0123456789abcdef0123456789abcdef"),
		AccessTTL:  15 * time.Minute,
		RefreshTTL: 24 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	return svc, users
}

func googleProfile(sub, email string, verified bool) SocialProfile {
	return SocialProfile{Provider: models.ProviderGoogle, Subject: sub, Email: email, EmailVerified: verified, Name: "Rahim Uddin"}
}

func TestSocialLoginCreatesBuyerThenReusesIt(t *testing.T) {
	ctx := context.Background()
	svc, _ := newSocialSvc(t)

	u, pair, err := svc.LoginWithSocial(ctx, googleProfile("g-1", "Rahim@Example.com", true))
	if err != nil || pair == nil {
		t.Fatalf("first login: %v", err)
	}
	if u.Role != models.RoleBuyer || u.Email != "rahim@example.com" || !u.EmailVerified || u.PasswordHash != "" {
		t.Fatalf("unexpected new user: %+v", u)
	}

	again, _, err := svc.LoginWithSocial(ctx, googleProfile("g-1", "rahim@example.com", true))
	if err != nil || again.ID != u.ID {
		t.Fatalf("second login should reuse the account: %v", err)
	}
}

func TestSocialLoginLinksExistingVerifiedAccount(t *testing.T) {
	ctx := context.Background()
	svc, users := newSocialSvc(t)
	_ = users.Create(ctx, &models.User{ID: "u1", Email: "a@example.com", EmailVerified: true,
		PasswordHash: "keep-me", Role: models.RoleSeller, Status: models.StatusActive})

	u, _, err := svc.LoginWithSocial(ctx, googleProfile("g-2", "a@example.com", true))
	if err != nil || u.ID != "u1" || u.Role != models.RoleSeller {
		t.Fatalf("should link to the seller account: u=%+v err=%v", u, err)
	}
	got, _ := users.FindByProvider(ctx, models.ProviderGoogle, "g-2")
	if got == nil || got.ID != "u1" || got.PasswordHash != "keep-me" {
		t.Fatalf("link not saved, or a verified account lost its password: %+v", got)
	}
}

func TestSocialLoginDropsPasswordOfUnverifiedAccount(t *testing.T) {
	ctx := context.Background()
	svc, users := newSocialSvc(t)
	_ = users.Create(ctx, &models.User{ID: "u1", Email: "a@example.com", EmailVerified: false,
		PasswordHash: "attacker-password", Role: models.RoleBuyer, Status: models.StatusActive})

	if _, _, err := svc.LoginWithSocial(ctx, googleProfile("g-3", "a@example.com", true)); err != nil {
		t.Fatal(err)
	}
	got, _ := users.FindByID(ctx, "u1")
	if got.PasswordHash != "" || !got.EmailVerified {
		t.Fatalf("old password must be removed: %+v", got)
	}
}

func TestSocialLoginRefusals(t *testing.T) {
	ctx := context.Background()
	svc, users := newSocialSvc(t)

	if _, _, err := svc.LoginWithSocial(ctx, googleProfile("g-4", "x@example.com", false)); errCode(t, err) != CodeSocialEmailUnverified {
		t.Fatalf("unverified email must be refused: %v", err)
	}

	_ = users.Create(ctx, &models.User{ID: "adm", Email: "admin@example.com", EmailVerified: true,
		Role: models.RoleAdmin, Status: models.StatusActive})
	if _, _, err := svc.LoginWithSocial(ctx, googleProfile("g-5", "admin@example.com", true)); errCode(t, err) != apperr.CodeForbidden {
		t.Fatalf("admin must be refused: %v", err)
	}

	_ = users.Create(ctx, &models.User{ID: "sus", Email: "sus@example.com", EmailVerified: true,
		Role: models.RoleBuyer, Status: models.StatusSuspended})
	if _, _, err := svc.LoginWithSocial(ctx, googleProfile("g-6", "sus@example.com", true)); errCode(t, err) != apperr.CodeAccountSuspended {
		t.Fatalf("suspended account must be refused: %v", err)
	}
}

func TestPasswordLoginRefusedForSocialOnlyAccount(t *testing.T) {
	ctx := context.Background()
	svc, _ := newSocialSvc(t)
	if _, _, err := svc.LoginWithSocial(ctx, googleProfile("g-7", "p@example.com", true)); err != nil {
		t.Fatal(err)
	}
	_, _, err := svc.Login(ctx, validation.LoginInput{Email: "p@example.com", Password: "whatever123"})
	if errCode(t, err) != apperr.CodeInvalidCredentials {
		t.Fatalf("expected INVALID_CREDENTIALS, got %v", err)
	}
}
