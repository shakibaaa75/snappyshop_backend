package apperr

import "net/http"

// Error codes for features that need infrastructure which is not set up on
// this server. They are 503s with their own code, so the frontend (and the
// person running the server) can tell WHICH dependency is missing instead of
// seeing a generic failure. The message stays generic for end users; the exact
// settings that are missing are logged at startup.
const (
	CodeEmailNotConfigured    = "EMAIL_NOT_CONFIGURED"     // no SMTP settings (SMTP_HOST ...)
	CodeSMSNotConfigured      = "SMS_NOT_CONFIGURED"       // no SMS provider
	CodeOTPStoreNotConfigured = "OTP_STORE_NOT_CONFIGURED" // no Redis for one-time codes
)

func EmailNotConfigured() *Error {
	return New(http.StatusServiceUnavailable, CodeEmailNotConfigured,
		"Email verification is not available right now. Please try again later.")
}

func SMSNotConfigured() *Error {
	return New(http.StatusServiceUnavailable, CodeSMSNotConfigured,
		"Phone verification is not available right now. Please try again later.")
}

func OTPStoreNotConfigured() *Error {
	return New(http.StatusServiceUnavailable, CodeOTPStoreNotConfigured,
		"Verification codes are not available right now. Please try again later.")
}
