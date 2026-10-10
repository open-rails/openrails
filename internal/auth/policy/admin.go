package policy

import (
	"context"
	"net/http"

	"github.com/open-rails/openrails/billing"
)

// AdminPermissionChecker is the control plane's live AuthKit check of
// merchant-local `merchant:` permissions for the user r authenticates as. A
// revoked session is an error joined with helpers/auth ErrRevoked.
type AdminPermissionChecker interface {
	ResolveAuthorizedMerchant(ctx context.Context, r *http.Request, merchantRef, perm string) (billing.MerchantID, string, error)
	// CheckRecentSignIn is the user's recent sign-in, with helpers/auth
	// RecentSignInChecker's errors.
	CheckRecentSignIn(ctx context.Context, r *http.Request) error
}
