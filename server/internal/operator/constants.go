package operator

import (
	"github.com/open-rails/openrails/server/internal/controlplane"
)

// MerchantType and CustomerType are OpenRails' permission-group personas, for
// hosts inspecting AuthKit memberships.
var (
	MerchantType = controlplane.MerchantType
	CustomerType = controlplane.CustomerType
)
