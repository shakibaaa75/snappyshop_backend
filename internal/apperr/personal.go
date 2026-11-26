package apperr

import "net/http"

// Error codes for the seller "Personal information" section.
const (
	// PASSWORD_REQUIRED: the account has no password (Google / Apple sign-up),
	// so it cannot re-enter one. The UI sends the person to "set a password".
	CodePasswordRequired = "PASSWORD_REQUIRED"
	// PHONE_TAKEN: another account already verified this phone number.
	CodePhoneTaken = "PHONE_TAKEN"
	// ALREADY_VERIFIED: nothing to verify.
	CodeAlreadyVerified = "ALREADY_VERIFIED"
	// EMAIL_CHANGED: the account's email changed while a code was pending.
	CodeEmailChanged = "EMAIL_CHANGED"
	// OTP_STORE_UNAVAILABLE: Redis is configured but the call failed. Distinct
	// from OTP_STORE_NOT_CONFIGURED (nothing set up at all).
	CodeOTPStoreUnavailable = "OTP_STORE_UNAVAILABLE"
	// SEND_FAILED: the email / SMS provider refused or timed out.
	CodeSendFailed = "SEND_FAILED"
)

func PasswordRequired() *Error {
	return New(http.StatusConflict, CodePasswordRequired,
		"Set a password for this account first, then try again.")
}

func PhoneTaken() *Error {
	return Conflict(CodePhoneTaken, "This phone number is already verified on another account", "phone")
}

func AlreadyVerified(what string) *Error {
	return New(http.StatusConflict, CodeAlreadyVerified, "Your "+what+" is already verified")
}

func EmailChanged() *Error {
	return New(http.StatusConflict, CodeEmailChanged,
		"Your email address changed in the meantime. Please start again.")
}

func OTPStoreUnavailable() *Error {
	return New(http.StatusServiceUnavailable, CodeOTPStoreUnavailable,
		"Verification codes are temporarily unavailable. Please try again in a moment.")
}

func SendFailed(channel string) *Error {
	return New(http.StatusServiceUnavailable, CodeSendFailed,
		"We couldn't send the "+channel+". Please try again in a moment.")
}
