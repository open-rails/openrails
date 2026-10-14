//go:build e2e && integration

package ci_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/authkit"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/vaulttest"
	"github.com/open-rails/openrails/openrailstest/nmimock"
	"github.com/open-rails/openrails/server"
)

// A hosted merchant publishing an NMI security key another merchant's live
// PSP already holds, under its own account id, gets a disarmed PSP and a
// finding: NMI cannot say which account a key belongs to, so the key does.
// Only a keyed fingerprint of it is stored.
func TestSecondPublicationOfAGatewayAccountStaysDisarmed(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()
	gateway := nmimock.New(nmimock.Options{})
	t.Cleanup(gateway.Close)
	vault := vaulttest.New(t)
	srv := f.newServer(t, func(cfg *server.Config, _ *server.Deps) {
		cfg.Engine.ProviderWriteMode = openrails.ProviderWritesFull
		cfg.Engine.Vault = vault.Config()
		cfg.Engine.ProviderSandbox = &openrails.ProviderSandboxConfig{NMIGatewayURL: gateway.URL()}
		cfg.Auth.Resource = authkit.ResourceConfig{ID: resourceID, PublicURL: rsOrigin}
	})
	// publish arms a merchant's NMI PSP and answers one of its customers'
	// tokens, which reads GET /v1/me/config.
	publish := func(account string) string {
		slug := "seller-" + uuid.NewString()[:8]
		m, err := srv.ProvisionMerchant(ctx, billing.ProvisionMerchantParams{Slug: slug})
		require.NoError(t, err)
		_, err = handle(t, srv, m.MerchantID).CreatePSP(ctx, billing.CreatePSPParams{Key: "nmi", Rail: billing.RailNMI, Environment: "sandbox", PSPAccount: billing.PSPAccount{GatewayID: account},
			TokenizationKey: "e2e-tokenization",
			Credentials:     map[string]string{"security_key": "e2e-one-gateway-key", "webhook_signing_secret": "e2e-nmi-webhook"}})
		require.NoError(t, err)
		login := newIssuerKey(t, "https://"+slug+".e2e.test")
		trust(t, srv, m.MerchantID, login.app(t, "viewer", nil))
		return customerOf(t, login, uuid.NewString())
	}
	armed := func(customer string) []string {
		w := serve(srv.Handler(), rsRequest{path: "/v1/me/config", authorization: customer})
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var cfg billing.CustomerConfig
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &cfg))
		var keys []string
		for _, psp := range cfg.Payment.PSPs {
			keys = append(keys, psp.Key)
		}
		return keys
	}
	q := func(sql string) string { return strings.ReplaceAll(sql, "billing.", f.schema+".") }

	first := publish("e2e-gateway-first")
	second := publish("e2e-gateway-relabeled")
	require.Equal(t, []string{"nmi"}, armed(first), "the first publication stays armed")
	require.Empty(t, armed(second), "the same account under another id is disarmed")
	var findings int
	require.NoError(t, f.pool.QueryRow(ctx, q(`SELECT count(*) FROM billing.reconciliation_findings fi
		JOIN billing.psps p ON p.merchant_id = fi.merchant_id AND p.id::text = fi.subject_key
		WHERE fi.finding_type = 'consistency.duplicate_gateway_account' AND fi.status = 'requires_review' AND p.account_id = 'e2e-gateway-relabeled'`)).Scan(&findings))
	require.Equal(t, 1, findings)
	var stored string
	require.NoError(t, f.pool.QueryRow(ctx, q(`SELECT credential_fingerprint FROM billing.psps WHERE account_id = 'e2e-gateway-first'`)).Scan(&stored))
	require.Regexp(t, `^[0-9a-f]{64}$`, stored)
	require.NotContains(t, stored, "gateway-key")
}
