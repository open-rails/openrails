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

// A self-hosted OpenRails without local sign-in: its console is the trusted
// issuer's OAuth 2.0 public client, the token that flow mints is all the
// admin API needs, and the control plane serves no sign-in of its own.
func TestConsoleSignsInAtATrustedIssuer(t *testing.T) {
	f := newFixture(t)
	roles := authkit.NewRoles()
	merchant := roles.Persona("merchant")
	merchant.Permission("operations", "read")
	admin := roles.Root.Role("admin", merchant.All())
	const console, callback = "openrails-console", "http://127.0.0.1/admin/callback"
	as := authtest.NewAuthorizationServer(t,
		authtest.WithDeps(func(d *authkit.Deps) { d.Postgres = f.pool }),
		authtest.WithConfig(func(c *authkit.Config) {
			c.Roles = roles
			c.AuthorizationServer = authkit.AuthorizationServerConfig{
				Resources: []authkit.ResourceServerConfig{{ID: resourceID, Scopes: []string{billing.ScopeMerchant}, Permissions: []string{"merchant:*"}}},
				Clients: []authkit.OAuthClientConfig{{ID: console, RedirectURIs: []string{callback}, Resources: []string{resourceID},
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

	shop := uniqueName("console-idp")
	federated := func(issuer *server.ConsoleIssuer) func(*server.Config, *server.Deps) {
		return func(cfg *server.Config, deps *server.Deps) {
			cfg.LocalSignIn = false
			cfg.ResourceServer = &server.ResourceServerConfig{
				Identifier: resourceID, DPoPNonceKey: strings.Repeat("n", 32),
				TrustedIssuers: []server.TrustedIssuerConfig{{
					Name: "Example ID", Issuer: as.URL, Keys: pinned, Merchants: []string{shop}, Permissions: []string{"merchant:*"},
				}},
			}
			cfg.AdminConsole, cfg.ConsoleIssuer = &server.AdminConsole{}, issuer
			deps.Engine.ConsoleAssets = consoleBuild("federated")
		}
	}
	cp := f.newServer(t, federated(&server.ConsoleIssuer{URL: as.URL, ClientID: console}))
	provision(t, cp, shop)
	handler, err := standaloneHandler(cp)
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
	require.Equal(t, "Example ID", boot.Issuer.Name, "the trusted issuer's name")
	require.Equal(t, resourceID, boot.Issuer.Resource)
	require.Contains(t, strings.Fields(boot.Issuer.Scope), billing.ScopeMerchant)

	require.Equal(t, http.StatusNotFound, get(handler, "/"+f.schema+"/v1/capabilities").Code, "no local sign-in surface")
	require.Equal(t, http.StatusOK, get(handler, "/"+f.schema+iam.JWKSPath).Code, "the issuer's keys stay published")

	owner := authtest.NewUser(t, as.Client)
	authtest.GrantRole(t, as.Client, iam.RootGroup(), iam.UserSubject(owner.ID), admin)
	flow := authtest.CodeFlow{ClientID: console, RedirectURI: callback, Resource: boot.Issuer.Resource, Scopes: strings.Fields(boot.Issuer.Scope)}
	tokens := as.Authorize(t, owner, flow)
	w = dpopServe(t, userMerchants(cp), tokens, rsRequest{path: "/hosted/merchants"})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	list := merchantList(t, w)
	require.Len(t, list, 1)
	require.Equal(t, shop, list[0].Slug)
	require.Equal(t, "owner", list[0].Role)
	require.Equal(t, http.StatusNotFound, dpopServe(t, handler, tokens, rsRequest{path: "/v1/merchants"}).Code, "the console's merchants come from the host")

	renewed := as.Refresh(t, console, "", tokens)
	require.Equal(t, http.StatusOK, dpopServe(t, handler, renewed, rsRequest{path: "/v1/admin/findings"}).Code, "the console's refreshed token")

	stranger := as.Authorize(t, authtest.NewUser(t, as.Client), flow)
	w = dpopServe(t, userMerchants(cp), stranger, rsRequest{path: "/hosted/merchants"})
	require.Equal(t, http.StatusOK, w.Code)
	require.Empty(t, merchantList(t, w), "the console shows the empty state")

	_, err = f.buildServer(t, federated(nil))
	require.ErrorContains(t, err, "no sign-in method")
	_, err = f.buildServer(t, federated(&server.ConsoleIssuer{URL: "https://elsewhere.e2e.test", ClientID: console}))
	require.ErrorContains(t, err, "not one of resource_server.trusted_issuers")
}
