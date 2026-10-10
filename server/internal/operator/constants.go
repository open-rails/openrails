package operator

import (
	"github.com/open-rails/authkit/iam"

	"github.com/open-rails/openrails/server/internal/controlplane"
)

// MerchantType and CustomerType are OpenRails' permission-group personas, for
// hosts inspecting AuthKit memberships.
var (
	MerchantType = controlplane.MerchantType
	CustomerType = controlplane.CustomerType
)

// CustomerGroup addresses a customer's own permission group, keyed by the
// customer's user id. Merchant groups are addressed by their stored id.
func CustomerGroup(customerID string) iam.GroupRef {
	return controlplane.CustomerGroup(customerID)
}
