//go:build e2e && integration

package ci_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bytes"
	"net/http/httptest"

	jwt "github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/open-rails/helpers/smtp/smtptest"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/open-rails/openrails/internal/staffperm"
	"github.com/open-rails/openrails/internal/vaultfake"
	"github.com/open-rails/openrails/openrailstest/nmimock"
	"github.com/open-rails/openrails/server"
	"github.com/open-rails/openrails/server/internal/bootstrap/serverboot"
	"github.com/open-rails/openrails/server/internal/operator"
)

// On a standalone server a customer's verified access token carries who it
// is: a user who registers and buys at once gets the receipt at the token's
// email with no SCIM push, through the built-in SMTP sender. Newest wins: an
// older push leaves the claims, a newer one replaces them.
func TestStandaloneRecordsVerifiedContactClaims(t *testing.T) {
	f := newFixture(t)
	vault := vaultfake.New("e2e-root")
	t.Cleanup(vault.Close)
	gateway := nmimock.New(nmimock.Options{})
	t.Cleanup(gateway.Close)
	mail := smtptest.Start(t, smtptest.Options{Username: "apikey", Password: "SG.e2e-key"})
	host := newIssuerKey(t, "https://claims-"+strings.ReplaceAll(f.schema, "_", "-")+".e2e.test")
	shop := uniqueName("claims")
	cp := f.newServer(t, func(cfg *server.Config, deps *server.Deps) {
		cfg.Engine.ProviderWriteMode = openrails.ProviderWritesFull
		cfg.Engine.Vault = &openrails.VaultConfig{Address: vault.URL(), Token: vault.Token}
		cfg.Engine.ProviderSandbox = &openrails.ProviderSandboxConfig{NMIGatewayURL: gateway.URL()}
		cfg.ResourceServer = &server.ResourceServerConfig{
			Identifier: resourceID, DPoPNonceKey: strings.Repeat("n", 32),
			TrustedIssuers: []server.TrustedIssuerConfig{{Name: "host", Issuer: host.iss, Keys: host.pinned(t), Merchants: []string{shop}, Permissions: []string{staffperm.BillingRead}}},
		}
		cfg.Engine.SMTP = &openrails.SMTPConfig{Host: mail.Host, Port: mail.Port, Username: "apikey", Password: "SG.e2e-key",
			From: openrails.EmailAddress{Name: "Claims", Address: "billing@claims.test"}}
	})
	scimToken := "declared-" + uuid.NewString()
	manifest := filepath.Join(t.TempDir(), "merchants.yaml")
	require.NoError(t, os.WriteFile(manifest, []byte(fmt.Sprintf(`version: 1
merchants:
  %s:
    display_name: Claims
    secrets:
      scim_token: %s
    psps:
      nmi:
        rail: nmi
        account_id: test
        secrets: {security_key: test, webhook_signing_secret: test}
        settings: {tokenization_key: test}
`, shop, scimToken)), 0o600))
	graph, plane := operator.Of(cp)
	require.NoError(t, serverboot.ReconcileBootMerchantManifest(t.Context(), graph.Config, graph, plane, manifest, nil, ""))
	handler, err := standaloneHandler(cp)
	require.NoError(t, err)
	ctx := t.Context()
	m, err := graph.Runtime.Merchants.GetBySlug(ctx, shop)
	require.NoError(t, err)
	at := openrails.ForMerchantID(m.ID)
	engine := cp.Client()
	hours := 720
	product, err := engine.CreateProduct(ctx, billing.CreateProductParams{Key: "members", DisplayName: "Members", Entitlements: []string{"content:members"}}, at)
	require.NoError(t, err)
	price, err := engine.CreatePrice(ctx, billing.CreatePriceParams{ProductID: product.ID, Key: "monthly", Currency: "USD", UnitAmount: 9_990_000, BillingIntervalHours: &hours, AccessDurationHours: &hours}, at)
	require.NoError(t, err)

	// A brand-new user: the first request is the purchase.
	browser := newBrowserKey(t)
	user := uuid.NewString()
	registered := time.Now().Add(-time.Minute).Truncate(time.Second)
	token := host.mint(t, func(c jwt.MapClaims) {
		c["sub"], c["scope"], c["cnf"] = user, billing.ScopeSelf, map[string]string{"jkt": browser.jkt}
		delete(c, "permissions")
		c["email"], c["email_verified"], c["preferred_username"], c["name"], c["updated_at"] = "new@claims.test", true, "newcomer", "New Comer", registered.Unix()
	})
	me := func(method, path string, body any) map[string]any {
		t.Helper()
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		q := rsRequest{method: method, path: "/v1/me" + path}
		if body != nil {
			q.body = string(raw)
		}
		w := staticDPoPServe(t, handler, browser, token, q)
		require.Less(t, w.Code, 300, "%s %s: %s", method, path, w.Body.String())
		out := map[string]any{}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out), w.Body.String())
		return out
	}
	psps, err := engine.ListPSPs(ctx, billing.PSPListParams{}, at)
	require.NoError(t, err)
	card := me(http.MethodPost, "/payment-methods", map[string]any{"psp_id": psps.Items[0].ID, "token": gateway.Tokenize(nmimock.Card{Brand: "visa", Last4: "4242"}),
		"billing_details": map[string]any{"name": "New Comer", "address": map[string]any{"postal_code": "10001", "country": "US"}}})["id"]
	session := me(http.MethodPost, "/checkout-sessions", map[string]any{"price_id": price.ID})["id"].(string)
	var option any
	for _, raw := range me(http.MethodGet, "/checkout-sessions/"+session, nil)["options"].([]any) {
		if o := raw.(map[string]any); o["rail"] == "nmi" {
			option = o["id"]
		}
	}
	paid := me(http.MethodPost, "/checkout-sessions/"+session+"/pay", map[string]any{"option_id": option, "payment_method_id": card})
	require.Equal(t, "succeeded", paid["status"], "%v", paid)

	sweep := riverjobs.NotificationEmailSweepWorker{DB: graph.Runtime.DB, Notifications: graph.Runtime.NotificationService}
	require.NoError(t, sweep.Work(ctx, &river.Job[riverjobs.NotificationEmailSweepArgs]{}))
	receipt := mail.Wait(t, 1, 10*time.Second)[0]
	require.Equal(t, []string{"new@claims.test"}, receipt.To, "the receipt goes to the token's email at once")
	require.Equal(t, "apikey", receipt.Username)
	require.Contains(t, receipt.Text+receipt.HTML, "9.99")

	// The directory pushes later. An older report leaves the claims; a newer one wins.
	scim := scimClient{t: t, handler: handler, base: "/v1/app/scim/v2", token: scimToken}
	older := scimUser(user, "newcomer", "older@claims.test", "New Comer")
	older["meta"] = map[string]any{"lastModified": registered.Add(-time.Hour).UTC().Format(time.RFC3339)}
	created := scim.must(http.MethodPost, "/Users", older, http.StatusCreated)
	require.Equal(t, "new@claims.test", created["emails"].([]any)[0].(map[string]any)["value"], "an older push leaves the newer claims")
	newer := scimUser(user, "newcomer", "newer@claims.test", "New Comer")
	newer["meta"] = map[string]any{"lastModified": registered.Add(time.Minute).UTC().Format(time.RFC3339)}
	replaced := scim.must(http.MethodPut, "/Users/"+user, newer, http.StatusOK)
	require.Equal(t, "newer@claims.test", replaced["emails"].([]any)[0].(map[string]any)["value"])

	// Claims older than the push no longer change it, and an unverified email never counts.
	me(http.MethodGet, "/entitlements", nil)
	customers, err := engine.ListCustomers(ctx, billing.CustomerListParams{IDs: []billing.CustomerID{billing.CustomerID(uuid.MustParse(user))}}, at)
	require.NoError(t, err)
	require.Equal(t, "newer@claims.test", *customers.Items[0].Contact.Email)
	token = host.mint(t, func(c jwt.MapClaims) {
		c["sub"], c["scope"], c["cnf"] = user, billing.ScopeSelf, map[string]string{"jkt": browser.jkt}
		delete(c, "permissions")
		c["email"], c["email_verified"], c["updated_at"] = "unverified@claims.test", false, time.Now().Add(time.Hour).Unix()
	})
	me(http.MethodGet, "/entitlements", nil)
	customers, err = engine.ListCustomers(ctx, billing.CustomerListParams{IDs: []billing.CustomerID{billing.CustomerID(uuid.MustParse(user))}}, at)
	require.NoError(t, err)
	require.Equal(t, "newer@claims.test", *customers.Items[0].Contact.Email, "an unverified email is no contact")

	// The issuer's application provisions too, holding no permission; a
	// person's token is refused, whatever it holds.
	machine := host.mint(t, func(c jwt.MapClaims) {
		c["sub"], c["client_id"], c["scope"] = "directory-sync", "directory-sync", billing.ScopeMerchant
		delete(c, "permissions")
	})
	scimClient{t: t, handler: handler, base: "/v1/app/scim/v2", token: machine}.must(http.MethodGet, "/Users/"+user, nil, http.StatusOK)
	person := scimClient{t: t, handler: handler, base: "/v1/app/scim/v2", token: host.mint(t, func(c jwt.MapClaims) {
		c["sub"], c["scope"] = user, billing.ScopeMerchant
	})}
	status, body := person.do(http.MethodGet, "/Users/"+user, nil)
	require.Equal(t, http.StatusForbidden, status, "a person provisions nothing: %v", body)
}

// scimClient drives the server's SCIM routes the way a directory does.
type scimClient struct {
	t       *testing.T
	handler http.Handler
	base    string
	token   string
}

func (c scimClient) do(method, path string, body any) (int, map[string]any) {
	c.t.Helper()
	var payload bytes.Buffer
	if body != nil {
		require.NoError(c.t, json.NewEncoder(&payload).Encode(body))
	}
	r := httptest.NewRequest(method, c.base+path, &payload)
	r.Header.Set("Content-Type", "application/scim+json")
	r.Header.Set("Authorization", "Bearer "+c.token)
	w := httptest.NewRecorder()
	c.handler.ServeHTTP(w, r)
	out := map[string]any{}
	if w.Body.Len() > 0 {
		require.NoError(c.t, json.Unmarshal(w.Body.Bytes(), &out), "%s %s: %s", method, path, w.Body.String())
	}
	return w.Code, out
}

func (c scimClient) must(method, path string, body any, want int) map[string]any {
	c.t.Helper()
	status, out := c.do(method, path, body)
	require.Equal(c.t, want, status, "%s %s: %v", method, path, out)
	return out
}

func scimUser(id, userName, email, name string) map[string]any {
	return map[string]any{
		"schemas": []string{"urn:ietf:params:scim:schemas:core:2.0:User"}, "externalId": id, "userName": userName, "active": true,
		"emails": []any{map[string]any{"value": email, "primary": true}}, "name": map[string]any{"formatted": name},
	}
}
