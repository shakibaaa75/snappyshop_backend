// Package validation holds server-side input validation. Error messages mirror
// the zod messages used by the existing frontend forms.
package validation

import (
	"net/mail"
	"strings"
	"unicode/utf8"

	"shopbd/backend/internal/apperr"
	"shopbd/backend/internal/models"
)

// Errors maps JSON field names to human-readable messages.
type Errors map[string]string

// Err returns a 422 apperr when there are errors, otherwise nil.
func (e Errors) Err() error {
	if len(e) == 0 {
		return nil
	}
	return apperr.Validation(map[string]string(e))
}

const (
	msgEmail      = "Please enter a valid email address"
	msgPassword   = "Password must be at least 8 characters"
	msgPasswordMx = "Password must be at most 128 characters"
	msgName       = "Name must be at least 2 characters"
	msgNameMx     = "Name must be at most 100 characters"
	msgConfirm    = "Passwords do not match"
	msgRole       = "Please select an account type"
)

// NormalizeEmail trims and lowercases an email for storage and lookup.
func NormalizeEmail(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// ValidEmail reports whether s (already normalized) is a plain address.
func ValidEmail(s string) bool {
	if s == "" || len(s) > 254 || strings.ContainsAny(s, " \t\r\n<>,;\"") {
		return false
	}
	addr, err := mail.ParseAddress(s)
	if err != nil || addr.Address != s {
		return false
	}
	at := strings.LastIndex(s, "@")
	domain := s[at+1:]
	return at > 0 && strings.Contains(domain, ".") && !strings.HasPrefix(domain, ".") && !strings.HasSuffix(domain, ".")
}

// RegisterInput is the payload sent by the existing RegisterForm.
type RegisterInput struct {
	Name              string
	Email             string
	Password          string
	ConfirmPassword   string
	Role              string
	VerificationToken string
}

// ValidateRegister returns a normalized copy of in plus any field errors.
func ValidateRegister(in RegisterInput) (RegisterInput, Errors) {
	errs := Errors{}
	in.Name = strings.TrimSpace(in.Name)
	in.Email = NormalizeEmail(in.Email)

	switch n := utf8.RuneCountInString(in.Name); {
	case n < 2:
		errs["name"] = msgName
	case n > 100:
		errs["name"] = msgNameMx
	}
	if !ValidEmail(in.Email) {
		errs["email"] = msgEmail
	}
	switch {
	case utf8.RuneCountInString(in.Password) < 8:
		errs["password"] = msgPassword
	case len(in.Password) > 128:
		errs["password"] = msgPasswordMx
	}
	if in.Password != in.ConfirmPassword {
		errs["confirmPassword"] = msgConfirm
	}
	if in.Role != string(models.RoleBuyer) && in.Role != string(models.RoleSeller) {
		errs["role"] = msgRole
	}
	return in, errs
}

// LoginInput is the payload sent by the existing LoginForm.
type LoginInput struct {
	Email    string
	Password string
}

// ValidateLogin returns a normalized copy of in plus any field errors. The
// password is not length-checked beyond an upper bound so that the response
// does not reveal password rules to someone probing accounts.
func ValidateLogin(in LoginInput) (LoginInput, Errors) {
	errs := Errors{}
	in.Email = NormalizeEmail(in.Email)
	if !ValidEmail(in.Email) {
		errs["email"] = msgEmail
	}
	switch {
	case in.Password == "":
		errs["password"] = "Password is required"
	case len(in.Password) > 128:
		errs["password"] = msgPasswordMx
	}
	return in, errs
}
