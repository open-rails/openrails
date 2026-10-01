package billingauth

import (
	"context"
	"errors"
	"net/http"

	auth "github.com/open-rails/helpers/auth"

	"github.com/open-rails/openrails/pkg/merchant"
)

// Gate protects merchant-scoped routes.
type Gate interface {
	Authorize(ctx context.Context, r *http.Request, permission string) (Principal, error)
	// RequireRecentSignIn is nil when principal, which Authorize returned for
	// r, may perform an operation that moves money or grants access
	// (permissions.RequiresRecentSignIn): a machine or delegated credential,
	// or a native user whose sign-in is recent. Otherwise it is the GateError
	// to answer.
	RequireRecentSignIn(ctx context.Context, r *http.Request, principal Principal) error
}

// Principal is the caller identity resolved by a Gate.
type Principal struct {
	MerchantID merchant.ID
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

// GateError maps authorization failures to stable HTTP responses. Code, when
// set, is the error's wire code, and Metadata its machine-readable context.
type GateError struct {
	Status   int
	Message  string
	Code     string
	Metadata map[string]any
}

func (e GateError) Error() string { return e.Message }

// ErrRecentSignInUnavailable is a native user's credential whose auth provider
// cannot say how recently the user signed in.
var ErrRecentSignInUnavailable = errors.New("recent sign-in cannot be checked")

// RequireRecentSignIn is the shared Gate.RequireRecentSignIn verdict. check
// is the auth provider's helpers/auth RecentSignInChecker for the request; a
// native user without one is refused. A stale sign-in is 403
// step_up_required carrying the provider's challenge.
func RequireRecentSignIn(ctx context.Context, p Principal, check func(context.Context) error) error {
	if p.Kind == Machine || p.Kind == DelegatedUser {
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
		return GateError{Status: http.StatusForbidden, Message: "step_up_unavailable"}
	case errors.Is(err, auth.ErrStepUpRequired):
		refusal := GateError{Status: http.StatusForbidden, Message: "step_up_required", Code: "step_up_required"}
		var challenge interface{ Metadata() map[string]any }
		if errors.As(err, &challenge) {
			refusal.Metadata = challenge.Metadata()
		}
		return refusal
	case errors.Is(err, auth.ErrRevoked), errors.Is(err, auth.ErrExpired):
		return authenticationFailure(err)
	case errors.Is(err, auth.ErrForbidden):
		return GateError{Status: http.StatusForbidden, Message: "permission_required"}
	default:
		return GateError{Status: http.StatusServiceUnavailable, Message: "authorization unavailable"}
	}
}
