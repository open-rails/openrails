//go:build e2e && integration

package ci_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/server"
	"github.com/stretchr/testify/require"
)

// A merchant app supplies the console's public PKCE client with the staff
// member's current claims. The server binds them to only the app's merchant.
func TestServerConsoleSignsInWithMerchantApp(t *testing.T) {
	f := newFixture(t)
	roles := authkit.NewRoles()
	merchant := roles.Persona("merchant")
	merchant.Permission("customer", "read")
	admin := roles.Root.Role("console-staff", merchant.All())
	const console, callback = "billing-console", "http://127.0.0.1/admin/callback"
	as := authtest.NewAuthorizationServer(t,
		authtest.WithDeps(func(deps *authkit.Deps) { deps.Postgres = f.pool }),
		authtest.WithConfig(func(cfg *authkit.Config) {
			cfg.Roles = roles
			cfg.AuthorizationServer = authkit.AuthorizationServerConfig{
				Resources: []authkit.ResourceServerConfig{{ID: resourceID, Scopes: []string{billing.ScopeMerchant}, Permissions: []string{"merchant:*"}}},
				Clients:   []authkit.OAuthClientConfig{{ID: console, RedirectURIs: []string{callback}, Resources: []string{resourceID}, GrantTypes: []authkit.OAuthGrantType{authkit.GrantAuthorizationCode, authkit.GrantRefreshToken}}},
			}
		}))
	response, err := as.HTTPClient().Get(as.URL + iam.JWKSPath)
	require.NoError(t, err)
	var set struct {
		Keys []iam.JWK `json:"keys"`
	}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&set))
	require.NoError(t, response.Body.Close())
	var pinned []iam.RemoteApplicationKey
	for _, key := range set.Keys {
		pinned = append(pinned, iam.RemoteApplicationKey{KID: key.Kid, JWK: &key})
	}
	shop := uniqueName("console-app")
	federated := func(resource string) func(*server.Config, *server.Deps) {
		return func(cfg *server.Config, deps *server.Deps) {
			cfg.LocalSignIn = false
			cfg.Auth.Resource = authkit.ResourceConfig{ID: resource}
			if resource != "" {
				cfg.Auth.Resource.PublicURL = rsOrigin
			}
			cfg.AdminConsole = &server.AdminConsole{Issuers: map[string]server.ConsoleIssuer{shop: {URL: as.URL, ClientID: console, Name: "Merchant app"}}}
			deps.ConsoleAssets = consoleBuild("federated")
		}
	}
	srv := f.newServer(t, federated(resourceID))
	// The CLI constructs the console before it provisions the boot manifest.
	mid := provision(t, srv, shop)
	other := provision(t, srv, uniqueName("console-other"))
	owner, _ := server.MerchantRole("owner")
	trust(t, srv, mid, iam.RemoteApplication{Issuer: as.URL, Mode: iam.RemoteApplicationModeStatic, PublicKeys: pinned, Enabled: true, Role: owner})
	handler, err := standaloneHandler(srv)
	require.NoError(t, err)
	var boot struct {
		LocalSignIn bool `json:"local_sign_in"`
		Issuers     []struct {
			URL      string `json:"url"`
			ClientID string `json:"client_id"`
			Resource string `json:"resource"`
			Scope    string `json:"scope"`
			Key      string `json:"key"`
		} `json:"issuers"`
	}
	w := get(handler, "/admin/config.json")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &boot))
	require.False(t, boot.LocalSignIn)
	require.Len(t, boot.Issuers, 1)
	issuer := boot.Issuers[0]
	require.Equal(t, shop, issuer.Key)
	require.Equal(t, as.URL, issuer.URL)
	require.Equal(t, console, issuer.ClientID)
	require.Equal(t, resourceID, issuer.Resource)
	require.Equal(t, http.StatusNotFound, get(handler, "/"+f.schema+"/v1/capabilities").Code)
	user := authtest.NewUser(t, as.Client)
	authtest.GrantRole(t, as.Client, iam.RootGroup(), iam.UserSubject(user.ID), admin)
	flow := authtest.CodeFlow{ClientID: console, RedirectURI: callback, Resource: issuer.Resource, Scopes: strings.Fields(issuer.Scope)}
	tokens := as.Authorize(t, user, flow)
	require.NotNil(t, tokens.DPoP, "a public console proves its browser key")
	read := func(tokens authtest.OAuthTokens, path string) int {
		t.Helper()
		bound := &boundToken{AccessToken: tokens.AccessToken, key: tokens.DPoP}
		return bound.call(t, handler, http.MethodGet, path, nil).Code
	}
	require.Equal(t, http.StatusOK, read(tokens, "/v1/admin/customers"))
	bound := &boundToken{AccessToken: tokens.AccessToken, key: tokens.DPoP}
	listing := bound.call(t, handler, http.MethodGet, "/admin/merchants?merchant="+other.String(), nil)
	require.Equal(t, http.StatusOK, listing.Code, listing.Body.String())
	directory := decode[consoleDirectoryPage](t, listing)
	require.False(t, directory.Root)
	require.Len(t, directory.Data, 1)
	require.Equal(t, mid.String(), directory.Data[0].ID)
	stranger := billing.CustomerID(uuid.New())
	grantTo(t, handle(t, srv, other), stranger)
	require.Equal(t, http.StatusNotFound, read(tokens, "/v1/admin/customers/"+stranger.String()))
	refreshed := as.Refresh(t, console, "", tokens)
	require.Equal(t, http.StatusOK, read(refreshed, "/v1/admin/customers"))
	authtest.RevokeRole(t, as.Client, iam.RootGroup(), iam.UserSubject(user.ID), admin)
	status, body := as.Token(t, authtest.TokenRequest{ClientID: console, DPoP: refreshed.DPoP, Params: url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refreshed.RefreshToken}}})
	if status == http.StatusOK {
		var ended authtest.OAuthTokens
		require.NoError(t, json.Unmarshal(body, &ended))
		ended.DPoP = refreshed.DPoP
		require.Equal(t, http.StatusForbidden, read(ended, "/v1/admin/customers"), "the next token carries no removed role")
	} else {
		require.Contains(t, []int{http.StatusBadRequest, http.StatusForbidden}, status, string(body))
		require.Contains(t, string(body), "invalid_grant", "the issuer refused to renew the removed grant")
	}
	_, err = f.buildServer(t, federated(""))
	require.ErrorContains(t, err, "auth.resource.id")
}
