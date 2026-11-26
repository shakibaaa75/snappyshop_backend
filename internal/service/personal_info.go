package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/fraud"
	"shopbd/backend/internal/mailer"
	"shopbd/backend/internal/models"
	"shopbd/backend/internal/otp"
	"shopbd/backend/internal/password"
	"shopbd/backend/internal/repository"
	"shopbd/backend/internal/sms"
	"shopbd/backend/internal/validation"
)

const (
	personalCodeTTL      = 10 * time.Minute // the emails / texts tell people "10 minutes"
	personalCooldown     = 60 * time.Second // between two codes for the same purpose
	personalMaxAttempts  = 5                // wrong guesses per code
	personalSendPerUser  = 6                // codes per user per hour (all purposes)
	personalSendPerTo    = 3                // codes to one address / number per hour
	personalSendWindow   = time.Hour
	personalReauthLimit  = 10 // password checks per user per window
	personalReauthWindow = 15 * time.Minute
)

// PersonalInfo is the owner's private contact information. It is NOT the public
// storefront contact (store phone, WhatsApp ...), which lives with the store.
type PersonalInfo struct {
	Name          string
	Email         string
	EmailVerified bool
	Phone         string // 01XXXXXXXXX, "" = none
	PhoneVerified bool
	AvatarURL     string
	// HasPassword is false for accounts that only sign in with Google / Apple.
	// Such accounts must set a password before they can change email or phone.
	HasPassword bool
}

// PersonalAvailability says which verified changes can work on this server.
type PersonalAvailability struct {
	Email bool // codes can be stored and an email can be delivered
	Phone bool // codes can be stored and an SMS can be delivered
}

// PersonalInfoService changes the signed-in owner's email and phone. It reuses
// the existing user record and password; it does not add another login system.
//
//	email change : password + new address -> code to the NEW address -> confirm
//	               -> email replaced, old address notified
//	email verify : code to the current address -> confirm
//	phone change : password + new number  -> code by SMS -> confirm
type PersonalInfoService struct {
	users  repository.UserRepository
	hasher *password.Hasher
	codes  otp.Store // nil = no Redis: every code flow refuses with a clear error
	mail   mailer.Mailer
	sms    sms.Sender
	key    []byte
	now    func() time.Time
	devLog *slog.Logger // development only: print codes instead of requiring a provider
	log    *slog.Logger
}

func NewPersonalInfoService(
	users repository.UserRepository,
	hasher *password.Hasher,
	codes otp.Store,
	m mailer.Mailer,
	s sms.Sender,
	jwtSecret []byte,
) *PersonalInfoService {
	return &PersonalInfoService{
		users: users, hasher: hasher, codes: codes, mail: m, sms: s,
		key: otp.DeriveKey(jwtSecret), now: time.Now, log: slog.Default(),
	}
}

// LogCodesTo prints every email code to the log when SMTP is not configured.
// Development only. (SMS has its own development sender, sms.DevLog.)
func (s *PersonalInfoService) LogCodesTo(l *slog.Logger) { s.devLog = l }

// WithClock replaces the time source (tests only).
func (s *PersonalInfoService) WithClock(now func() time.Time) *PersonalInfoService {
	s.now = now
	return s
}

func (s *PersonalInfoService) emailReady() bool {
	return s.codes != nil && (mailer.Configured(s.mail) || s.devLog != nil)
}

func (s *PersonalInfoService) phoneReady() bool { return s.codes != nil && sms.Configured(s.sms) }

// Availability tells the UI which flows can work, so it can explain instead of
// failing after the person typed their password.
func (s *PersonalInfoService) Availability() PersonalAvailability {
	return PersonalAvailability{Email: s.emailReady(), Phone: s.phoneReady()}
}

// MissingDependencies names what is not set up, for the startup log.
func (s *PersonalInfoService) MissingDependencies() []string {
	var out []string
	if s.codes == nil {
		out = append(out, "Redis for one-time codes (set REDIS_URL or OTP_REDIS_URL)")
	}
	if !mailer.Configured(s.mail) {
		out = append(out, "email delivery (set SMTP_HOST and the other SMTP_* settings)")
	}
	if !sms.Configured(s.sms) {
		out = append(out, "SMS provider (implement sms.Sender and choose it in cmd/api/main.go)")
	}
	return out
}

func (s *PersonalInfoService) needEmail() error {
	switch {
	case s.codes == nil:
		return apperr.OTPStoreNotConfigured()
	case !s.emailReady():
		return apperr.EmailNotConfigured()
	}
	return nil
}

func (s *PersonalInfoService) needPhone() error {
	switch {
	case s.codes == nil:
		return apperr.OTPStoreNotConfigured()
	case !s.phoneReady():
		return apperr.SMSNotConfigured()
	}
	return nil
}

// Get returns the owner's personal information.
func (s *PersonalInfoService) Get(ctx context.Context, userID string) (*PersonalInfo, error) {
	u, err := s.activeUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	return infoOf(u), nil
}

func infoOf(u *models.User) *PersonalInfo {
	return &PersonalInfo{
		Name: u.Name, Email: u.Email, EmailVerified: u.EmailVerified,
		Phone: u.Phone, PhoneVerified: u.Phone != "" && u.PhoneVerified,
		AvatarURL: u.AvatarURL, HasPassword: u.PasswordHash != "",
	}
}

// ---------------------------------------------------------------- email change

// RequestEmailChange re-checks the password, then sends a code to the NEW
// address. The account is not touched until ConfirmEmailChange. It returns the
// seconds before another code may be requested.
func (s *PersonalInfoService) RequestEmailChange(ctx context.Context, userID, currentPassword, rawNewEmail string) (int, error) {
	if err := s.needEmail(); err != nil {
		return 0, err
	}
	u, err := s.activeUser(ctx, userID)
	if err != nil {
		return 0, err
	}
	newEmail := validation.NormalizeEmail(rawNewEmail)
	switch {
	case !validation.ValidEmail(newEmail):
		return 0, apperr.Validation(map[string]string{"newEmail": "Please enter a valid email address"})
	case newEmail == u.Email:
		return 0, apperr.Validation(map[string]string{"newEmail": "This is already your email address"})
	}
	if err := s.reauth(ctx, u, currentPassword); err != nil {
		return 0, err
	}
	if _, err := s.users.FindByEmail(ctx, newEmail); err == nil {
		return 0, apperr.Conflict(apperr.CodeEmailTaken, "An account with this email already exists", "newEmail")
	} else if !errors.Is(err, repository.ErrNotFound) {
		return 0, err
	}
	return s.sendEmailCode(ctx, otp.PurposeEmailChange, u.ID, newEmail)
}

// ConfirmEmailChange checks the code sent to newEmail and, if right, replaces
// the account's email (already verified, since the code proved it) and tells
// the previous address.
func (s *PersonalInfoService) ConfirmEmailChange(ctx context.Context, userID, rawNewEmail, code string) (*PersonalInfo, error) {
	if s.codes == nil {
		return nil, apperr.OTPStoreNotConfigured()
	}
	newEmail := validation.NormalizeEmail(rawNewEmail)
	if !validation.ValidEmail(newEmail) {
		return nil, apperr.Validation(map[string]string{"newEmail": "Please enter a valid email address"})
	}
	code = strings.TrimSpace(code)
	if !otp.IsSixDigits(code) {
		return nil, apperr.InvalidCode() // junk never costs an attempt
	}
	u, err := s.activeUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	target, err := s.verify(ctx, otp.PurposeEmailChange, u.ID, newEmail, code)
	if err != nil {
		return nil, err
	}
	oldEmail := u.Email
	if err := s.users.ChangeEmail(ctx, u.ID, oldEmail, target); err != nil {
		switch {
		case errors.Is(err, repository.ErrDuplicateEmail):
			return nil, apperr.Conflict(apperr.CodeEmailTaken, "An account with this email already exists", "newEmail")
		case errors.Is(err, repository.ErrEmailChanged):
			return nil, apperr.EmailChanged()
		case errors.Is(err, repository.ErrNotFound):
			return nil, apperr.Unauthenticated("Invalid or expired session")
		}
		return nil, err
	}
	s.notifyOldEmail(oldEmail, target)
	return s.Get(ctx, u.ID)
}

// notifyOldEmail tells the previous address that the sign-in email moved. It
// is best effort and off the request path: the change already happened.
func (s *PersonalInfoService) notifyOldEmail(oldEmail, newEmail string) {
	if !mailer.Configured(s.mail) {
		return
	}
	subject := "Your ShopBD email address was changed"
	when := s.now().UTC().Format("2 Jan 2006, 15:04 UTC")
	masked := maskEmail(newEmail)
	text := fmt.Sprintf("The email address on your ShopBD account was changed to %s on %s.\r\n\r\n"+
		"If you made this change, no action is needed.\r\n"+
		"If you did not, reset your password now and contact ShopBD support.\r\n\r\n— ShopBD", masked, when)
	htmlBody := fmt.Sprintf(`<div style="font-family:Arial,sans-serif;max-width:480px;margin:0 auto;padding:24px;color:#111">
<p style="font-size:16px;margin:0 0 12px">The email address on your ShopBD account was changed to <strong>%s</strong> on %s.</p>
<p style="font-size:14px;color:#555;margin:0 0 8px">If you made this change, no action is needed.</p>
<p style="font-size:14px;color:#555;margin:0">If you did not, reset your password now and contact ShopBD support.</p>
</div>`, maskedHTML(masked), when)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := s.mail.Send(ctx, oldEmail, subject, htmlBody, text); err != nil {
			s.log.Warn("email-change notice failed", "error", err.Error())
		}
	}()
}

// ---------------------------------------------------------------- email verify

// RequestEmailVerify sends a code to the account's current, unverified email.
func (s *PersonalInfoService) RequestEmailVerify(ctx context.Context, userID string) (int, error) {
	if err := s.needEmail(); err != nil {
		return 0, err
	}
	u, err := s.activeUser(ctx, userID)
	if err != nil {
		return 0, err
	}
	if u.EmailVerified {
		return 0, apperr.AlreadyVerified("email address")
	}
	return s.sendEmailCode(ctx, otp.PurposeEmailVerify, u.ID, u.Email)
}

// ConfirmEmailVerify marks the current email verified.
func (s *PersonalInfoService) ConfirmEmailVerify(ctx context.Context, userID, code string) (*PersonalInfo, error) {
	if s.codes == nil {
		return nil, apperr.OTPStoreNotConfigured()
	}
	code = strings.TrimSpace(code)
	if !otp.IsSixDigits(code) {
		return nil, apperr.InvalidCode()
	}
	u, err := s.activeUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	if u.EmailVerified {
		return infoOf(u), nil
	}
	target, err := s.verify(ctx, otp.PurposeEmailVerify, u.ID, u.Email, code)
	if err != nil {
		return nil, err
	}
	if err := s.users.MarkEmailVerified(ctx, u.ID, target); err != nil {
		if errors.Is(err, repository.ErrEmailChanged) {
			return nil, apperr.EmailChanged()
		}
		return nil, err
	}
	return s.Get(ctx, u.ID)
}

// ---------------------------------------------------------------- phone change

// RequestPhoneChange re-checks the password, then texts a code to the NEW
// number. Whether another account already owns the number is only revealed at
// confirmation, after the caller proved they hold it, so this endpoint cannot
// be used to probe which numbers are registered.
func (s *PersonalInfoService) RequestPhoneChange(ctx context.Context, userID, currentPassword, rawPhone string) (int, error) {
	if err := s.needPhone(); err != nil {
		return 0, err
	}
	u, err := s.activeUser(ctx, userID)
	if err != nil {
		return 0, err
	}
	phone, ok := fraud.NormalizePhone(rawPhone)
	if !ok {
		return 0, apperr.Validation(map[string]string{"phone": "Enter a valid Bangladesh mobile number, like 01XXXXXXXXX"})
	}
	if phone == u.Phone && u.PhoneVerified {
		return 0, apperr.Validation(map[string]string{"phone": "This is already your verified phone number"})
	}
	if err := s.reauth(ctx, u, currentPassword); err != nil {
		return 0, err
	}
	if err := s.sendLimits(ctx, u.ID, phone); err != nil {
		return 0, err
	}
	code, err := s.issue(ctx, otp.PurposePhoneChange, u.ID, phone)
	if err != nil {
		return 0, err
	}
	msg := fmt.Sprintf("Your ShopBD verification code is %s. It expires in 10 minutes. Never share it with anyone.", code)
	if err := s.sms.Send(ctx, phone, msg); err != nil {
		_ = s.codes.Cancel(context.WithoutCancel(ctx), otp.PurposePhoneChange, u.ID)
		if errors.Is(err, sms.ErrNotConfigured) {
			return 0, apperr.SMSNotConfigured()
		}
		s.log.Error("sms send failed", "error", err.Error())
		return 0, apperr.SendFailed("text message")
	}
	return int(personalCooldown.Seconds()), nil
}

// ConfirmPhoneChange checks the code and stores the verified phone.
func (s *PersonalInfoService) ConfirmPhoneChange(ctx context.Context, userID, rawPhone, code string) (*PersonalInfo, error) {
	if s.codes == nil {
		return nil, apperr.OTPStoreNotConfigured()
	}
	phone, ok := fraud.NormalizePhone(rawPhone)
	if !ok {
		return nil, apperr.Validation(map[string]string{"phone": "Enter a valid Bangladesh mobile number, like 01XXXXXXXXX"})
	}
	code = strings.TrimSpace(code)
	if !otp.IsSixDigits(code) {
		return nil, apperr.InvalidCode()
	}
	u, err := s.activeUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	target, err := s.verify(ctx, otp.PurposePhoneChange, u.ID, phone, code)
	if err != nil {
		return nil, err
	}
	if err := s.users.ChangePhone(ctx, u.ID, target, true); err != nil {
		switch {
		case errors.Is(err, repository.ErrDuplicatePhone):
			return nil, apperr.PhoneTaken()
		case errors.Is(err, repository.ErrNotFound):
			return nil, apperr.Unauthenticated("Invalid or expired session")
		}
		return nil, err
	}
	return s.Get(ctx, u.ID)
}

// ---------------------------------------------------------------- avatar

// SetAvatar stores the owner's personal profile picture ("" removes it) and
// returns the previous URL so the caller can delete the old file. The URL must
// come from an upload the caller already validated; this method only stores it.
func (s *PersonalInfoService) SetAvatar(ctx context.Context, userID, url string) (prev string, err error) {
	u, err := s.activeUser(ctx, userID)
	if err != nil {
		return "", err
	}
	if err := s.users.SetAvatar(ctx, u.ID, url); err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return "", apperr.Unauthenticated("Invalid or expired session")
		}
		return "", err
	}
	return u.AvatarURL, nil
}

// ---------------------------------------------------------------- shared steps

func (s *PersonalInfoService) activeUser(ctx context.Context, userID string) (*models.User, error) {
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

// reauth makes the owner type the account password again. A stolen session
// alone must not be able to move the email or phone. The attempts are counted
// per user (not just per IP) so the password cannot be guessed through this
// endpoint. A wrong password is a 422 on the field, never a 401, so the
// frontend does not mistake it for an expired session.
func (s *PersonalInfoService) reauth(ctx context.Context, u *models.User, pw string) error {
	if u.PasswordHash == "" {
		return apperr.PasswordRequired()
	}
	if pw == "" {
		return apperr.Validation(map[string]string{"currentPassword": "Enter your current password"})
	}
	allowed, wait, err := s.codes.Hit(ctx, "pi:reauth:"+u.ID, personalReauthLimit, personalReauthWindow)
	if err != nil {
		s.log.Error("otp store failed", "op", "reauth", "error", err.Error())
		return apperr.OTPStoreUnavailable()
	}
	if !allowed {
		return apperr.RateLimited(wait)
	}
	ok, err := s.hasher.Verify(pw, u.PasswordHash)
	if err != nil {
		return err
	}
	if !ok {
		return apperr.Validation(map[string]string{"currentPassword": "Incorrect password"})
	}
	return nil
}

// sendLimits caps how many codes one person may trigger and how many may go to
// one address or number (so the feature cannot be used to spam someone).
func (s *PersonalInfoService) sendLimits(ctx context.Context, userID, target string) error {
	sum := sha256.Sum256([]byte(strings.ToLower(target)))
	for _, h := range []struct {
		key   string
		limit int
	}{
		{"pi:send:user:" + userID, personalSendPerUser},
		{"pi:send:to:" + hex.EncodeToString(sum[:16]), personalSendPerTo},
	} {
		allowed, wait, err := s.codes.Hit(ctx, h.key, h.limit, personalSendWindow)
		if err != nil {
			s.log.Error("otp store failed", "op", "send-limit", "error", err.Error())
			return apperr.OTPStoreUnavailable()
		}
		if !allowed {
			return apperr.RateLimited(wait)
		}
	}
	return nil
}

// issue stores a fresh challenge and returns the plain code (to be sent, never
// stored). A code requested again within the cooldown is a 429 with the wait.
func (s *PersonalInfoService) issue(ctx context.Context, p otp.Purpose, userID, target string) (string, error) {
	code, err := otp.NewCode()
	if err != nil {
		return "", err
	}
	hash := otp.Hash(s.key, p, userID, target, code)
	if err := s.codes.Issue(ctx, p, userID, target, hash, personalCodeTTL, personalCooldown); err != nil {
		var cd *otp.CooldownError
		if errors.As(err, &cd) {
			return "", apperr.RateLimited(time.Duration(math.Ceil(cd.Wait.Seconds())) * time.Second)
		}
		s.log.Error("otp store failed", "op", "issue", "error", err.Error())
		return "", apperr.OTPStoreUnavailable()
	}
	return code, nil
}

func (s *PersonalInfoService) sendEmailCode(ctx context.Context, p otp.Purpose, userID, to string) (int, error) {
	if err := s.sendLimits(ctx, userID, to); err != nil {
		return 0, err
	}
	code, err := s.issue(ctx, p, userID, to)
	if err != nil {
		return 0, err
	}
	if s.devLog != nil && !mailer.Configured(s.mail) {
		s.devLog.Info("personal-info email code (development only)", "purpose", string(p), "to", to, "code", code)
		return int(personalCooldown.Seconds()), nil
	}

	mctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	intro := "Use this code to confirm your new email address on ShopBD:"
	if p == otp.PurposeEmailVerify {
		intro = "Use this code to verify your email address on ShopBD:"
	}
	text := fmt.Sprintf("%s\r\n\r\n%s\r\n\r\nIt expires in 10 minutes. If you didn't ask for this, you can ignore this email.\r\n\r\n— ShopBD", intro, code)
	htmlBody := fmt.Sprintf(`<div style="font-family:Arial,sans-serif;max-width:420px;margin:0 auto;padding:24px;color:#111">
<p style="font-size:16px;margin:0 0 16px">%s</p>
<p style="font-size:32px;font-weight:700;letter-spacing:8px;margin:0 0 16px">%s</p>
<p style="font-size:14px;color:#555;margin:0">It expires in 10 minutes. If you didn't ask for this, you can ignore this email.</p>
</div>`, intro, code)
	if err := s.mail.Send(mctx, to, "Your ShopBD verification code", htmlBody, text); err != nil {
		_ = s.codes.Cancel(context.WithoutCancel(ctx), p, userID)
		s.log.Error("verification email failed", "error", err.Error())
		return 0, apperr.SendFailed("email")
	}
	return int(personalCooldown.Seconds()), nil
}

// verify checks a code for (purpose, user, target) and maps the store's answer
// to the same error codes the sign-up verification uses.
func (s *PersonalInfoService) verify(ctx context.Context, p otp.Purpose, userID, target, code string) (string, error) {
	hash := otp.Hash(s.key, p, userID, target, code)
	got, err := s.codes.Verify(ctx, p, userID, hash, personalMaxAttempts)
	switch {
	case err == nil:
		return got, nil
	case errors.Is(err, otp.ErrNotFound):
		return "", apperr.CodeExpired()
	case errors.Is(err, otp.ErrInvalid):
		return "", apperr.InvalidCode()
	case errors.Is(err, otp.ErrTooManyAttempts):
		return "", apperr.TooManyAttempts()
	}
	s.log.Error("otp store failed", "op", "verify", "error", err.Error())
	return "", apperr.OTPStoreUnavailable()
}

// maskEmail shows enough to recognise an address without printing it in full:
// john@example.com -> j***@example.com.
func maskEmail(e string) string {
	at := strings.LastIndex(e, "@")
	if at <= 0 {
		return "***"
	}
	r := []rune(e[:at])
	return string(r[:1]) + "***" + e[at:]
}

// maskedHTML escapes the few characters that matter in the notice body.
func maskedHTML(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}
