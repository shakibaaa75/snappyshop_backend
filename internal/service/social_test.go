package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/password"
	"shopbd/backend/internal/repository/memrepo"
	"shopbd/backend/internal/validation"
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

// seedPasswordUser registers a verified email/password account the real way,
// so the hash is genuine and the password can be used to log in.
func seedPasswordUser(t *testing.T, svc *AuthService, email string, role models.Role) *models.User {
	t.Helper()
	hash, err := svc.hasher.Hash("correct-horse-1")
	if err != nil {
		t.Fatal(err)
	}
	u := &models.User{ID: "u-" + email, Name: "Seller One", Email: email, EmailVerified: true,
		PasswordHash: hash, Role: role, Status: models.StatusActive}
	if err := svc.users.Create(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	return u
}

func asLinkRequired(t *testing.T, err error) *LinkRequiredError {
	t.Helper()
	var lr *LinkRequiredError
	if !errors.As(err, &lr) {
		t.Fatalf("expected LinkRequiredError, got %v", err)
	}
	return lr
}

func TestSocialLoginDoesNotAutoLinkExistingAccount(t *testing.T) {
	ctx := context.Background()
	svc, users := newSocialSvc(t)
	orig := seedPasswordUser(t, svc, "a@example.com", models.RoleSeller)

	u, pair, err := svc.LoginWithSocial(ctx, googleProfile("g-2", "A@example.com", true))
	if u != nil || pair != nil {
		t.Fatalf("no session may be created before the owner proves who they are")
	}
	lr := asLinkRequired(t, err)
	if lr.Link.UserID != orig.ID || lr.Link.Subject != "g-2" {
		t.Fatalf("wrong link target: %+v", lr.Link)
	}
	if got, _ := users.FindByProvider(ctx, models.ProviderGoogle, "g-2"); got != nil {
		t.Fatalf("Google must not be linked yet")
	}
	// No duplicate account either.
	if _, err := users.FindByEmail(ctx, "a@example.com"); err != nil {
		t.Fatal(err)
	}
}

func TestLinkSocialWithPasswordKeepsEverything(t *testing.T) {
	ctx := context.Background()
	svc, users := newSocialSvc(t)
	orig := seedPasswordUser(t, svc, "a@example.com", models.RoleSeller)
	hashBefore := orig.PasswordHash

	_, _, err := svc.LoginWithSocial(ctx, googleProfile("g-2", "a@example.com", true))
	lr := asLinkRequired(t, err)

	// Wrong password: refused, nothing linked.
	if _, _, err := svc.LinkSocial(ctx, lr.Link, "wrong-password", ""); errCode(t, err) != apperr.CodeInvalidCredentials {
		t.Fatalf("wrong password must be refused: %v", err)
	}
	// No proof at all: refused.
	if _, _, err := svc.LinkSocial(ctx, lr.Link, "", ""); errCode(t, err) != apperr.CodeInvalidCredentials {
		t.Fatalf("missing proof must be refused: %v", err)
	}
	// A session for a DIFFERENT account is not proof.
	if _, _, err := svc.LinkSocial(ctx, lr.Link, "", "someone-else"); errCode(t, err) != apperr.CodeInvalidCredentials {
		t.Fatalf("other user's session must be refused: %v", err)
	}
	if got, _ := users.FindByProvider(ctx, models.ProviderGoogle, "g-2"); got != nil {
		t.Fatalf("failed attempts must not link")
	}

	u, pair, err := svc.LinkSocial(ctx, lr.Link, "correct-horse-1", "")
	if err != nil || pair == nil || u.ID != orig.ID {
		t.Fatalf("link failed: %v", err)
	}

	after, _ := users.FindByID(ctx, orig.ID)
	if after.PasswordHash != hashBefore {
		t.Fatalf("linking must not change the password hash")
	}
	if after.Role != models.RoleSeller || after.Email != orig.Email || after.Name != orig.Name ||
		after.Status != models.StatusActive || after.GoogleID != "g-2" {
		t.Fatalf("account changed unexpectedly: %+v", after)
	}

	// Both methods now reach the same account.
	g, _, err := svc.LoginWithSocial(ctx, googleProfile("g-2", "a@example.com", true))
	if err != nil || g.ID != orig.ID || g.Role != models.RoleSeller {
		t.Fatalf("google login after link: %v", err)
	}
	p, _, err := svc.Login(ctx, validation.LoginInput{Email: "a@example.com", Password: "correct-horse-1"})
	if err != nil || p.ID != orig.ID {
		t.Fatalf("password login after link: %v", err)
	}
	// Repeated Google logins still never alter the password.
	again, _ := users.FindByID(ctx, orig.ID)
	if again.PasswordHash != hashBefore {
		t.Fatalf("google sign-in changed the password hash")
	}
}

func TestLinkSocialWithExistingSession(t *testing.T) {
	ctx := context.Background()
	svc, users := newSocialSvc(t)
	orig := seedPasswordUser(t, svc, "a@example.com", models.RoleBuyer)

	// Already signed in to the account: Google links straight away.
	p := googleProfile("g-9", "a@example.com", true)
	p.AuthenticatedUserID = orig.ID
	u, _, err := svc.LoginWithSocial(ctx, p)
	if err != nil || u.ID != orig.ID {
		t.Fatalf("signed-in user should link directly: %v", err)
	}
	if got, _ := users.FindByID(ctx, orig.ID); got.PasswordHash != orig.PasswordHash || got.GoogleID != "g-9" {
		t.Fatalf("bad state after session link: %+v", got)
	}

	// A session for another account does not count.
	svc2, users2 := newSocialSvc(t)
	victim := seedPasswordUser(t, svc2, "v@example.com", models.RoleBuyer)
	p2 := googleProfile("g-10", "v@example.com", true)
	p2.AuthenticatedUserID = "attacker"
	_, _, err = svc2.LoginWithSocial(ctx, p2)
	asLinkRequired(t, err)
	if got, _ := users2.FindByID(ctx, victim.ID); got.GoogleID != "" {
		t.Fatalf("foreign session must not link")
	}
}

func TestLinkSocialSessionLinksPasswordlessAccount(t *testing.T) {
	ctx := context.Background()
	svc, _ := newSocialSvc(t)
	// Apple-only account (no password); Google arrives with the same email.
	_ = svc.users.Create(ctx, &models.User{ID: "ap", Email: "ap@example.com", EmailVerified: true,
		AppleID: "apple-1", Role: models.RoleBuyer, Status: models.StatusActive})
	_, _, err := svc.LoginWithSocial(ctx, googleProfile("g-11", "ap@example.com", true))
	lr := asLinkRequired(t, err)
	if _, _, err := svc.LinkSocial(ctx, lr.Link, "anything-123", ""); errCode(t, err) != apperr.CodeInvalidCredentials {
		t.Fatalf("no password on file: %v", err)
	}
	if _, _, err := svc.LinkSocial(ctx, lr.Link, "", "ap"); err != nil {
		t.Fatalf("session proof should link: %v", err)
	}
}

func TestSocialLoginNeverReplacesLinkedGoogle(t *testing.T) {
	ctx := context.Background()
	svc, users := newSocialSvc(t)
	orig := seedPasswordUser(t, svc, "a@example.com", models.RoleBuyer)
	_ = users.LinkProvider(ctx, orig.ID, models.ProviderGoogle, "g-old")

	_, _, err := svc.LoginWithSocial(ctx, googleProfile("g-new", "a@example.com", true))
	if errCode(t, err) != CodeSocialAlreadyLinked {
		t.Fatalf("a second Google identity must be refused: %v", err)
	}
	if got, _ := users.FindByID(ctx, orig.ID); got.GoogleID != "g-old" {
		t.Fatalf("linked Google ID was replaced: %+v", got)
	}
}

func TestLinkSocialRejectsForgedOrStaleLink(t *testing.T) {
	ctx := context.Background()
	svc, users := newSocialSvc(t)
	orig := seedPasswordUser(t, svc, "a@example.com", models.RoleBuyer)
	_, _, err := svc.LoginWithSocial(ctx, googleProfile("g-2", "a@example.com", true))
	lr := asLinkRequired(t, err)

	// Email on the account changed since the link was issued.
	_ = users.UpdateProfile(ctx, orig.ID, orig.Name, "new@example.com")
	if _, _, err := svc.LinkSocial(ctx, lr.Link, "correct-horse-1", ""); err == nil {
		t.Fatalf("stale link must be refused")
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
