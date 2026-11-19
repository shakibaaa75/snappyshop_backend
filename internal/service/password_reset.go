package service

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"time"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/jobs"
	"shopbd/backend/internal/mailer"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/password"
	"shopbd/backend/internal/repository"
	"shopbd/backend/internal/validation"
)

const (
	resetCodeTTL     = 5 * time.Minute // the reset email says "5 minutes"
	resetMaxAttempts = 5               // wrong guesses allowed per reset challenge
)

// ResetJobQueue runs work in the background. *jobs.Queue implements it.
type ResetJobQueue interface {
	Enqueue(j jobs.Job) bool
}

// How a reset email is retried when the mail server fails. The code lives for
// resetCodeTTL, so the retries have to finish well inside it.
const resetEmailAttempts = 4

var resetEmailBackoff = []time.Duration{2 * time.Second, 10 * time.Second, 30 * time.Second}

type PasswordResetService struct {
	users   repository.UserRepository
	resets  repository.PasswordResetRepository
	tokens  repository.TokenRepository
	hasher  *password.Hasher
	mail    mailer.Mailer
	key     []byte // HMAC key for code hashes, derived from the JWT secret
	now     func() time.Time
	newCode func() (string, error)

	queue        ResetJobQueue // nil = work runs inline / in a plain goroutine
	log          *slog.Logger
	devLog       *slog.Logger // development only: prints codes when SMTP is off
	retryBackoff []time.Duration
}

func NewPasswordResetService(
	users repository.UserRepository,
	resets repository.PasswordResetRepository,
	hasher *password.Hasher,
	m mailer.Mailer,
	jwtSecret []byte,
) *PasswordResetService {
	// A key derived from the JWT secret, with its own label, so a leaked
	// database of code hashes cannot be brute-forced offline without the
	// server secret, and the key is never reused for another purpose.
	mac := hmac.New(sha256.New, jwtSecret)
	mac.Write([]byte("shopbd/password-reset/v1"))
	return &PasswordResetService{
		users:   users,
		resets:  resets,
		hasher:  hasher,
		mail:    m,
		key:     mac.Sum(nil),
		now:     time.Now,
		newCode: generateCode,

		log:          slog.Default(),
		retryBackoff: resetEmailBackoff,
	}
}

// SetQueue makes reset work run in the background. Without it RequestResetAsync
// falls back to a plain goroutine and RequestReset sends mail inline.
func (s *PasswordResetService) SetQueue(q ResetJobQueue, log *slog.Logger) {
	s.queue = q
	if log != nil {
		s.log = log
	}
}

// LogCodesTo prints every code to the log. Development only: use it when SMTP
// is not configured so you can still finish a reset locally.
func (s *PasswordResetService) LogCodesTo(l *slog.Logger) { s.devLog = l }

// SetTokenRepo enables session revocation after a successful password reset.
// Optional for backward-compatible construction; recommended in production.
func (s *PasswordResetService) SetTokenRepo(tokens repository.TokenRepository) {
	s.tokens = tokens
}

func (s *PasswordResetService) RequestReset(ctx context.Context, email string) error {
	email = validation.NormalizeEmail(email)
	if email == "" {
		return nil
	}
	u, err := s.users.FindByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil // silently succeed — do not reveal account existence
		}
		return err
	}
	if u.Status != models.StatusActive {
		return nil // silent: suspended accounts get no reset email
	}

	code, err := s.newCode()
	if err != nil {
		return err
	}

	// A new request replaces any earlier challenge, and with it the attempt
	// counter, so each emailed code gets its own five guesses.
	_ = s.resets.DeleteByUserID(ctx, u.ID)

	now := s.now().UTC()
	id := models.NewID()
	pr := &models.PasswordReset{
		ID:        id,
		UserID:    u.ID,
		CodeHash:  s.hashCode(u.ID, id, code),
		ExpiresAt: now.Add(resetCodeTTL),
		CreatedAt: now,
	}
	if err := s.resets.Create(ctx, pr); err != nil {
		return err
	}

	if s.devLog != nil {
		s.devLog.Info("password reset code (development only)", "email", u.Email, "code", code)
	}
	return s.deliver(u.Email, code)
}

// RequestResetAsync is what the HTTP handler calls. It does no work for the
// caller: the account lookup, the database writes and the email all happen in
// the background. The request therefore takes the same time and does the same
// thing whether or not the address belongs to an account, so neither the
// response nor its timing reveals which addresses are registered. Problems are
// logged for the operator and never reach the caller.
func (s *PasswordResetService) RequestResetAsync(email string) {
	run := func(ctx context.Context) error {
		if err := s.RequestReset(ctx, email); err != nil {
			s.log.Error("password reset request failed", "error", err.Error())
		}
		// Never ask the queue to retry this step: a retry would issue a second
		// code and invalidate the first one while its email may already be on
		// the way. Only the email send is retried, in deliver.
		return nil
	}
	if s.queue == nil {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_ = run(ctx)
		}()
		return
	}
	if !s.queue.Enqueue(jobs.Job{Name: "password-reset-request", Run: run, MaxAttempts: 1, Timeout: 30 * time.Second}) {
		s.log.Warn("password reset request dropped: job queue full or stopped")
	}
}

// deliver sends the code. With a queue the send is a background job that is
// retried with the same code; without one it is sent now and the error is
// returned (used by tests and by callers that want to know).
func (s *PasswordResetService) deliver(to, code string) error {
	send := func(ctx context.Context) error { return s.sendEmail(ctx, to, code) }
	if s.queue == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return send(ctx)
	}
	ok := s.queue.Enqueue(jobs.Job{
		Name:        "password-reset-email",
		Run:         send,
		MaxAttempts: resetEmailAttempts,
		Backoff:     s.retryBackoff,
		Timeout:     20 * time.Second,
	})
	if !ok {
		return errors.New("password reset email not queued: job queue full or stopped")
	}
	return nil
}

// invalidResetCode is the one answer for every way a code can fail (unknown
// email, no challenge, expired, wrong, or too many guesses), so the response
// never reveals whether an account exists or why the code was refused.
func invalidResetCode() error {
	return apperr.Validation(map[string]string{"code": "Invalid or expired code"})
}

// ConfirmReset trades a valid code for a new password. A code is single-use
// and each challenge allows resetMaxAttempts wrong guesses.
func (s *PasswordResetService) ConfirmReset(ctx context.Context, email, code, newPassword string) error {
	email = validation.NormalizeEmail(email)
	code = strings.TrimSpace(code)

	// Check the password first. If it came after the code check, a "password
	// too short" reply would tell a guesser that the code was right.
	if n := len([]rune(newPassword)); n < 8 {
		return apperr.Validation(map[string]string{"password": "Password must be at least 8 characters"})
	} else if len(newPassword) > 128 {
		return apperr.Validation(map[string]string{"password": "Password must be at most 128 characters"})
	}

	// A malformed code can never match, so refuse it without using an attempt.
	if !isSixDigits(code) {
		return invalidResetCode()
	}

	u, err := s.users.FindByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return invalidResetCode()
		}
		return err
	}

	pr, err := s.resets.FindByUserID(ctx, u.ID)
	if err != nil {
		if errors.Is(err, repository.ErrResetNotFound) {
			return invalidResetCode()
		}
		return err
	}

	// Defence in depth: repository already filters expiresAt > now, but
	// always verify here so a buggy query cannot accept stale codes.
	now := s.now().UTC()
	if !pr.ExpiresAt.After(now) {
		_ = s.resets.DeleteByUserID(ctx, u.ID)
		return invalidResetCode()
	}

	// Count the guess first and atomically, so parallel requests cannot get
	// around the limit: each one receives a different count.
	n, err := s.resets.IncrementAttempts(ctx, pr.ID)
	if err != nil {
		if errors.Is(err, repository.ErrResetNotFound) {
			return invalidResetCode()
		}
		return err
	}
	if n > resetMaxAttempts {
		_ = s.resets.DeleteByUserID(ctx, u.ID)
		return invalidResetCode()
	}

	want := s.hashCode(u.ID, pr.ID, code)
	if subtle.ConstantTimeCompare([]byte(pr.CodeHash), []byte(want)) != 1 {
		if n >= resetMaxAttempts {
			// Out of guesses: the code is dead, the user must request a new one.
			_ = s.resets.DeleteByUserID(ctx, u.ID)
		}
		return invalidResetCode()
	}

	hash, err := s.hasher.Hash(newPassword)
	if err != nil {
		return err
	}

	// Burn the code before changing the password. Consume is atomic, so when
	// two requests present the right code at once only one of them proceeds.
	won, err := s.resets.Consume(ctx, pr.ID)
	if err != nil {
		return err
	}
	if !won {
		return invalidResetCode()
	}

	if err := s.users.UpdatePassword(ctx, u.ID, hash); err != nil {
		return err
	}

	// Invalidate every existing session so a stolen refresh cookie cannot
	// keep access after the password change.
	if s.tokens != nil {
		_ = s.tokens.RevokeAllForUser(ctx, u.ID, now)
	}
	return nil
}

func (s *PasswordResetService) sendEmail(ctx context.Context, to, code string) error {
	subject := "Reset your ShopBD password"
	text := fmt.Sprintf(
		"Hello,\r\n\r\n"+
			"Your password reset code is: %s\r\n\r\n"+
			"This code expires in 5 minutes. If you didn't request this, you can ignore this email.\r\n\r\n"+
			"— ShopBD", code)
	html := fmt.Sprintf(`<div style="font-family:Arial,sans-serif;max-width:420px;margin:0 auto;padding:24px;color:#111">
<p style="font-size:16px;margin:0 0 16px">Use this code to reset your ShopBD password:</p>
<p style="font-size:32px;font-weight:700;letter-spacing:8px;margin:0 0 16px">%s</p>
<p style="font-size:14px;color:#555;margin:0">It expires in 5 minutes. If you didn't request this, you can ignore this email.</p>
</div>`, code)
	return s.mail.Send(ctx, to, subject, html, text)
}

// generateCode returns a uniformly random 6-digit code. It draws a whole
// number below 1,000,000 from crypto/rand instead of reducing random bytes
// with "% 10", which would make some digits slightly more likely than others.
func generateCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

// hashCode is an HMAC keyed with the server secret and bound to both the user
// and this particular challenge, so a stored hash is useless for brute force
// without the key and can never be replayed against another user or request.
func (s *PasswordResetService) hashCode(userID, resetID, code string) string {
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(userID + ":" + resetID + ":" + code))
	return hex.EncodeToString(mac.Sum(nil))
}
