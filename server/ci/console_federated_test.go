//go:build e2e && integration

package ci_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/server"
)

// A self-hosted OpenRails without local sign-in: its console is a trusted
// issuer's OAuth 2.0 client, the token that flow mints is all the admin API
// needs, and the server's AuthKit serves no sign-in of its own.
func TestConsoleSignsInAtATrustedIssuer(t *testing.T) {
	f := newFixture(t)
	roles := authkit.NewRoles()
	merchant := roles.Persona("merchant")
	merchant.Permission("billing", "read")
	admin := roles.Root.Role("admin", merchant.All())
	const console, callback = "openrails-console", "http://127.0.0.1/admin/callback"
	secret := strings.Repeat("c", 48)
	as := authtest.NewAuthorizationServer(t,
		authtest.WithDeps(func(d *authkit.Deps) { d.Postgres = f.pool }),
		authtest.WithConfig(func(c *authkit.Config) {
			c.Roles = roles
			c.AuthorizationServer = authkit.AuthorizationServerConfig{
				Resources: []authkit.ResourceServerConfig{{ID: resourceID, Scopes: []string{billing.ScopeMerchant}, Permissions: []string{"merchant:*"}}},
				Clients: []authkit.OAuthClientConfig{{ID: console, SecretSHA256: authtest.ClientSecretSHA256(secret), RedirectURIs: []string{callback}, Resources: []string{resourceID},
					GrantTypes: []authkit.OAuthGrantType{authkit.GrantAuthorizationCode, authkit.GrantRefreshToken}}},
			}
		}))
	res, err := as.HTTPClient().Get(as.URL + iam.JWKSPath)
	require.NoError(t, err)
	var set struct {
		Keys []iam.JWK `json:"keys"`
	}
	require.NoError(t, json.NewDecoder(res.Body).Decode(&set))
	require.NoError(t, res.Body.Close())
	var pinned []iam.RemoteApplicationKey
	for _, k := range set.Keys {
		pinned = append(pinned, iam.RemoteApplicationKey{KID: k.Kid, JWK: &k})
	}

	federated := func(resource string, issuer *server.ConsoleIssuer) func(*server.Config, *server.Deps) {
		return func(cfg *server.Config, deps *server.Deps) {
			cfg.LocalSignIn = false
			cfg.Auth.Resource = authkit.ResourceConfig{ID: resource}
			if resource != "" {
				cfg.Auth.Resource.PublicURL = rsOrigin
			}
			cfg.AdminConsole, cfg.ConsoleIssuer = &server.AdminConsole{}, issuer
			deps.Engine.ConsoleAssets = consoleBuild("federated")
		}
	}
	srv := f.newServer(t, federated(resourceID, &server.ConsoleIssuer{URL: as.URL, ClientID: console, Name: "Example ID"}))
	shop := uniqueName("console-idp")
	owner, _ := server.MerchantRole("owner")
	trust(t, srv, provision(t, srv, shop), iam.RemoteApplication{Issuer: as.URL, Mode: iam.RemoteApplicationModeStatic, PublicKeys: pinned, Enabled: true, Role: owner})
	handler, err := standaloneHandler(srv)
	require.NoError(t, err)

	var boot struct {
		AuthBaseURL string `json:"auth_base_url"`
		Issuer      *struct {
			URL      string `json:"url"`
			ClientID string `json:"client_id"`
			Name     string `json:"name"`
			Resource string `json:"resource"`
			Scope    string `json:"scope"`
		} `json:"issuer"`
	}
	w := get(handler, "/admin/config.json")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &boot))
	require.Empty(t, boot.AuthBaseURL, "no local sign-in to point at")
	require.NotNil(t, boot.Issuer)
	require.Equal(t, as.URL, boot.Issuer.URL)
	require.Equal(t, console, boot.Issuer.ClientID)
	require.Equal(t, "Example ID", boot.Issuer.Name)
	require.Equal(t, resourceID, boot.Issuer.Resource, "auth.resource.id")
	require.Contains(t, strings.Fields(boot.Issuer.Scope), billing.ScopeMerchant)

	require.Equal(t, http.StatusNotFound, get(handler, "/"+f.schema+"/v1/capabilities").Code, "no local sign-in surface")
	require.Equal(t, http.StatusOK, get(handler, "/"+f.schema+iam.JWKSPath).Code, "the issuer's keys stay published")

	user := authtest.NewUser(t, as.Client)
	authtest.GrantRole(t, as.Client, iam.RootGroup(), iam.UserSubject(user.ID), admin)
	flow := authtest.CodeFlow{ClientID: console, ClientSecret: secret, RedirectURI: callback, Resource: boot.Issuer.Resource, Scopes: strings.Fields(boot.Issuer.Scope)}
	tokens := as.Authorize(t, user, flow)
	findings := func(token, selector string) int {
		return serve(handler, rsRequest{path: "/v1/admin/findings", authorization: "Bearer " + token, selector: selector}).Code
	}
	require.Equal(t, http.StatusOK, findings(tokens.AccessToken, shop), "the console names the merchant its issuer is trusted by")
	require.Equal(t, http.StatusNotFound, serve(handler, rsRequest{path: "/v1/merchants", authorization: "Bearer " + tokens.AccessToken}).Code, "the console's merchants come from the host")
	renewed := as.Refresh(t, console, secret, tokens)
	require.Equal(t, http.StatusOK, findings(renewed.AccessToken, ""), "the console's refreshed token")
	stranger := as.Authorize(t, authtest.NewUser(t, as.Client), flow)
	require.Equal(t, http.StatusForbidden, findings(stranger.AccessToken, ""), "a user the issuer grants nothing")

	_, err = f.buildServer(t, federated(resourceID, nil))
	require.ErrorContains(t, err, "no sign-in method")
	_, err = f.buildServer(t, federated("", &server.ConsoleIssuer{URL: as.URL, ClientID: console}))
	require.ErrorContains(t, err, "auth.resource.id")
}
