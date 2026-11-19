// Package apperr defines the application error type that is rendered to
// clients as the predictable JSON error format.
package apperr

import (
	"math"
	"net/http"
	"time"
)

// Error codes returned to clients.
const (
	CodeBadRequest         = "BAD_REQUEST"
	CodeUnsupportedMedia   = "UNSUPPORTED_MEDIA_TYPE"
	CodeUnauthenticated    = "UNAUTHENTICATED"
	CodeInvalidCredentials = "INVALID_CREDENTIALS"
	CodeTokenReused        = "TOKEN_REUSED"
	CodeForbidden          = "FORBIDDEN"
	CodeAccountSuspended   = "ACCOUNT_SUSPENDED"
	CodeCSRFRejected       = "CSRF_REJECTED"
	CodeNotFound           = "NOT_FOUND"
	CodeMethodNotAllowed   = "METHOD_NOT_ALLOWED"
	CodeEmailTaken         = "EMAIL_TAKEN"
	CodeSlugTaken          = "SLUG_TAKEN"
	CodeDomainTaken        = "DOMAIN_TAKEN"
	CodeSlugChangeCooldown = "SLUG_CHANGE_COOLDOWN"
	CodeNameChangeLimit    = "NAME_CHANGE_LIMIT"
	CodeNameChangeCooldown = "NAME_CHANGE_COOLDOWN"
	CodeNameChangeConfirm  = "NAME_CHANGE_CONFIRMATION_REQUIRED"
	CodeValidationFailed   = "VALIDATION_FAILED"
	CodeRateLimited        = "RATE_LIMITED"
	CodeInternal           = "INTERNAL"
)

// Error is an error that knows how it should be presented over HTTP.
type Error struct {
	Status     int
	Code       string
	Message    string
	Fields     map[string]string
	RetryAfter int // seconds; only used for 429
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func New(status int, code, message string) *Error {
	return &Error{Status: status, Code: code, Message: message}
}

func BadRequest(message string) *Error {
	return New(http.StatusBadRequest, CodeBadRequest, message)
}

func UnsupportedMedia(message string) *Error {
	return New(http.StatusUnsupportedMediaType, CodeUnsupportedMedia, message)
}

func Unauthenticated(message string) *Error {
	return New(http.StatusUnauthorized, CodeUnauthenticated, message)
}

func InvalidCredentials() *Error {
	return New(http.StatusUnauthorized, CodeInvalidCredentials, "Invalid email or password")
}

func TokenReused() *Error {
	return New(http.StatusUnauthorized, CodeTokenReused, "Your session is no longer valid. Please sign in again.")
}

func Forbidden(message string) *Error {
	return New(http.StatusForbidden, CodeForbidden, message)
}

func AccountSuspended() *Error {
	return New(http.StatusForbidden, CodeAccountSuspended, "This account has been suspended")
}

func CSRFRejected(message string) *Error {
	return New(http.StatusForbidden, CodeCSRFRejected, message)
}

func NotFound() *Error {
	return New(http.StatusNotFound, CodeNotFound, "Resource not found")
}

func MethodNotAllowed() *Error {
	return New(http.StatusMethodNotAllowed, CodeMethodNotAllowed, "Method not allowed")
}

// Conflict is a 409 attached to a specific field (e.g. a duplicate email).
func Conflict(code, message, field string) *Error {
	e := New(http.StatusConflict, code, message)
	if field != "" {
		e.Fields = map[string]string{field: message}
	}
	return e
}

func Validation(fields map[string]string) *Error {
	e := New(http.StatusUnprocessableEntity, CodeValidationFailed, "Invalid input")
	e.Fields = fields
	return e
}

func RateLimited(wait time.Duration) *Error {
	secs := int(math.Ceil(wait.Seconds()))
	if secs < 1 {
		secs = 1
	}
	e := New(http.StatusTooManyRequests, CodeRateLimited, "Too many requests. Please try again later.")
	e.RetryAfter = secs
	return e
}

func Internal() *Error {
	return New(http.StatusInternalServerError, CodeInternal, "Something went wrong")
}
