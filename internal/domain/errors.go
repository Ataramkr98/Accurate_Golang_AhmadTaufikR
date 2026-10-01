package domain

import (
	"fmt"
	"net/http"
)

// Standardized error codes exposed to the frontend.
//
// Every code that can appear in an APIError belongs here; the HTTP layer never
// invents its own literals, so the client switches on a closed set.
//
// Request shape:
//   - CodeValidation          a submitted value is unusable; may carry Fields
//   - CodePayloadTooLarge     the request body exceeded the accepted size
//   - CodeMethodNotAllowed    the path exists, but not for this HTTP method
//
// Identity and tenancy:
//   - CodeUnauthorized        missing or invalid credentials
//   - CodeForbidden           authenticated but not allowed
//   - CodeRateLimited         too many attempts inside the window
//
// Data integrity:
//   - CodeNotFound            the addressed record does not exist
//   - CodeDuplicate           a uniqueness rule rejected the write
//   - CodeConflict            a state transition lost a race
//   - CodeImmutable           a posted document cannot be edited
//   - CodeJournalNotBalanced  journal debits and credits differ
//
// Server-side conditions:
//   - CodeNotReady            a dependency (the database) is unavailable
//   - CodeInternal            unclassified failure; the message stays generic
//   - CodeDemoResetUnavailable the demo reset is not backed by a store that supports it
const (
	CodeValidation           = "VALIDATION_ERROR"
	CodePayloadTooLarge      = "PAYLOAD_TOO_LARGE"
	CodeMethodNotAllowed     = "METHOD_NOT_ALLOWED"
	CodeUnauthorized         = "UNAUTHORIZED"
	CodeForbidden            = "FORBIDDEN"
	CodeRateLimited          = "RATE_LIMITED"
	CodeNotFound             = "NOT_FOUND"
	CodeDuplicate            = "DUPLICATE"
	CodeConflict             = "CONFLICT"
	CodeImmutable            = "IMMUTABLE"
	CodeJournalNotBalanced   = "JOURNAL_NOT_BALANCED"
	CodeNotReady             = "NOT_READY"
	CodeInternal             = "INTERNAL_ERROR"
	CodeDemoResetUnavailable = "DEMO_RESET_UNAVAILABLE"
)

// Error is a domain error carrying a machine-readable code and HTTP status.
// Fields is optional and only populated for validation failures, where it maps
// a request field name to a human-readable reason so the client can highlight
// the offending input instead of showing a banner.
type Error struct {
	Code    string            `json:"code"`
	Message string            `json:"message"`
	Fields  map[string]string `json:"fields,omitempty"`
	Status  int               `json:"-"`
}

func (e *Error) Error() string {
	if len(e.Fields) == 0 {
		return fmt.Sprintf("%s: %s", e.Code, e.Message)
	}
	return fmt.Sprintf("%s: %s (%d field(s) rejected)", e.Code, e.Message, len(e.Fields))
}

// NewError builds a domain error.
func NewError(code, message string, status int) *Error {
	return &Error{Code: code, Message: message, Status: status}
}

// Common error constructors.
func ErrValidation(msg string) *Error {
	return NewError(CodeValidation, msg, http.StatusUnprocessableEntity)
}

// NewValidationError builds a 422 carrying per-field reasons. The map is copied
// so a caller cannot mutate the error after returning it.
func NewValidationError(message string, fields map[string]string) *Error {
	err := NewError(CodeValidation, message, http.StatusUnprocessableEntity)
	if len(fields) > 0 {
		err.Fields = make(map[string]string, len(fields))
		for name, reason := range fields {
			err.Fields[name] = reason
		}
	}
	return err
}

func ErrNotFound(msg string) *Error {
	return NewError(CodeNotFound, msg, http.StatusNotFound)
}

func ErrUnauthorized(msg string) *Error {
	return NewError(CodeUnauthorized, msg, http.StatusUnauthorized)
}

func ErrForbidden(msg string) *Error {
	return NewError(CodeForbidden, msg, http.StatusForbidden)
}

func ErrConflict(code, msg string) *Error {
	return NewError(code, msg, http.StatusConflict)
}

func ErrMethodNotAllowed(msg string) *Error {
	return NewError(CodeMethodNotAllowed, msg, http.StatusMethodNotAllowed)
}

// ErrPayloadTooLarge reports a body above the accepted size. It is deliberately
// distinct from a malformed body, which is a 422.
func ErrPayloadTooLarge(msg string) *Error {
	return NewError(CodePayloadTooLarge, msg, http.StatusRequestEntityTooLarge)
}

// ErrNotReady reports that a dependency the request needs is unavailable.
func ErrNotReady(msg string) *Error {
	return NewError(CodeNotReady, msg, http.StatusServiceUnavailable)
}
