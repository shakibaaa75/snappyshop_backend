package service

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/mailer"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/repository"
	"shopbd/backend/internal/validation"
)

const (
	emailCodeTTL        = 10 * time.Minute // the UI tells people "10 minutes"
	emailResendCooldown = 60 * time.Second
	emailMaxAttempts    = 5
	emailTokenTTL       = 30 * time.Minute
	emailTokenAudience  = "shopbd-email-verification"
	emailTokenPurpose   = "register"
)

// EmailVerifier checks that an email address was verified. AuthService uses it
// to refuse sign-ups that skipped verification.
type EmailVerifier interface {
	CheckToken(token, email string) error
}

// EmailVerificationService proves that someone controls an email address
// before an account is created for it:
//
//	SendCode   -> emails a 6-digit code
//	VerifyCode -> trades the code for a short-lived signed token
//	CheckToken -> used by registration to validate that token
type EmailVerificationService struct {
	users  repository.UserRepository
	codes  repository.EmailVerificationRepository
	mail   mailer.Mailer
	key    []byte
	now    func() time.Time
	devLog *slog.Logger
}

func NewEmailVerificationService(
	users repository.UserRepository,
	codes repository.EmailVerificationRepository,
	m mailer.Mailer,
	jwtSecret []byte,
) *EmailVerificationService {
	// A key derived from the JWT secret keeps these tokens from ever being
	// valid as access tokens, and vice versa.
	mac := hmac.New(sha256.New, jwtSecret)
	mac.Write([]byte("shopbd/email-verification/v1"))
	return &EmailVerificationService{
		users: users,
		codes: codes,
		mail:  m,
		key:   mac.Sum(nil),
		now:   time.Now,
	}
}

// LogCodesTo prints every code to the log. Development only: use it when SMTP
// is not configured so you can still complete sign-up locally.
func (s *EmailVerificationService) LogCodesTo(l *slog.Logger) { s.devLog = l }

// SendCode emails a fresh code and returns the number of seconds before
// another one may be requested. If a code was sent less than a minute ago it
// does not send again; it just reports the time left, so a page refresh does
// not cost the user their valid code.
func (s *EmailVerificationService) SendCode(ctx context.Context, rawEmail string) (int, error) {
	email := validation.NormalizeEmail(rawEmail)
	if !validation.ValidEmail(email) {
		return 0, apperr.Validation(map[string]string{"email": "Please enter a valid email address"})
	}

	if _, err := s.users.FindByEmail(ctx, email); err == nil {
		return 0, apperr.Conflict(apperr.CodeEmailTaken, "An account with this email already exists", "email")
	} else if !errors.Is(err, repository.ErrNotFound) {
		return 0, err
	}

	now := s.now().UTC()
	if prev, err := s.codes.FindByEmail(ctx, email); err == nil {
		if wait := prev.SentAt.Add(emailResendCooldown).Sub(now); wait > 0 {
			return int(math.Ceil(wait.Seconds())), nil
		}
	} else if !errors.Is(err, repository.ErrVerificationNotFound) {
		return 0, err
	}

	code, err := generateCode()
	if err != nil {
		return 0, err
	}
	rec := &models.EmailVerification{
		ID:        models.NewID(),
		Email:     email,
		CodeHash:  s.hashCode(email, code),
		SentAt:    now,
		ExpiresAt: now.Add(emailCodeTTL),
		CreatedAt: now,
	}
	if err := s.codes.Replace(ctx, rec); err != nil {
		return 0, err
	}

	if s.devLog != nil {
		s.devLog.Info("email verification code (development only)", "email", email, "code", code)
	}

	if err := s.sendEmail(ctx, email, code); err != nil {
		_ = s.codes.DeleteByEmail(ctx, email)
		return 0, fmt.Errorf("send verification email: %w", err)
	}
	return int(emailResendCooldown.Seconds()), nil
}

// VerifyCode checks the code and, if it is right, returns a token that
// registration accepts for this exact email. A code works once, and only
// emailMaxAttempts guesses are allowed per code.
func (s *EmailVerificationService) VerifyCode(ctx context.Context, rawEmail, rawCode string) (string, error) {
	email := validation.NormalizeEmail(rawEmail)
	code := strings.TrimSpace(rawCode)
	if !validation.ValidEmail(email) {
		return "", apperr.Validation(map[string]string{"email": "Please enter a valid email address"})
	}
	if !isSixDigits(code) {
		return "", apperr.InvalidCode()
	}

	rec, err := s.codes.FindByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, repository.ErrVerificationNotFound) {
			return "", apperr.CodeExpired()
		}
		return "", err
	}
	if !rec.ExpiresAt.After(s.now().UTC()) {
		_ = s.codes.DeleteByEmail(ctx, email)
		return "", apperr.CodeExpired()
	}

	// Count the attempt first and atomically, so parallel guesses cannot
	// get around the limit.
	n, err := s.codes.IncrementAttempts(ctx, email)
	if err != nil {
		if errors.Is(err, repository.ErrVerificationNotFound) {
			return "", apperr.CodeExpired()
		}
		return "", err
	}
	if n > emailMaxAttempts {
		_ = s.codes.DeleteByEmail(ctx, email)
		return "", apperr.TooManyAttempts()
	}

	want := s.hashCode(email, code)
	if subtle.ConstantTimeCompare([]byte(rec.CodeHash), []byte(want)) != 1 {
		if n >= emailMaxAttempts {
			_ = s.codes.DeleteByEmail(ctx, email)
			return "", apperr.TooManyAttempts()
		}
		return "", apperr.InvalidCode()
	}

	_ = s.codes.DeleteByEmail(ctx, email)
	return s.issueToken(email)
}

type emailClaims struct {
	Purpose string `json:"purpose"`
	jwt.RegisteredClaims
}

func (s *EmailVerificationService) issueToken(email string) (string, error) {
	now := s.now()
	claims := emailClaims{
		Purpose: emailTokenPurpose,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    jwtIssuer,
			Subject:   email,
			Audience:  jwt.ClaimStrings{emailTokenAudience},
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(emailTokenTTL)),
			ID:        models.NewID(),
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(s.key)
}

// CheckToken reports whether token proves that email was verified. It always
// returns the same VERIFICATION_REQUIRED error, whatever was wrong with it.
func (s *EmailVerificationService) CheckToken(token, rawEmail string) error {
	email := validation.NormalizeEmail(rawEmail)
	claims := &emailClaims{}
	_, err := jwt.ParseWithClaims(token, claims,
		func(*jwt.Token) (any, error) { return s.key, nil },
		jwt.WithValidMethods([]string{"HS256"}),
		jwt.WithIssuer(jwtIssuer),
		jwt.WithAudience(emailTokenAudience),
		jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(s.now),
		jwt.WithLeeway(5*time.Second),
	)
	if token == "" || err != nil || claims.Purpose != emailTokenPurpose ||
		subtle.ConstantTimeCompare([]byte(claims.Subject), []byte(email)) != 1 {
		return apperr.VerificationRequired()
	}
	return nil
}

func (s *EmailVerificationService) sendEmail(ctx context.Context, to, code string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	subject := "Your ShopBD verification code"
	text := fmt.Sprintf(
		"Your ShopBD verification code is %s.\r\n\r\n"+
			"It expires in 10 minutes. If you didn't try to create an account, you can ignore this email.\r\n\r\n"+
			"— ShopBD", code)
	html := fmt.Sprintf(`<div style="font-family:Arial,sans-serif;max-width:420px;margin:0 auto;padding:24px;color:#111">
<p style="font-size:16px;margin:0 0 16px">Use this code to verify your email and finish creating your ShopBD account:</p>
<p style="font-size:32px;font-weight:700;letter-spacing:8px;margin:0 0 16px">%s</p>
<p style="font-size:14px;color:#555;margin:0">It expires in 10 minutes. If you didn't try to create an account, you can ignore this email.</p>
</div>`, code)
	return s.mail.Send(ctx, to, subject, html, text)
}

// hashCode binds the code to the email, so a code sent to one address can
// never be replayed against another.
func (s *EmailVerificationService) hashCode(email, code string) string {
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(email + ":" + code))
	return hex.EncodeToString(mac.Sum(nil))
}

func isSixDigits(s string) bool {
	if len(s) != 6 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
