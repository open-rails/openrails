//go:build e2e && integration

package ci_test

import (
	"net/http"
	"testing"

	"github.com/open-rails/authkit/auth"
	helpersauthtest "github.com/open-rails/authkit/auth/authtest"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/openrailstest"
	"github.com/open-rails/openrails/server/internal/controlplane"
)

// The standalone server's Authenticator, AuthKit's, passes the conformance
// kit a host runs, in a merchant's scope (its AuthKit group, whose id is the
// merchant's under the Authenticator's root authority): the owner's session
// holding the merchant: permissions there and nowhere else, a viewer holding
// only the reads, a user holding none, a stale sign-in, the merchant's owner
// API key, and the owner signed out last.
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

	root, err := ak.Authenticator().(auth.Root).RootScope(ctx)
	require.NoError(t, err)
	scope, err := cp.MerchantScope(ctx, mid)
	require.NoError(t, err)
	require.Equal(t, auth.Scope{Authority: root.Authority, ID: mid.String()}, scope, "a merchant's scope is its own group, where the gate asks")
	reads, ok := controlplane.MerchantRole("viewer")
	require.True(t, ok)
	authtest.GrantRole(t, ak, iam.GroupByID(scope.ID), iam.UserSubject(viewer.ID), reads)

	request := func(token string) func() *http.Request {
		return bearerRequest(token)
	}
	session := authtest.SignIn(t, ak, owner).AccessToken
	openrailstest.CheckAuth(t, openrails.Routes{Auth: ak.Authenticator()}, helpersauthtest.Cases{
		Scope:       scope,
		Permissions: []string{string(billing.MerchantCustomerRead), string(billing.MerchantCustomerManage)},
		Staff:       request(session),
		User:        request(authtest.SignIn(t, ak, stranger).AccessToken),
		Holders:     map[string]func() *http.Request{string(billing.MerchantCustomerRead): request(authtest.SignIn(t, ak, viewer).AccessToken)},
		Stale:       request(authtest.StaleSession(t, ak, authtest.SignIn(t, ak, owner).AccessToken)),
		Application: request(key.Secret),
		Refused:     map[string]func() *http.Request{"forged": request(session + "x"), "signed out": request(signedOut), "a forged key": request(key.Secret + "x")},
		Revoke: func() {
			_, err := ak.RevokeAccountSessions(ctx, iam.UserIdentity(owner.ID), owner.ID)
			require.NoError(t, err)
		},
	})
}
