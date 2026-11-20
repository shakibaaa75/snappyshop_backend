// Package service contains business logic. It depends on repository
// interfaces only, never on HTTP or the database driver.
package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/password"
	"shopbd/backend/internal/repository"
	"shopbd/backend/internal/validation"
)

const jwtIssuer = "shopbd-api"

type AuthConfig struct {
	JWTSecret  []byte
	AccessTTL  time.Duration
	RefreshTTL time.Duration
}

// TokenPair is what a successful authentication produces.
type TokenPair struct {
	Access           string
	AccessExpiresAt  time.Time
	Refresh          string
	RefreshExpiresAt time.Time
}

// Claims are the verified contents of an access token.
type Claims struct {
	UserID string
	Role   models.Role
}

type accessClaims struct {
	Role string `json:"role"`
	jwt.RegisteredClaims
}

type AuthService struct {
	users     repository.UserRepository
	tokens    repository.TokenRepository
	hasher    *password.Hasher
	cfg       AuthConfig
	now       func() time.Time
	dummyHash string
	verifier  EmailVerifier
}

func NewAuthService(users repository.UserRepository, tokens repository.TokenRepository, hasher *password.Hasher, cfg AuthConfig) (*AuthService, error) {
	if len(cfg.JWTSecret) < 32 {
		return nil, errors.New("auth: JWT secret must be at least 32 bytes")
	}
	// A hash of a throwaway value lets login do the same amount of work when
	// the email does not exist, so response time does not reveal accounts.
	dummy, err := hasher.Hash("shopbd-timing-equalizer")
	if err != nil {
		return nil, err
	}
	return &AuthService{users: users, tokens: tokens, hasher: hasher, cfg: cfg, now: time.Now, dummyHash: dummy}, nil
}

// SetEmailVerifier turns on the "email must be verified" rule for sign-up.
// Without it (as in most unit tests) registration does not check the email.
func (s *AuthService) SetEmailVerifier(v EmailVerifier) { s.verifier = v }

// WithClock replaces the time source (tests only).
func (s *AuthService) WithClock(now func() time.Time) *AuthService {
	s.now = now
	return s
}

func (s *AuthService) AccessTTL() time.Duration  { return s.cfg.AccessTTL }
func (s *AuthService) RefreshTTL() time.Duration { return s.cfg.RefreshTTL }

func (s *AuthService) Register(ctx context.Context, in validation.RegisterInput) (*models.User, *TokenPair, error) {
	in, errs := validation.ValidateRegister(in)
	if err := errs.Err(); err != nil {
		return nil, nil, err
	}
	// Checked before hashing the password so an unverified request is cheap.
	if s.verifier != nil {
		if err := s.verifier.CheckToken(in.VerificationToken, in.Email); err != nil {
			return nil, nil, err
		}
	}
	hash, err := s.hasher.Hash(in.Password)
	if err != nil {
		return nil, nil, err
	}
	now := s.now()
	u := &models.User{
		ID:            models.NewID(),
		Name:          in.Name,
		Email:         in.Email,
		EmailVerified: s.verifier != nil,
		PasswordHash:  hash,
		Role:          models.Role(in.Role),
		Status:        models.StatusActive,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := s.users.Create(ctx, u); err != nil {
		if errors.Is(err, repository.ErrDuplicateEmail) {
			return nil, nil, apperr.Conflict(apperr.CodeEmailTaken, "An account with this email already exists", "email")
		}
		return nil, nil, err
	}
	pair, err := s.issueTokens(ctx, u, models.NewID(), models.NewID())
	if err != nil {
		return nil, nil, err
	}
	return u, pair, nil
}

func (s *AuthService) Login(ctx context.Context, in validation.LoginInput) (*models.User, *TokenPair, error) {
	in, errs := validation.ValidateLogin(in)
	if err := errs.Err(); err != nil {
		return nil, nil, err
	}
	u, err := s.users.FindByEmail(ctx, in.Email)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			_, _ = s.hasher.Verify(in.Password, s.dummyHash)
			return nil, nil, apperr.InvalidCredentials()
		}
		return nil, nil, err
	}
	// Accounts created with Google / Apple have no password. Treat them like a
	// wrong password (same timing) instead of failing on an empty hash.
	if u.PasswordHash == "" {
		_, _ = s.hasher.Verify(in.Password, s.dummyHash)
		return nil, nil, apperr.InvalidCredentials()
	}
	ok, err := s.hasher.Verify(in.Password, u.PasswordHash)
	if err != nil {
		return nil, nil, err
	}
	if !ok {
		return nil, nil, apperr.InvalidCredentials()
	}
	if u.Status != models.StatusActive {
		return nil, nil, apperr.AccountSuspended()
	}
	pair, err := s.issueTokens(ctx, u, models.NewID(), models.NewID())
	if err != nil {
		return nil, nil, err
	}
	return u, pair, nil
}

// Refresh rotates a refresh token. Presenting a token that was already used
// is treated as theft: the whole token family is revoked.
func (s *AuthService) Refresh(ctx context.Context, raw string) (*models.User, *TokenPair, error) {
	if raw == "" {
		return nil, nil, apperr.Unauthenticated("Missing refresh token")
	}
	rt, err := s.tokens.FindByHash(ctx, hashToken(raw))
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, nil, apperr.Unauthenticated("Invalid refresh token")
		}
		return nil, nil, err
	}
	now := s.now()
	if rt.RevokedAt != nil {
		if err := s.tokens.RevokeFamily(ctx, rt.FamilyID, now); err != nil {
			return nil, nil, err
		}
		return nil, nil, apperr.TokenReused()
	}
	if !rt.ExpiresAt.After(now) {
		return nil, nil, apperr.Unauthenticated("Refresh token expired")
	}
	u, err := s.users.FindByID(ctx, rt.UserID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			_ = s.tokens.RevokeFamily(ctx, rt.FamilyID, now)
			return nil, nil, apperr.Unauthenticated("Invalid refresh token")
		}
		return nil, nil, err
	}
	if u.Status != models.StatusActive {
		_ = s.tokens.RevokeFamily(ctx, rt.FamilyID, now)
		return nil, nil, apperr.AccountSuspended()
	}
	newID := models.NewID()
	claimed, err := s.tokens.Revoke(ctx, rt.ID, newID, now)
	if err != nil {
		return nil, nil, err
	}
	if !claimed {
		// Someone else already rotated this token between our read and now.
		_ = s.tokens.RevokeFamily(ctx, rt.FamilyID, now)
		return nil, nil, apperr.TokenReused()
	}
	pair, err := s.issueTokens(ctx, u, rt.FamilyID, newID)
	if err != nil {
		return nil, nil, err
	}
	return u, pair, nil
}

// Logout revokes the whole token family of the presented refresh token. It is
// idempotent and never reveals whether the token existed.
func (s *AuthService) Logout(ctx context.Context, raw string) error {
	if raw == "" {
		return nil
	}
	rt, err := s.tokens.FindByHash(ctx, hashToken(raw))
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil
		}
		return err
	}
	return s.tokens.RevokeFamily(ctx, rt.FamilyID, s.now())
}

// Me returns the current, active user.
func (s *AuthService) Me(ctx context.Context, userID string) (*models.User, error) {
	u, err := s.users.FindByID(ctx, userID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, apperr.Unauthenticated("Invalid or expired session")
		}
		return nil, err
	}
	if u.Status != models.StatusActive {
		return nil, apperr.AccountSuspended()
	}
	return u, nil
}

// AuthenticateAccess verifies an access token and returns its claims.
// It also re-loads the user so suspended/banned/restricted accounts are
// blocked immediately (not only at the next login).
func (s *AuthService) AuthenticateAccess(token string) (*Claims, error) {
	claims := &accessClaims{}
	_, err := jwt.ParseWithClaims(token, claims,
		func(*jwt.Token) (any, error) { return s.cfg.JWTSecret, nil },
		jwt.WithValidMethods([]string{"HS256"}),
		jwt.WithIssuer(jwtIssuer),
		jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(s.now),
		jwt.WithLeeway(5*time.Second),
	)
	if err != nil || claims.Subject == "" {
		return nil, apperr.Unauthenticated("Invalid or expired session")
	}
	role := models.Role(claims.Role)
	switch role {
	case models.RoleBuyer, models.RoleSeller, models.RoleAdmin:
	default:
		return nil, apperr.Unauthenticated("Invalid or expired session")
	}
	// Live status check — suspend/ban takes effect on the next request.
	u, err := s.users.FindByID(context.Background(), claims.Subject)
	if err != nil {
		return nil, apperr.Unauthenticated("Invalid or expired session")
	}
	if u.Status != models.StatusActive {
		return nil, apperr.AccountSuspended()
	}
	// Prefer DB role (e.g. after promoting to ADMIN) over stale JWT claim.
	return &Claims{UserID: u.ID, Role: u.Role}, nil
}

func (s *AuthService) issueTokens(ctx context.Context, u *models.User, familyID, refreshID string) (*TokenPair, error) {
	now := s.now()
	accessExp := now.Add(s.cfg.AccessTTL)
	claims := accessClaims{
		Role: string(u.Role),
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    jwtIssuer,
			Subject:   u.ID,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(accessExp),
			ID:        models.NewID(),
		},
	}
	access, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(s.cfg.JWTSecret)
	if err != nil {
		return nil, err
	}

	raw, err := randomToken(32)
	if err != nil {
		return nil, err
	}
	refreshExp := now.Add(s.cfg.RefreshTTL)
	rt := &models.RefreshToken{
		ID:        refreshID,
		UserID:    u.ID,
		TokenHash: hashToken(raw),
		FamilyID:  familyID,
		ExpiresAt: refreshExp,
		CreatedAt: now,
	}
	if err := s.tokens.Create(ctx, rt); err != nil {
		return nil, err
	}
	return &TokenPair{Access: access, AccessExpiresAt: accessExp, Refresh: raw, RefreshExpiresAt: refreshExp}, nil
}

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func hashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// CodeAlreadySeller is returned when a seller asks to become a seller.
const CodeAlreadySeller = "ALREADY_SELLER"

// BecomeSeller turns the signed-in BUYER into a SELLER on the same account.
// Only the role changes: the id, email, password, linked Google account,
// name and every order or other record stay as they are, so both sign-in
// methods keep reaching the same account. setup creates the shop (name and
// address) and must succeed for the upgrade to stay; if it fails, the role is
// put back to BUYER and the account is exactly as before.
func (s *AuthService) BecomeSeller(ctx context.Context, userID string, setup func(ctx context.Context) error) (*models.User, error) {
	u, err := s.users.FindByID(ctx, userID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, apperr.Unauthenticated("Invalid or expired session")
		}
		return nil, err
	}
	if u.Status != models.StatusActive {
		return nil, apperr.AccountSuspended()
	}
	switch u.Role {
	case models.RoleBuyer:
	case models.RoleSeller:
		return nil, apperr.New(http.StatusConflict, CodeAlreadySeller, "This account is already a seller")
	default:
		return nil, apperr.Forbidden("This account type cannot become a seller")
	}
	if err := s.users.UpdateRole(ctx, u.ID, models.RoleSeller); err != nil {
		return nil, err
	}
	if err := setup(ctx); err != nil {
		if rerr := s.users.UpdateRole(context.WithoutCancel(ctx), u.ID, models.RoleBuyer); rerr != nil {
			return nil, errors.Join(err, rerr)
		}
		return nil, err
	}
	u.Role = models.RoleSeller
	return u, nil
}
