//go:build e2e && integration

package ci_test

import (
	"context"
	"net/http"

	"github.com/open-rails/helpers/auth"

	"github.com/open-rails/openrails"
)

// perm is a test host's own permission.
type perm string

func (p perm) String() string { return string(p) }

// staffGroups turns every staff route group on, and staffPermissions gives
// each its own permission, held in staffScope.
var (
	staffGroups      = openrails.RouteGroups{Admin: true, Catalog: true, MerchantConfig: true, Metrics: true}
	staffPermissions = openrails.Permissions{AdminRead: perm("root:billing:read"), AdminUpdate: perm("root:billing:manage"), Catalog: perm("root:catalog:manage"), MerchantConfig: perm("root:config:manage"), Metrics: perm("root:metrics:read")}
	staffScope       = openrails.Scope{Authority: "test", ID: "staff"}
)

// hostKey says every request is the host backend's API key, an application
// holding every permission in staffScope.
type hostKey struct{}

func (hostKey) Authenticate(*http.Request) (auth.Verified, error) { return hostKey{}, nil }

func (hostKey) Identity() openrails.Identity {
	return openrails.Identity{Issuer: "test", Subject: "test-host", SubjectKind: openrails.SubjectApplication,
		Invoker: openrails.Invoker{Issuer: "test", ID: "test-host"}, Credential: openrails.Credential{Kind: openrails.CredentialAPIKey, ID: "k_test"}}
}

func (hostKey) Can(_ context.Context, scope openrails.Scope, permission string) (bool, error) {
	return scope == staffScope && permission != "", nil
}
