package controlplane

import (
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/pkg/merchant"
)

// NameAuthority exposes the attached control plane's merchant-group directory
// for catalog exports and other separately constructed name-bearing tools.
func NameAuthority(a *app.App) merchant.NameAuthority {
	cp := Get(a)
	if cp == nil {
		return nil
	}
	return controlplane.MerchantNameAuthority(cp.Core())
}

// GroupDirectory is AuthKit's read-only identity projection contract.
type GroupDirectory = controlplane.GroupDirectory

// NewNameAuthority adapts a host-owned AuthKit client or lightweight directory
// without constructing an OpenRails control plane.
func NewNameAuthority(groups GroupDirectory) merchant.NameAuthority {
	return controlplane.MerchantNameAuthority(groups)
}
