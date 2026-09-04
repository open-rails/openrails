package policy

import (
	"context"
	"errors"
	"github.com/open-rails/openrails/pkg/merchant"
)

// PermMerchantCatalogUpdate is the narrow merchant catalog mutation capability. It
// mirrors controlplane.PermMerchantCatalogUpdate (== merchant:catalog:update, #554)
// without making gin-free route registration import the control-plane package.
const PermMerchantCatalogUpdate = "merchant:catalog:update"

// AdminPermissionChecker is the live AuthKit effective-permission check the
// control plane provides for merchant-local `merchant:` permissions.
type AdminPermissionChecker interface {
	ResolveAuthorizedMerchant(ctx context.Context, merchantRef, userID, perm string) (merchant.ID, string, error)
}

var ErrPermissionRequired = errors.New("merchant permission required")
var ErrMerchantUnresolved = errors.New("merchant identity unresolved")
