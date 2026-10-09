//go:build e2e && integration

package subscriptions_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
)

// armedKeys are the keys of the PSPs a merchant's public config lists armed.
func armedKeys(t *testing.T, cfg *billing.PublicConfig) []string {
	t.Helper()
	require.NotNil(t, cfg.Payment)
	var keys []string
	for _, psp := range cfg.Payment.PSPs {
		keys = append(keys, psp.Key)
	}
	return keys
}

// duplicateFindings counts the open duplicate-account findings on the PSP
// with key.
func duplicateFindings(t *testing.T, w *world, key string) int {
	t.Helper()
	var n int
	require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT count(*) FROM billing.reconciliation_findings f
		JOIN billing.psps p ON p.merchant_id = f.merchant_id AND p.id::text = f.subject_key
		WHERE f.finding_type = 'consistency.duplicate_gateway_account' AND f.status = 'requires_review' AND p.key = $1`), key).Scan(&n))
	return n
}

// One gateway account declared under two labels bills through only the first:
// the later declaration is disarmed, and a finding says why.
func TestSecondDeclarationOfAGatewayAccountStaysDisarmed(t *testing.T) {
	t.Parallel()
	w := prepareWorld(t, 12)
	w.declare = func(psps map[string]openrails.PSPConfig) {
		// The harness's "nmi" account again, under another account id.
		psps["nmi-again"] = openrails.NMIPSP{AccountID: "e2e-nmi-again", SecurityKey: "e2e-nmi-key", WebhookSigningSecret: "nmi_again_webhook_e2e"}.PSPConfig()
	}
	w.start()
	cfg := publicConfig(t, w.client[embedded])
	keys := armedKeys(t, &cfg)
	require.Contains(t, keys, "nmi", "the first declaration stays armed")
	require.NotContains(t, keys, "nmi-again", "the second declaration of the account is disarmed")
	require.Equal(t, 1, duplicateFindings(t, w, "nmi-again"))

	c := w.newCustomer()
	price := w.membership("content:gateway", 9_990_000)
	c.subscribe(embedded, "nmi", price.ID.String(), "content:gateway", c.saveCard("nmi", visa))
}
