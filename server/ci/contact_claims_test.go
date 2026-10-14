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

	jwt "github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/open-rails/authkit"
	"github.com/open-rails/helpers/smtp/smtptest"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/vaultfake"
	"github.com/open-rails/openrails/openrailstest/nmimock"
	"github.com/open-rails/openrails/server"
	"github.com/open-rails/openrails/server/internal/bootstrap/serverboot"
	"github.com/open-rails/openrails/server/internal/operator"
)

// On a standalone server a customer's verified access token carries who it
// is: AuthKit records its contact claims in the merchant's directory, so a
// user who registers and buys at once has current contact data and a durable
// billing event, with no SCIM push or customer email delivery.
func TestStandaloneRecordsVerifiedContactClaims(t *testing.T) {
	f := newFixture(t)
	vault := vaultfake.New("e2e-root")
	t.Cleanup(vault.Close)
	gateway := nmimock.New(nmimock.Options{})
	t.Cleanup(gateway.Close)
	mail := smtptest.Start(t, smtptest.Options{Username: "apikey", Password: "SG.e2e-key"})
	host := newIssuerKey(t, "https://claims-"+strings.ReplaceAll(f.schema, "_", "-")+".e2e.test")
	shop := uniqueName("claims")
	srv := f.newServer(t, func(cfg *server.Config, deps *server.Deps) {
		cfg.Auth.Resource = authkit.ResourceConfig{ID: resourceID, PublicURL: rsOrigin}
		cfg.Engine.ProviderWriteMode = openrails.ProviderWritesFull
		cfg.Engine.Vault = &openrails.VaultConfig{Address: vault.URL(), Token: vault.Token}
		cfg.Engine.ProviderSandbox = &openrails.ProviderSandboxConfig{NMIGatewayURL: gateway.URL()}
		cfg.Engine.SMTP = &openrails.SMTPConfig{Host: mail.Host, Port: mail.Port, Username: "apikey", Password: "SG.e2e-key",
			From: openrails.EmailAddress{Name: "Claims", Address: "billing@claims.test"}}
	})
	manifest := filepath.Join(t.TempDir(), "merchants.yaml")
	require.NoError(t, os.WriteFile(manifest, []byte(fmt.Sprintf(`version: 1
merchants:
  %s:
    display_name: Claims
    psps:
      nmi:
        rail: nmi
        environment: sandbox
        gateway_id: test
        secrets: {security_key: test, webhook_signing_secret: test}
        tokenization_key: test
`, shop)), 0o600))
	graph, plane := operator.Of(srv)
	require.NoError(t, serverboot.ReconcileBootMerchantManifest(t.Context(), graph.Config, graph, plane, manifest, nil, ""))
	handler, err := standaloneHandler(srv)
	require.NoError(t, err)
	ctx := t.Context()
	m, err := graph.Runtime.Merchants.GetBySlug(ctx, shop)
	require.NoError(t, err)
	trust(t, srv, m.ID, host.app(t, "viewer", nil))
	members := handle(t, srv, m.ID)
	hours := 720
	product, err := members.CreateProduct(ctx, billing.CreateProductParams{Key: "members", DisplayName: "Members", Entitlements: []string{"content:members"}})
	require.NoError(t, err)
	price, err := members.CreatePrice(ctx, billing.CreatePriceParams{ProductID: product.ID, Key: "monthly", Currency: "USD", UnitAmount: 9_990_000, BillingIntervalHours: &hours, AccessDurationHours: &hours})
	require.NoError(t, err)

	// A brand-new user: the first request is the purchase.
	user := uuid.NewString()
	token := host.mint(t, func(c jwt.MapClaims) {
		c["sub"], c["scope"] = user, billing.ScopeSelf
		delete(c, "permissions")
		c["email"], c["email_verified"], c["preferred_username"], c["name"], c["updated_at"] = "new@claims.test", true, "newcomer", "New Comer", time.Now().Unix()
	})
	me := func(method, path string, body any) map[string]any {
		t.Helper()
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		q := rsRequest{method: method, path: "/v1/me" + path, authorization: "Bearer " + token, idempotencyKey: uuid.NewString()}
		if body != nil {
			q.body = string(raw)
		}
		w := serve(handler, q)
		require.Less(t, w.Code, 300, "%s %s: %s", method, path, w.Body.String())
		out := map[string]any{}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out), w.Body.String())
		return out
	}
	psps, err := members.ListPSPs(ctx, billing.PSPListParams{})
	require.NoError(t, err)
	card := me(http.MethodPost, "/payment-methods", map[string]any{"psp_id": psps.Items[0].ID, "token": gateway.Tokenize(nmimock.Card{Brand: "visa", Last4: "4242"}),
		"billing_details": map[string]any{"name": "New Comer", "address": map[string]any{"postal_code": "10001", "country": "US"}}})["id"]
	paid := me(http.MethodPost, "/orders", map[string]any{"lines": []any{map[string]any{"price_id": price.ID}}, "expected_total": "9990000", "payment": map[string]any{"payment_method_id": card}})
	require.Equal(t, "complete", paid["status"], "%v", paid)

	consumer, err := srv.NotificationEvents("contact-claims-test")
	require.NoError(t, err)
	require.NoError(t, consumer.Register(ctx))
	events, err := consumer.Pending(ctx, 100)
	require.NoError(t, err)
	require.NotEmpty(t, events, "purchase emits source facts, not customer email")
	require.Equal(t, user, events[0].CustomerID.String())

	customers, err := members.ListCustomers(ctx, billing.CustomerListParams{IDs: []billing.CustomerID{billing.CustomerID(uuid.MustParse(user))}})
	require.NoError(t, err)
	require.Equal(t, "new@claims.test", *customers.Items[0].Contact.Email)
	require.Equal(t, "New Comer", *customers.Items[0].Contact.Name)
	require.Equal(t, http.StatusNotFound, serve(handler, rsRequest{path: "/v1/app/scim/v2/Users", authorization: "Bearer " + token}).Code, "AuthKit is the server's SCIM directory")
}
