//go:build e2e && integration

package ci_test

import (
	"net/http"
	"testing"

	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/server/internal/controlplane"
	"github.com/stretchr/testify/require"
)

type consoleDirectoryPage struct {
	Data []struct {
		ID   string `json:"id"`
		Slug string `json:"slug"`
	} `json:"data"`
	Root bool `json:"root"`
}

func TestServerConsoleMerchantsListFollowsRoles(t *testing.T) {
	f := newFixture(t)
	srv := f.newServer(t, standaloneConsole("http://127.0.0.1/"+f.schema, nil))
	handler, err := standaloneHandler(srv)
	require.NoError(t, err)
	a, b := provision(t, srv, uniqueName("directory-a")), provision(t, srv, uniqueName("directory-b"))
	staff, operator := newAccount(t, srv), newAccount(t, srv)
	authtest.GrantRole(t, srv.AuthKit(), iam.GroupByID(a.String()), iam.UserSubject(staff.ID), controlplane.MerchantSupport)
	authtest.EnrollTOTP(t, srv.AuthKit(), operator)
	authtest.GrantRole(t, srv.AuthKit(), iam.RootGroup(), iam.UserSubject(operator.ID), controlplane.Roles.Root.Owner)
	staffSession := authtest.SignIn(t, srv.AuthKit(), staff).AccessToken
	operatorSession := authtest.SignIn(t, srv.AuthKit(), operator).AccessToken
	list := func(token, path string) consoleDirectoryPage {
		t.Helper()
		response := call(t, handler, token, http.MethodGet, path, nil)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		require.Equal(t, "no-store", response.Header().Get("Cache-Control"))
		return decode[consoleDirectoryPage](t, response)
	}
	own := list(staffSession, "/admin/merchants?merchant="+b.String())
	require.False(t, own.Root)
	require.Len(t, own.Data, 1)
	require.Equal(t, a.String(), own.Data[0].ID, "a query cannot expand a member's directory")
	all := list(operatorSession, "/admin/merchants")
	require.True(t, all.Root)
	require.Len(t, all.Data, 2)
	require.ElementsMatch(t, []string{a.String(), b.String()}, []string{all.Data[0].ID, all.Data[1].ID})
	key := merchantKey(t, srv, a, "support")
	boundKey := list(key.Secret, "/admin/merchants")
	require.False(t, boundKey.Root)
	require.Len(t, boundKey.Data, 1)
	require.Equal(t, a.String(), boundKey.Data[0].ID)
	_, status, body := f.exchangeFor(t, handler, staffSession, b)
	require.Equal(t, http.StatusBadRequest, status, string(body))
	require.Contains(t, string(body), "invalid_target")
	require.Equal(t, http.StatusUnauthorized, get(handler, "/admin/merchants").Code)
}

func TestServerConsoleFirstOperatorFromManifest(t *testing.T) {
	f := newFixture(t)
	srv := f.newServer(t, standaloneConsole("http://127.0.0.1/"+f.schema, nil))
	name := uniqueName("first-operator")
	email := name + "@e2e.test"
	manifest, err := authkit.ParseBootstrapManifestYAML([]byte("users:\n  - email: " + email + "\n    username: " + name + "\n    email_verified: true\n    password:\n      plaintext: " + authtest.Password + "\n    root_role: root:owner\n"))
	require.NoError(t, err)
	result, err := srv.AuthKit().ApplyBootstrapManifest(t.Context(), manifest, iam.BootstrapOptions{StartupOnly: true})
	require.NoError(t, err)
	require.Equal(t, 1, result.UsersCreated)
	require.Equal(t, 1, result.RootRoleAssignments)
	replay, err := srv.AuthKit().ApplyBootstrapManifest(t.Context(), manifest, iam.BootstrapOptions{StartupOnly: true})
	require.NoError(t, err)
	require.True(t, replay.AlreadyApplied)
	merchant := provision(t, srv, uniqueName("first-merchant"))
	token := authtest.SignIn(t, srv.AuthKit(), authtest.User{Email: email, Password: authtest.Password}).AccessToken
	handler, err := standaloneHandler(srv)
	require.NoError(t, err)
	response := call(t, handler, token, http.MethodGet, "/admin/merchants", nil)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	directory := decode[consoleDirectoryPage](t, response)
	require.True(t, directory.Root)
	require.Len(t, directory.Data, 1)
	require.Equal(t, merchant.String(), directory.Data[0].ID)
	bound := f.exchanged(t, handler, token, merchant)
	response = bound.call(t, handler, http.MethodGet, "/v1/admin/access", nil)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.Equal(t, billing.AccessUpdate, decode[billing.AdminAccess](t, response).Admin)
}
