package billingauth

import (
	"errors"
	"net/http"

	auth "github.com/open-rails/helpers/auth"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
)

// GateError maps authorization failures to stable HTTP responses. Code is the
// error's wire code (a host hook that names none answers its status's generic
// code), and Metadata its machine-readable context.
type GateError struct {
	Status   int
	Message  string
	Code     string
	Metadata map[string]any
	// Headers are set on the refusal (a DPoP challenge's WWW-Authenticate
	// and DPoP-Nonce).
	Headers map[string]string
	// Err is the helpers/auth class the refusal answers as, so a refusal of
	// OpenRails' own Authenticators classifies like a host's (auth.Refuse).
	Err error
}

func (e GateError) Error() string {
	if e.Message == "" {
		return e.Code
	}
	return e.Message
}

func (e GateError) Unwrap() error { return e.Err }

// Refusal is the GateError of a registered error code; message, when given,
// replaces the code's meaning as the human text.
func Refusal(code string, message ...string) GateError {
	info, ok := billing.LookupErrorCode(code)
	if !ok {
		return GateError{Status: http.StatusInternalServerError, Code: billing.CodeInternalError, Message: "unregistered refusal " + code}
	}
	out := GateError{Status: info.Status, Code: info.Code, Message: info.Meaning, Err: classOf(info.Code, info.Status)}
	if len(message) > 0 && message[0] != "" {
		out.Message = message[0]
	}
	return out
}

// classOf is the helpers/auth class of a registered refusal.
func classOf(code string, status int) error {
	switch code {
	case billing.CodeCredentialExpired:
		return errors.Join(auth.ErrUnauthenticated, auth.ErrExpired)
	case billing.CodeCredentialRevoked:
		return errors.Join(auth.ErrUnauthenticated, auth.ErrRevoked)
	case billing.CodeSenderProofRequired, billing.CodeDPoPNonceRequired:
		return errors.Join(auth.ErrUnauthenticated, auth.ErrSenderProofRequired)
	case billing.CodeStepUpRequired:
		return auth.ErrStepUpRequired
	}
	switch status {
	case http.StatusUnauthorized:
		return auth.ErrUnauthenticated
	case http.StatusForbidden:
		return auth.ErrForbidden
	case http.StatusServiceUnavailable:
		return auth.ErrUnavailable
	}
	return nil
}

// RefusalError is a refusal as the wire answers it: its code, or its status's
// generic code when a host hook named none.
func RefusalError(e GateError) *api.APIError {
	if e.Code == "" {
		simple := api.SimpleErrorResponse(e.Status, e.Message).Error
		return api.NewAPIError(e.Status, simple.Type, simple.Code, e.Message)
	}
	return api.NewAPIError(e.Status, api.TypeForCode(e.Status, e.Code), e.Code, e.Message).WithMetadata(e.Metadata)
}
