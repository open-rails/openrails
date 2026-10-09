package openrails

import (
	"github.com/open-rails/openrails/billing"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
)

// Permissions are the merchant permissions a person may hold: every one the
// merchant API asks Auth.RequirePermission, `merchant:<resource>:<action>`.
// Register them in your RBAC (an AuthKit "merchant" persona) and grant them
// to roles like any other permission. MachinePermissions are not among them.
func Permissions() []string {
	var out []string
	for _, p := range httproutes.Permissions() {
		if !billing.MachineOnly(p) {
			out = append(out, p)
		}
	}
	return out
}

// MachinePermissions are the merchant permissions only a machine credential
// (an API key, a service token) may hold: OpenRails refuses them to a person
// whatever the host's roles say. Grant them to machine roles only.
func MachinePermissions() []string {
	var out []string
	for _, p := range httproutes.Permissions() {
		if billing.MachineOnly(p) {
			out = append(out, p)
		}
	}
	return out
}
