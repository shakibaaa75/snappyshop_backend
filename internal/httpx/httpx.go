// Package httpx contains small HTTP helpers: JSON responses, centralized error
// rendering, and strict JSON request decoding.
package httpx

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5/middleware"

	"shopbd/backend/internal/apperr"
)

// MaxBodyBytes is the default request body limit.
const MaxBodyBytes = 1 << 20 // 1 MiB

type errorBody struct {
	Error errorPayload `json:"error"`
}

type errorPayload struct {
	Code      string            `json:"code"`
	Message   string            `json:"message"`
	Fields    map[string]string `json:"fields,omitempty"`
	RequestID string            `json:"requestId,omitempty"`
}

// WriteJSON writes v as JSON with the given status. A nil v writes no body.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	if v == nil {
		w.WriteHeader(status)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// WriteError renders any error in the standard format. Errors that are not
// *apperr.Error are logged and returned as a generic 500 so internals are
// never leaked to clients.
func WriteError(w http.ResponseWriter, r *http.Request, err error, log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	var ae *apperr.Error
	if !errors.As(err, &ae) {
		log.Error("unhandled error", "error", err.Error(), "path", r.URL.Path, "request_id", middleware.GetReqID(r.Context()))
		ae = apperr.Internal()
	}
	if ae.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(ae.RetryAfter))
	}
	WriteJSON(w, ae.Status, errorBody{Error: errorPayload{
		Code:      ae.Code,
		Message:   ae.Message,
		Fields:    ae.Fields,
		RequestID: middleware.GetReqID(r.Context()),
	}})
}

// DecodeJSON decodes a single JSON object into dst. It requires a JSON
// content type, limits the body size, and rejects unknown fields so clients
// cannot smuggle server-managed fields (mass assignment).
//
// Use this for auth, admin, order, and anything else where a client must not
// be able to set server-managed fields. For public, write-only telemetry
// (analytics events), use DecodeJSONLenient instead.
func DecodeJSON(w http.ResponseWriter, r *http.Request, dst any, maxBytes int64) error {
	return decodeJSON(w, r, dst, maxBytes, true)
}

// DecodeJSONLenient behaves exactly like DecodeJSON but ignores unknown fields.
//
// Use this for public, write-only endpoints whose input is a telemetry payload
// the frontend owns (e.g. analytics events). The service layer validates the
// fields it actually consumes; extra fields are silently ignored rather than
// 422'ing the whole request and silently dropping the event.
func DecodeJSONLenient(w http.ResponseWriter, r *http.Request, dst any, maxBytes int64) error {
	return decodeJSON(w, r, dst, maxBytes, false)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any, maxBytes int64, strict bool) error {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/json" {
		return apperr.UnsupportedMedia("Content-Type must be application/json")
	}
	if maxBytes <= 0 {
		maxBytes = MaxBodyBytes
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	dec := json.NewDecoder(r.Body)
	if strict {
		dec.DisallowUnknownFields()
	}
	if err := dec.Decode(dst); err != nil {
		return mapDecodeError(err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return apperr.BadRequest("Request body must contain a single JSON object")
	}
	return nil
}

func mapDecodeError(err error) error {
	var maxErr *http.MaxBytesError
	var typeErr *json.UnmarshalTypeError
	var syntaxErr *json.SyntaxError
	switch {
	case errors.As(err, &maxErr):
		return apperr.BadRequest("Request body too large")
	case errors.Is(err, io.EOF):
		return apperr.BadRequest("Request body is empty")
	case errors.As(err, &syntaxErr), errors.Is(err, io.ErrUnexpectedEOF):
		return apperr.BadRequest("Malformed JSON")
	case errors.As(err, &typeErr):
		field := typeErr.Field
		if field == "" {
			field = "body"
		}
		return apperr.Validation(map[string]string{field: "Invalid type"})
	case strings.HasPrefix(err.Error(), "json: unknown field "):
		name := strings.Trim(strings.TrimPrefix(err.Error(), "json: unknown field "), `"`)
		return apperr.Validation(map[string]string{name: "Unknown field"})
	default:
		return apperr.BadRequest("Malformed JSON")
	}
}
