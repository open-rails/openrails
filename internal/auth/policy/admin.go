package policy

import (
	"context"
	"net/http"

	"github.com/open-rails/openrails/billing"
)

// AdminPermissionChecker is the live AuthKit permission check the control
// plane provides for merchant-local `merchant:` permissions. It checks the
// user r authenticates as, the token's session included: a revoked session
// is an error joined with helpers/auth ErrRevoked.
type AdminPermissionChecker interface {
	ResolveAuthorizedMerchant(ctx context.Context, r *http.Request, merchantRef, perm string) (billing.MerchantID, string, error)
	// CheckRecentSignIn is the user's recent sign-in, with helpers/auth
	// RecentSignInChecker's errors.
	CheckRecentSignIn(ctx context.Context, r *http.Request) error
}
