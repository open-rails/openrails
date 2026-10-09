package billingauth

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

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
}

func (e GateError) Error() string {
	if e.Message == "" {
		return e.Code
	}
	return e.Message
}

// Refusal is the GateError of a registered error code; message, when given,
// replaces the code's meaning as the human text.
func Refusal(code string, message ...string) GateError {
	info, ok := billing.LookupErrorCode(code)
	if !ok {
		return GateError{Status: http.StatusInternalServerError, Code: billing.CodeInternalError, Message: "unregistered refusal " + code}
	}
	out := GateError{Status: info.Status, Code: info.Code, Message: info.Meaning}
	if len(message) > 0 && message[0] != "" {
		out.Message = message[0]
	}
	return out
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

// ErrRecentSignInUnavailable is a credential whose provider cannot say how
// recently its user signed in.
var ErrRecentSignInUnavailable = errors.New("recent sign-in cannot be checked")

// WriteRefusal answers a refusal from net/http middleware: its headers (a
// DPoP challenge) and the error envelope.
func WriteRefusal(w http.ResponseWriter, r *http.Request, e GateError) {
	if e.Code == billing.CodeSenderProofRequired && e.Headers["WWW-Authenticate"] == "" {
		w.Header().Set("WWW-Authenticate", `DPoP error="invalid_dpop_proof", algs="ES256"`)
	}
	for name, value := range e.Headers {
		w.Header().Set(name, value)
	}
	body := RefusalError(e)
	if id := strings.TrimSpace(w.Header().Get("X-Request-ID")); id != "" {
		body.WithRequestID(id)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.Status)
	_ = json.NewEncoder(w).Encode(body.ToResponse())
}

// AsRefusal is err as the refusal it answers: a coded refusal keeps its
// code, a credential failure its own, anything else is an outage.
func AsRefusal(err error) GateError {
	var gate GateError
	switch {
	case errors.As(err, &gate) && gate.Status >= 400:
		return gate
	case errors.Is(err, auth.ErrExpired), errors.Is(err, auth.ErrRevoked), errors.Is(err, auth.ErrSenderProofRequired), errors.Is(err, auth.ErrUnauthenticated):
		return Unauthenticated(err)
	case errors.Is(err, auth.ErrStepUpRequired):
		refusal := Refusal(billing.CodeStepUpRequired)
		var challenge interface{ Metadata() map[string]any }
		if errors.As(err, &challenge) {
			refusal.Metadata = challenge.Metadata()
		}
		return refusal
	case errors.Is(err, ErrRecentSignInUnavailable):
		return Refusal(billing.CodeStepUpUnavailable)
	case errors.Is(err, auth.ErrForbidden):
		return Refusal(billing.CodePermissionRequired)
	case errors.Is(err, auth.ErrUnavailable):
		return Refusal(billing.CodeAuthenticationUnavailable)
	}
	return Refusal(billing.CodeAuthorizationUnavailable)
}
