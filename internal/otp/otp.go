// Package otp stores short-lived one-time codes and the counters around them
// (expiry, wrong-guess attempts, resend cooldown, send rate limits).
//
// Only a hash of the code is stored, never the code. The caller (a service)
// generates the code, hashes it with Hash, sends the plain code to the person
// and hands only the hash to the Store.
//
// A Store fails CLOSED: any Redis error is returned to the caller, which must
// refuse the request. It never falls back to "allow".
package otp

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"time"
)

// Purpose says what a code is for. A code issued for one purpose can never be
// used for another.
type Purpose string

const (
	PurposeEmailChange Purpose = "email_change" // code sent to the NEW email
	PurposeEmailVerify Purpose = "email_verify" // code sent to the current, unverified email
	PurposePhoneChange Purpose = "phone_change" // code sent to the NEW phone
	PurposePhoneVerify Purpose = "phone_verify" // code sent to the current, unverified phone
)

var (
	// ErrNotFound: no active code (never issued, expired, used, or locked out).
	ErrNotFound = errors.New("otp: no active code")
	// ErrInvalid: the code is wrong; attempts remain.
	ErrInvalid = errors.New("otp: wrong code")
	// ErrTooManyAttempts: the last allowed guess was used (or exceeded). The
	// code is deleted; a new one must be requested.
	ErrTooManyAttempts = errors.New("otp: too many wrong attempts")
)

// CooldownError is returned by Issue when a code was sent too recently.
type CooldownError struct{ Wait time.Duration }

func (e *CooldownError) Error() string {
	return fmt.Sprintf("otp: resend available in %s", e.Wait.Round(time.Second))
}

// Store keeps one active challenge per (purpose, subject). subject is a user ID.
type Store interface {
	// Issue replaces any earlier challenge for (purpose, subject) with a fresh
	// one: target is what the code was sent to (the new email / phone), hash is
	// Hash(...) of the code. If a code was issued less than cooldown ago it
	// returns *CooldownError and changes nothing.
	Issue(ctx context.Context, p Purpose, subject, target, hash string, ttl, cooldown time.Duration) error

	// Verify counts one attempt and compares hash with the stored one.
	// On success the challenge is consumed (it works once) and the stored
	// target is returned. Errors: ErrNotFound, ErrInvalid, ErrTooManyAttempts.
	Verify(ctx context.Context, p Purpose, subject, hash string, maxAttempts int) (target string, err error)

	// Cancel drops the active challenge, if any.
	Cancel(ctx context.Context, p Purpose, subject string) error

	// Hit counts one event under key and reports whether it is still within
	// limit events per window. When it is not, retryAfter says how long until
	// the window resets. Used for send limits (per user, per target).
	Hit(ctx context.Context, key string, limit int, window time.Duration) (allowed bool, retryAfter time.Duration, err error)
}

// NewCode returns a uniformly random 6-digit code ("000000"-"999999").
func NewCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

// IsSixDigits reports whether s looks like a code (cheap check before any
// Redis call, so junk input never costs an attempt).
func IsSixDigits(s string) bool {
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

// Hash binds a code to its purpose, owner and target, so a code issued for
// one address or number can never be replayed against another. key is a
// server secret (derive it from the JWT secret, see DeriveKey).
func Hash(key []byte, p Purpose, subject, target, code string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(string(p) + "\x00" + subject + "\x00" + target + "\x00" + code))
	return hex.EncodeToString(mac.Sum(nil))
}

// DeriveKey makes a purpose-specific key from the JWT secret, so these hashes
// can never double as any other token signature.
func DeriveKey(secret []byte) []byte {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte("shopbd/otp/v1"))
	return mac.Sum(nil)
}
