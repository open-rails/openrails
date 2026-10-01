package policy

import (
	"context"
	"errors"
	"net/http"

	"github.com/open-rails/openrails/pkg/merchant"
)

// PermMerchantCatalogUpdate is the narrow merchant catalog mutation capability. It
// mirrors controlplane.PermMerchantCatalogUpdate (== merchant:catalog:update, #554)
// without making gin-free route registration import the control-plane package.
const PermMerchantCatalogUpdate = "merchant:catalog:update"

// AdminPermissionChecker is the live AuthKit permission check the control
// plane provides for merchant-local `merchant:` permissions. It checks the
// user r authenticates as, the token's session included: a revoked session
// is an error joined with helpers/auth ErrRevoked.
type AdminPermissionChecker interface {
	ResolveAuthorizedMerchant(ctx context.Context, r *http.Request, merchantRef, perm string) (merchant.ID, string, error)
	// CheckRecentSignIn is the user's recent sign-in, with helpers/auth
	// RecentSignInChecker's errors.
	CheckRecentSignIn(ctx context.Context, r *http.Request) error
}

var ErrPermissionRequired = errors.New("merchant permission required")
var ErrMerchantUnresolved = errors.New("merchant identity unresolved")
