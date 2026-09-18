// Package apperr is the service-layer error model: a refusal carries the HTTP
// status and stable wire code a handler answers with, so classification never
// reads a human message. It is the server-side twin of openrails.StatusError
// and classifies exactly like the StatusError a Client receives for it.
package apperr

import (
	"fmt"
	"net/http"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/pkg/api"
)

// Error is a typed refusal. Status and Code are frozen contract; Message is
// diagnostic and free to change; Param names the offending field when known.
type Error struct {
	Status  int
	Code    string
	Message string
	Param   string
}

// New declares a refusal sentinel. Wrap it with %w to add call-site detail.
func New(status int, code, message string) *Error {
	return &Error{Status: status, Code: code, Message: message}
}

// Invalidf is an ad-hoc request-validation refusal (400, invalid_param).
func Invalidf(format string, args ...any) *Error {
	return &Error{Status: http.StatusBadRequest, Code: api.CodeInvalidParam, Message: fmt.Sprintf(format, args...)}
}

// Conflictf is an ad-hoc state-conflict refusal (409, resource_conflict).
func Conflictf(format string, args ...any) *Error {
	return &Error{Status: http.StatusConflict, Code: api.CodeResourceConflict, Message: fmt.Sprintf(format, args...)}
}

// WithParam returns a copy naming the offending request field.
func (e *Error) WithParam(param string) *Error {
	out := *e
	out.Param = param
	return &out
}

func (e *Error) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return e.Code
}

// ErrorCode is the wire error code.
func (e *Error) ErrorCode() string { return e.Code }

// Is matches another refusal with the same status and code, and every root
// openrails class sentinel (ErrInvalid, ErrNotFound, ErrConflict, ...) the
// equivalent StatusError matches.
func (e *Error) Is(target error) bool {
	if other, ok := target.(*Error); ok {
		return other.Status == e.Status && other.Code == e.Code
	}
	return (&openrails.StatusError{Status: e.Status, ErrorDetails: openrails.ErrorDetails{Code: e.Code}}).Is(target)
}
