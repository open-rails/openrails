//go:build e2e && integration

package ci_test

import (
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
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
	key := make([]byte, 32)
	_, err := rand.Read(key)
	require.NoError(t, err)
	srv := f.newServer(t, func(cfg *server.Config, _ *server.Deps) {
		cfg.Engine.ProviderWriteMode = openrails.ProviderWritesFull
		cfg.Engine.SecretBackend = openrails.SecretBackendDB
		cfg.Engine.Encryption = &openrails.EncryptionConfig{MasterKey: base64.StdEncoding.EncodeToString(key)}
		cfg.Engine.ProviderSandbox = &openrails.ProviderSandboxConfig{NMIGatewayURL: gateway.URL()}
	})
	engine := srv.Client()
	publish := func(account string) openrails.RequestOption {
		m, err := srv.ProvisionMerchant(ctx, billing.ProvisionMerchantParams{Slug: "seller-" + uuid.NewString()[:8]})
		require.NoError(t, err)
		at := openrails.ForMerchantID(m.MerchantID)
		_, err = engine.CreatePSP(ctx, billing.CreatePSPParams{OperationID: uuid.New(), Key: "nmi", Rail: billing.RailNMI, AccountID: account,
			Settings:    map[string]any{"tokenization_key": "e2e-tokenization"},
			Credentials: map[string]string{"security_key": "e2e-one-gateway-key", "webhook_signing_secret": "e2e-nmi-webhook"}}, at)
		require.NoError(t, err)
		return at
	}
	armed := func(at openrails.RequestOption) []string {
		cfg, err := engine.GetPublicConfig(ctx, at)
		require.NoError(t, err)
		require.NotNil(t, cfg.Payment)
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
