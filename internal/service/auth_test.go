package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/password"
	"shopbd/backend/internal/repository/memrepo"
	"shopbd/backend/internal/validation"
)

type fixture struct {
	svc    *AuthService
	users  *memrepo.Users
	tokens *memrepo.Tokens
	clock  *time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	users, tokens := memrepo.NewUsers(), memrepo.NewTokens()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	svc, err := NewAuthService(users, tokens,
		password.New(password.Params{Memory: 8, Time: 1, Threads: 1, KeyLen: 32, SaltLen: 16}),
		AuthConfig{JWTSecret: []byte(strings.Repeat("k", 40)), AccessTTL: 15 * time.Minute, RefreshTTL: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{svc: svc, users: users, tokens: tokens, clock: &now}
	svc.WithClock(func() time.Time { return *f.clock })
	return f
}

func (f *fixture) advance(d time.Duration) { *f.clock = f.clock.Add(d) }

func regInput(email string) validation.RegisterInput {
	return validation.RegisterInput{Name: "Sakib", Email: email, Password: "password1", ConfirmPassword: "password1", Role: "SELLER"}
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code != code {
		t.Fatalf("want error code %s, got %v", code, err)
	}
}

func TestRegisterCreatesUserAndTokens(t *testing.T) {
	f := newFixture(t)
	u, pair, err := f.svc.Register(context.Background(), regInput("  Sakib@Example.com "))
	if err != nil {
		t.Fatal(err)
	}
	if u.Email != "sakib@example.com" || u.Role != models.RoleSeller || u.Status != models.StatusActive {
		t.Errorf("unexpected user: %+v", u)
	}
	if u.PasswordHash == "" || strings.Contains(u.PasswordHash, "password1") {
		t.Error("password must be stored hashed")
	}
	claims, err := f.svc.AuthenticateAccess(pair.Access)
	if err != nil || claims.UserID != u.ID || claims.Role != models.RoleSeller {
		t.Fatalf("access token invalid: %v %+v", err, claims)
	}
	if pair.Refresh == "" {
		t.Error("refresh token missing")
	}
}

func TestRegisterDuplicateEmailIsCaseInsensitive(t *testing.T) {
	f := newFixture(t)
	if _, _, err := f.svc.Register(context.Background(), regInput("a@example.com")); err != nil {
		t.Fatal(err)
	}
	_, _, err := f.svc.Register(context.Background(), regInput("A@EXAMPLE.COM"))
	wantCode(t, err, apperr.CodeEmailTaken)
}

func TestRegisterValidationError(t *testing.T) {
	f := newFixture(t)
	in := regInput("a@example.com")
	in.ConfirmPassword = "nope"
	_, _, err := f.svc.Register(context.Background(), in)
	wantCode(t, err, apperr.CodeValidationFailed)
}

func TestLogin(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	u, _, _ := f.svc.Register(ctx, regInput("a@example.com"))

	got, pair, err := f.svc.Login(ctx, validation.LoginInput{Email: "A@example.com", Password: "password1"})
	if err != nil || got.ID != u.ID || pair.Access == "" {
		t.Fatalf("login failed: %v", err)
	}
	_, _, err = f.svc.Login(ctx, validation.LoginInput{Email: "a@example.com", Password: "wrong-password"})
	wantCode(t, err, apperr.CodeInvalidCredentials)
	_, _, err = f.svc.Login(ctx, validation.LoginInput{Email: "nobody@example.com", Password: "password1"})
	wantCode(t, err, apperr.CodeInvalidCredentials)

	f.users.SetStatus(u.ID, models.StatusSuspended)
	_, _, err = f.svc.Login(ctx, validation.LoginInput{Email: "a@example.com", Password: "password1"})
	wantCode(t, err, apperr.CodeAccountSuspended)
	// A wrong password for a suspended account must not reveal suspension.
	_, _, err = f.svc.Login(ctx, validation.LoginInput{Email: "a@example.com", Password: "wrong-password"})
	wantCode(t, err, apperr.CodeInvalidCredentials)
}

func TestRefreshRotatesToken(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	_, pair, _ := f.svc.Register(ctx, regInput("a@example.com"))

	f.advance(time.Minute)
	_, next, err := f.svc.Refresh(ctx, pair.Refresh)
	if err != nil {
		t.Fatal(err)
	}
	if next.Refresh == pair.Refresh || next.Access == pair.Access {
		t.Error("tokens must be rotated")
	}
	if _, _, err := f.svc.Refresh(ctx, next.Refresh); err != nil {
		t.Errorf("new refresh token should work: %v", err)
	}
}

func TestRefreshReuseRevokesWholeFamily(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	_, pair, _ := f.svc.Register(ctx, regInput("a@example.com"))

	_, next, err := f.svc.Refresh(ctx, pair.Refresh)
	if err != nil {
		t.Fatal(err)
	}
	// Replaying the already-used token signals theft.
	_, _, err = f.svc.Refresh(ctx, pair.Refresh)
	wantCode(t, err, apperr.CodeTokenReused)
	// The legitimate newer token is now revoked too.
	_, _, err = f.svc.Refresh(ctx, next.Refresh)
	wantCode(t, err, apperr.CodeTokenReused)
}

func TestRefreshRejectsUnknownEmptyAndExpired(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	_, pair, _ := f.svc.Register(ctx, regInput("a@example.com"))

	_, _, err := f.svc.Refresh(ctx, "")
	wantCode(t, err, apperr.CodeUnauthenticated)
	_, _, err = f.svc.Refresh(ctx, "not-a-real-token")
	wantCode(t, err, apperr.CodeUnauthenticated)

	f.advance(25 * time.Hour)
	_, _, err = f.svc.Refresh(ctx, pair.Refresh)
	wantCode(t, err, apperr.CodeUnauthenticated)
}

func TestRefreshBlockedForSuspendedUser(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	u, pair, _ := f.svc.Register(ctx, regInput("a@example.com"))
	f.users.SetStatus(u.ID, models.StatusSuspended)
	_, _, err := f.svc.Refresh(ctx, pair.Refresh)
	wantCode(t, err, apperr.CodeAccountSuspended)
}

func TestLogoutRevokesFamilyAndIsIdempotent(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	_, pair, _ := f.svc.Register(ctx, regInput("a@example.com"))

	if err := f.svc.Logout(ctx, pair.Refresh); err != nil {
		t.Fatal(err)
	}
	_, _, err := f.svc.Refresh(ctx, pair.Refresh)
	wantCode(t, err, apperr.CodeTokenReused)
	if err := f.svc.Logout(ctx, pair.Refresh); err != nil {
		t.Errorf("second logout should succeed: %v", err)
	}
	if err := f.svc.Logout(ctx, ""); err != nil {
		t.Errorf("logout without a token should succeed: %v", err)
	}
	if err := f.svc.Logout(ctx, "unknown"); err != nil {
		t.Errorf("logout with unknown token should succeed: %v", err)
	}
}

func TestRefreshTokensStoredHashedOnly(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	_, pair, _ := f.svc.Register(ctx, regInput("a@example.com"))
	if _, err := f.tokens.FindByHash(ctx, pair.Refresh); err == nil {
		t.Error("the raw refresh token must not be usable as a stored hash")
	}
	if _, err := f.tokens.FindByHash(ctx, hashToken(pair.Refresh)); err != nil {
		t.Errorf("hash of refresh token should be stored: %v", err)
	}
}

func TestAccessTokenExpiry(t *testing.T) {
	f := newFixture(t)
	_, pair, _ := f.svc.Register(context.Background(), regInput("a@example.com"))
	f.advance(14 * time.Minute)
	if _, err := f.svc.AuthenticateAccess(pair.Access); err != nil {
		t.Errorf("token should still be valid: %v", err)
	}
	f.advance(2 * time.Minute)
	_, err := f.svc.AuthenticateAccess(pair.Access)
	wantCode(t, err, apperr.CodeUnauthenticated)
}

func TestAccessTokenRejectsForgeries(t *testing.T) {
	f := newFixture(t)
	u, pair, _ := f.svc.Register(context.Background(), regInput("a@example.com"))

	// Tampered signature.
	_, err := f.svc.AuthenticateAccess(pair.Access[:len(pair.Access)-2] + "xx")
	wantCode(t, err, apperr.CodeUnauthenticated)

	mk := func(method jwt.SigningMethod, key any, claims jwt.Claims) string {
		s, err := jwt.NewWithClaims(method, claims).SignedString(key)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	valid := accessClaims{Role: "ADMIN", RegisteredClaims: jwt.RegisteredClaims{
		Issuer: jwtIssuer, Subject: u.ID, ExpiresAt: jwt.NewNumericDate(f.clock.Add(time.Hour)),
	}}
	// alg=none must never be accepted.
	_, err = f.svc.AuthenticateAccess(mk(jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType, valid))
	wantCode(t, err, apperr.CodeUnauthenticated)
	// Signed with a different secret.
	_, err = f.svc.AuthenticateAccess(mk(jwt.SigningMethodHS256, []byte(strings.Repeat("z", 40)), valid))
	wantCode(t, err, apperr.CodeUnauthenticated)
	// Wrong issuer.
	wrongIss := valid
	wrongIss.Issuer = "someone-else"
	_, err = f.svc.AuthenticateAccess(mk(jwt.SigningMethodHS256, []byte(strings.Repeat("k", 40)), wrongIss))
	wantCode(t, err, apperr.CodeUnauthenticated)
	// Missing expiration.
	noExp := valid
	noExp.ExpiresAt = nil
	_, err = f.svc.AuthenticateAccess(mk(jwt.SigningMethodHS256, []byte(strings.Repeat("k", 40)), noExp))
	wantCode(t, err, apperr.CodeUnauthenticated)
	// Unknown role.
	badRole := valid
	badRole.Role = "SUPERUSER"
	_, err = f.svc.AuthenticateAccess(mk(jwt.SigningMethodHS256, []byte(strings.Repeat("k", 40)), badRole))
	wantCode(t, err, apperr.CodeUnauthenticated)
	// Garbage.
	_, err = f.svc.AuthenticateAccess("not.a.jwt")
	wantCode(t, err, apperr.CodeUnauthenticated)
}

func TestMe(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	u, _, _ := f.svc.Register(ctx, regInput("a@example.com"))
	got, err := f.svc.Me(ctx, u.ID)
	if err != nil || got.ID != u.ID {
		t.Fatalf("me failed: %v", err)
	}
	_, err = f.svc.Me(ctx, "missing")
	wantCode(t, err, apperr.CodeUnauthenticated)
	f.users.SetStatus(u.ID, models.StatusSuspended)
	_, err = f.svc.Me(ctx, u.ID)
	wantCode(t, err, apperr.CodeAccountSuspended)
}

func TestNewAuthServiceRejectsShortSecret(t *testing.T) {
	_, err := NewAuthService(memrepo.NewUsers(), memrepo.NewTokens(),
		password.New(password.Params{Memory: 8, Time: 1, Threads: 1, KeyLen: 32, SaltLen: 16}),
		AuthConfig{JWTSecret: []byte("short")})
	if err == nil {
		t.Fatal("expected error for short secret")
	}
}
