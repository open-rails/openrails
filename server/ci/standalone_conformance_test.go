//go:build e2e && integration

package ci_test

import (
	"net/http"
	"testing"

	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	helpersauthtest "github.com/open-rails/helpers/auth/authtest"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/openrailstest"
	"github.com/open-rails/openrails/server"
	"github.com/open-rails/openrails/server/internal/controlplane"
)

// The standalone server's Authenticator, AuthKit's, passes the conformance
// kit a host runs, in a merchant's scope (its AuthKit group): the owner's
// session holding the merchant permissions there and nowhere else, a viewer
// holding only the reads, a user holding none, a stale sign-in, the
// merchant's owner API key, and the owner signed out last.
func TestStandaloneAuthPassesCheckAuth(t *testing.T) {
	f := newFixture(t)
	cp := f.newServer(t, reserving())
	ctx := t.Context()
	ak := cp.AuthKit()
	owner, viewer, stranger, gone := newAccount(t, cp), newAccount(t, cp), newAccount(t, cp), newAccount(t, cp)
	provisioned, err := cp.ProvisionMerchant(ctx, billing.ProvisionMerchantParams{Slug: uniqueName("conform"), OwnerUserID: owner.ID})
	require.NoError(t, err)
	mid := provisioned.MerchantID
	key := merchantKey(t, cp, mid, "owner")
	signedOut := authtest.SignIn(t, ak, gone).AccessToken
	_, err = ak.RevokeAccountSessions(ctx, iam.UserIdentity(gone.ID), gone.ID)
	require.NoError(t, err)

	root, err := ak.Scope(ctx, iam.RootGroup())
	require.NoError(t, err)
	scope, err := cp.MerchantScope(ctx, mid)
	require.NoError(t, err)
	require.NotEqual(t, root, scope, "a merchant's scope is its own group")
	reads, ok := controlplane.MerchantRole("viewer")
	require.True(t, ok)
	authtest.GrantRole(t, ak, iam.GroupByID(scope.ID), iam.UserSubject(viewer.ID), reads)

	request := func(token string) func() *http.Request {
		return bearerRequest(token)
	}
	session := authtest.SignIn(t, ak, owner).AccessToken
	openrailstest.CheckAuth(t, openrails.Routes{
		Auth: ak.Authenticator(), Scope: scope, RouteGroups: openrails.RouteGroups{Admin: true, Programmatic: true},
		Permissions: openrails.Permissions{AdminRead: perm(server.MerchantBillingRead), AdminUpdate: perm(server.MerchantBillingManage)},
	}, helpersauthtest.Cases{
		Staff:       request(session),
		User:        request(authtest.SignIn(t, ak, stranger).AccessToken),
		Holders:     map[string]func() *http.Request{server.MerchantBillingRead: request(authtest.SignIn(t, ak, viewer).AccessToken)},
		Stale:       request(authtest.StaleSession(t, ak, authtest.SignIn(t, ak, owner).AccessToken)),
		Application: request(key.Secret),
		Refused:     map[string]func() *http.Request{"forged": request(session + "x"), "signed out": request(signedOut), "a forged key": request(key.Secret + "x")},
		Revoke: func() {
			_, err := ak.RevokeAccountSessions(ctx, iam.UserIdentity(owner.ID), owner.ID)
			require.NoError(t, err)
		},
	})
}
