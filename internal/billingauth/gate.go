package billingauth

import (
	"context"
	"errors"
	"net/http"

	auth "github.com/open-rails/helpers/auth"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
)

// Gate protects merchant-scoped routes.
type Gate interface {
	Authorize(ctx context.Context, r *http.Request, permission string) (Principal, error)
	// RequireRecentSignIn is nil when principal, which Authorize returned for
	// r, may perform an operation that moves money or grants access
	// (billing.RequiresRecentSignIn): a machine or delegated credential,
	// or a native user whose sign-in is recent. Otherwise it is the GateError
	// to answer.
	RequireRecentSignIn(ctx context.Context, r *http.Request, principal Principal) error
}

// Principal is the caller identity resolved by a Gate.
type Principal struct {
	MerchantID billing.MerchantID
	// Kind is the credential's provenance. Only a NativeUser carries a sign-in
	// of its own; an empty Kind is treated as one.
	Kind PrincipalKind
	// Subject is the opaque host identity verified by the Gate. Catalog owner
	// routes require it; request fields and headers never supply this authority.
	Subject     string
	UserContext UserContext
	// Permissions is the credential's resolved grant set for NON-USER principals
	// (API keys, service JWTs, host/delegated principals) — consumers that need
	// no-escalation checks (#757 api-key minting) read it. Empty for user
	// sessions: those carry UserContext.UserID and are checked against live
	// AuthKit group state instead.
	Permissions []string
}

// GateError maps authorization failures to stable HTTP responses. Code is the
// error's wire code (a host hook that names none answers its status's generic
// code), and Metadata its machine-readable context.
type GateError struct {
	Status   int
	Message  string
	Code     string
	Metadata map[string]any
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

// ErrRecentSignInUnavailable is a native user's credential whose auth provider
// cannot say how recently the user signed in.
var ErrRecentSignInUnavailable = errors.New("recent sign-in cannot be checked")

// RequireRecentSignIn is the shared Gate.RequireRecentSignIn verdict. check
// is the auth provider's helpers/auth RecentSignInChecker for the request; a
// native user without one is refused. A stale sign-in is 403
// step_up_required carrying the provider's challenge.
func RequireRecentSignIn(ctx context.Context, p Principal, check func(context.Context) error) error {
	if p.Kind == Machine || p.Kind == Delegated {
		return nil
	}
	var err error = ErrRecentSignInUnavailable
	if check != nil {
		err = check(ctx)
	}
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrRecentSignInUnavailable):
		return Refusal(billing.CodeStepUpUnavailable)
	case errors.Is(err, auth.ErrStepUpRequired):
		refusal := Refusal(billing.CodeStepUpRequired)
		var challenge interface{ Metadata() map[string]any }
		if errors.As(err, &challenge) {
			refusal.Metadata = challenge.Metadata()
		}
		return refusal
	case errors.Is(err, auth.ErrRevoked), errors.Is(err, auth.ErrExpired):
		return authenticationFailure(err)
	case errors.Is(err, auth.ErrForbidden):
		return Refusal(billing.CodePermissionRequired)
	default:
		return Refusal(billing.CodeAuthorizationUnavailable)
	}
}
