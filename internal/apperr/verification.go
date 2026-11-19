package apperr

import "net/http"

// Error codes for the email verification flow. The frontend maps these to
// messages (see useEmailVerification).
const (
	CodeInvalidCode          = "INVALID_CODE"
	CodeCodeExpired          = "CODE_EXPIRED"
	CodeTooManyAttempts      = "TOO_MANY_ATTEMPTS"
	CodeVerificationRequired = "VERIFICATION_REQUIRED"
)

func InvalidCode() *Error {
	return New(http.StatusBadRequest, CodeInvalidCode, "That code isn't correct")
}

func CodeExpired() *Error {
	return New(http.StatusBadRequest, CodeCodeExpired, "That code has expired. Request a new one.")
}

func TooManyAttempts() *Error {
	return New(http.StatusBadRequest, CodeTooManyAttempts, "Too many wrong codes. Request a new one.")
}

func VerificationRequired() *Error {
	return New(http.StatusBadRequest, CodeVerificationRequired, "Please verify your email address first")
}
