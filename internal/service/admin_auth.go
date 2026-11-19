package service

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/mailer"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/password"
	"shopbd/backend/internal/repository"
)

// AdminAuthService gates the Super Admin panel with env credentials + email OTP.
type AdminAuthService struct {
	email     string // expected admin email (lowercase)
	password  string // expected plaintext password from env
	users     repository.UserRepository
	tokens    repository.TokenRepository
	auth      *AuthService
	jwtSecret []byte
	sender    mailer.Mailer
	hasher    *password.Hasher
	log       *slog.Logger
	now       func() time.Time

	// logDevOTP prints the OTP to the server log when email cannot be sent.
	// It must stay false in production: anyone who can read the logs would
	// otherwise get the second factor.
	logDevOTP bool

	mu   sync.Mutex
	otps map[string]adminOTP // email -> otp
}

// AllowOTPLogging lets the OTP be written to the log when SMTP is missing or
// failing. Call it with !cfg.IsProduction().
func (s *AdminAuthService) AllowOTPLogging(on bool) { s.logDevOTP = on }

// secureEqual compares two strings in constant time (it hashes both first so
// the length does not leak either).
func secureEqual(a, b string) bool {
	ha, hb := sha256.Sum256([]byte(a)), sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ha[:], hb[:]) == 1
}

type adminOTP struct {
	codeHash  string
	expiresAt time.Time
	attempts  int
}

func NewAdminAuthService(
	adminEmail, adminPassword string,
	users repository.UserRepository,
	tokens repository.TokenRepository,
	auth *AuthService,
	sender mailer.Mailer,
	log *slog.Logger,
) *AdminAuthService {
	return &AdminAuthService{
		email:     strings.ToLower(strings.TrimSpace(adminEmail)),
		password:  adminPassword,
		users:     users,
		tokens:    tokens,
		auth:      auth,
		jwtSecret: auth.cfg.JWTSecret,
		sender:    sender,
		hasher:    password.New(password.DefaultParams),
		log:       log,
		now:       time.Now,
		otps:      map[string]adminOTP{},
	}
}

func (s *AdminAuthService) Enabled() bool {
	return s.email != "" && s.password != ""
}

// Login checks env email/password and emails a 6-digit OTP.
func (s *AdminAuthService) Login(ctx context.Context, email, pass string) error {
	if !s.Enabled() {
		return apperr.Forbidden("Admin login is not configured")
	}
	email = strings.ToLower(strings.TrimSpace(email))
	// Both checks always run, in constant time, so timing reveals nothing.
	emailOK := secureEqual(email, s.email)
	passOK := secureEqual(pass, s.password)
	if !emailOK || !passOK {
		return apperr.InvalidCredentials()
	}

	code, err := randomDigits(6)
	if err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(code))
	s.mu.Lock()
	s.otps[email] = adminOTP{
		codeHash:  hex.EncodeToString(sum[:]),
		expiresAt: s.now().UTC().Add(10 * time.Minute),
		attempts:  0,
	}
	s.mu.Unlock()

	subject := "ShopBD Super Admin verification code"
	body := fmt.Sprintf("Your Super Admin login code is: %s\n\nIt expires in 10 minutes.\nIf you did not request this, ignore this email.\n", code)
	if s.sender == nil {
		if s.logDevOTP {
			s.log.Warn("admin OTP (no SMTP, development only)", "email", email, "code", code)
			return nil
		}
		s.dropOTP(email)
		return apperr.Forbidden("Admin login needs SMTP to be configured")
	}
	if err := s.sender.Send(ctx, email, subject, "<p>"+body+"</p>", body); err != nil {
		s.log.Error("admin OTP email failed", "error", err.Error())
		if s.logDevOTP {
			// Development only, so a broken SMTP setup does not lock you out.
			s.log.Warn("admin OTP fallback (development only)", "email", email, "code", code)
			return nil
		}
		s.dropOTP(email)
		return err
	}
	return nil
}

func (s *AdminAuthService) dropOTP(email string) {
	s.mu.Lock()
	delete(s.otps, email)
	s.mu.Unlock()
}

// VerifyOTP checks the code and issues a normal session for the ADMIN user.
func (s *AdminAuthService) VerifyOTP(ctx context.Context, email, code string) (*models.User, *TokenPair, error) {
	if !s.Enabled() {
		return nil, nil, apperr.Forbidden("Admin login is not configured")
	}
	email = strings.ToLower(strings.TrimSpace(email))
	if !secureEqual(email, s.email) {
		return nil, nil, apperr.InvalidCredentials()
	}

	s.mu.Lock()
	entry, ok := s.otps[email]
	if !ok || s.now().UTC().After(entry.expiresAt) {
		s.mu.Unlock()
		return nil, nil, apperr.Validation(map[string]string{"code": "Code expired or not found"})
	}
	if entry.attempts >= 5 {
		delete(s.otps, email)
		s.mu.Unlock()
		return nil, nil, apperr.Validation(map[string]string{"code": "Too many attempts"})
	}
	entry.attempts++
	s.otps[email] = entry
	sum := sha256.Sum256([]byte(strings.TrimSpace(code)))
	hash := hex.EncodeToString(sum[:])
	if !secureEqual(hash, entry.codeHash) {
		s.mu.Unlock()
		return nil, nil, apperr.Validation(map[string]string{"code": "Invalid code"})
	}
	delete(s.otps, email)
	s.mu.Unlock()

	u, err := s.ensureAdminUser(ctx)
	if err != nil {
		return nil, nil, err
	}
	pair, err := s.auth.issueTokens(ctx, u, models.NewID(), models.NewID())
	if err != nil {
		return nil, nil, err
	}
	return u, pair, nil
}

func (s *AdminAuthService) ensureAdminUser(ctx context.Context) (*models.User, error) {
	u, err := s.users.FindByEmail(ctx, s.email)
	if err == nil {
		if u.Status != models.StatusActive {
			_ = s.users.UpdateStatus(ctx, u.ID, models.StatusActive)
			u.Status = models.StatusActive
		}
		// Never turn an existing seller or buyer into an admin. If ADMIN_EMAIL is
		// the email of a normal account, silently changing its role would wipe
		// out that person's seller/buyer access. The admin must use their own
		// email instead.
		if u.Role != models.RoleAdmin {
			if s.log != nil {
				s.log.Error("ADMIN_EMAIL belongs to a non-admin account; refusing to change its role",
					"email", s.email, "role", string(u.Role))
			}
			return nil, apperr.Forbidden("ADMIN_EMAIL is already used by a " +
				strings.ToLower(string(u.Role)) + " account. Use a separate email for the admin.")
		}
		return u, nil
	}
	if err != repository.ErrNotFound {
		return nil, err
	}
	hash, err := s.hasher.Hash(s.password)
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	u = &models.User{
		ID:            models.NewID(),
		Name:          "Super Admin",
		Email:         s.email,
		EmailVerified: true,
		PasswordHash:  hash,
		Role:          models.RoleAdmin,
		Status:        models.StatusActive,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := s.users.Create(ctx, u); err != nil {
		return nil, err
	}
	return u, nil
}

func randomDigits(n int) (string, error) {
	const digits = "0123456789"
	out := make([]byte, n)
	for i := range out {
		// Rejection sampling: 250 is the largest multiple of 10 below 256,
		// so every digit is equally likely (no modulo bias).
		for {
			var b [1]byte
			if _, err := rand.Read(b[:]); err != nil {
				return "", err
			}
			if b[0] < 250 {
				out[i] = digits[int(b[0])%10]
				break
			}
		}
	}
	return string(out), nil
}

const adminGateTTL = 8 * time.Hour

// gateMAC signs a gate payload with HMAC-SHA256. The "admin-gate" prefix keeps
// this signature from being valid for anything else the JWT secret signs.
func (s *AdminAuthService) gateMAC(payload string) []byte {
	m := hmac.New(sha256.New, s.jwtSecret)
	m.Write([]byte("admin-gate|"))
	m.Write([]byte(payload))
	return m.Sum(nil)
}

// IssueGateToken returns a signed token proving admin 2FA completed.
func (s *AdminAuthService) IssueGateToken() (string, time.Duration, error) {
	now := s.now().UTC()
	exp := now.Add(adminGateTTL)
	payload := s.email + "|" + exp.UTC().Format(time.RFC3339)
	token := hex.EncodeToString(s.gateMAC(payload)) + "." + hex.EncodeToString([]byte(payload))
	return token, adminGateTTL, nil
}

// ValidGateToken checks the admin_gate cookie value.
func (s *AdminAuthService) ValidGateToken(token string) bool {
	if token == "" || !s.Enabled() {
		return false
	}
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		return false
	}
	raw, err := hex.DecodeString(parts[1])
	if err != nil {
		return false
	}
	payload := string(raw)
	got, err := hex.DecodeString(parts[0])
	if err != nil || !hmac.Equal(got, s.gateMAC(payload)) {
		return false
	}
	// payload = email|RFC3339
	idx := strings.LastIndex(payload, "|")
	if idx < 0 {
		return false
	}
	email, expStr := payload[:idx], payload[idx+1:]
	if !secureEqual(email, s.email) {
		return false
	}
	exp, err := time.Parse(time.RFC3339, expStr)
	if err != nil || s.now().UTC().After(exp) {
		return false
	}
	return true
}
