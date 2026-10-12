//go:build e2e && integration

package subscriptions_test

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/hosttools"
	"github.com/open-rails/openrails/internal/merchantbootstrap"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/nmimock"
)

// migrateCustody applies a custodian's vault-export manifest as an operator
// does, from a one-off process reading this world's merchant manifest.
func (w *world) migrateCustody(export hosttools.VaultExport) hosttools.CustodyMigrationResult {
	w.t.Helper()
	name := w.displayName
	if name == "" {
		name = w.slug
	}
	manifest, err := yaml.Marshal(merchantbootstrap.BillingConfig{Version: 1, Merchants: map[string]openrails.MerchantDeclaration{
		w.slug: {DisplayName: name, PSPs: w.psps, Custodians: w.custodians, Settings: w.settings}}})
	require.NoError(w.t, err)
	path := filepath.Join(w.t.TempDir(), "merchants.yaml")
	require.NoError(w.t, os.WriteFile(path, manifest, 0o600))
	cfg := &config.Config{TestMode: config.CredentialPostureSandbox, ProviderWriteMode: config.ProviderWriteModeFull,
		DB: &config.DBConfig{URL: w.dsn}, Database: config.DatabaseConfig{Schema: w.schema}}
	res, err := hosttools.MigrateCustody(w.t.Context(), hosttools.CustodyMigrationOptions{Config: cfg, PGXPool: w.pool,
		MerchantID: w.client[embedded].MerchantID(), Export: export, Apply: true, MerchantManifestPath: path})
	require.NoError(w.t, err)
	return res
}

// A custody remap moves a card from its PSP's vault to a custodian that
// another gateway account charges through. The card's agreements stay with
// the account whose transactions they cite: a collection through the new
// account is refused before any provider request, and the card cannot be
// named for collection there. A card the customer pays with on that account
// collects under the agreement its own charge started.
func TestCustodyRemapNeverCitesAnotherAccount(t *testing.T) {
	t.Parallel()
	const number, replacement = "4111111111111111", "4242424242424242"
	bt, btURL := newBTFake(t)
	w := prepareWorld(t, 12)
	w.custodians = map[string]openrails.CustodianConfig{"bt": {Kind: "basis_theory", AccountID: "bt-e2e",
		Settings: map[string]any{"public_api_key": "key_public_e2e"}, Secrets: map[string]string{"api_key": "key_private_e2e"}}}
	w.declare = func(psps map[string]openrails.PSPConfig) {
		psps["nmi-bt"] = openrails.NMIPSP{AccountID: "e2e-nmi-bt", SecurityKey: "e2e-nmi-bt-key", WebhookSigningSecret: "nmi_bt_webhook_e2e", Custodian: "bt"}.PSPConfig()
	}
	// The merchant sells cards through its custodian's PSP.
	routing := []billing.CheckoutRoutingRule{{Prefer: []string{"nmi-bt"}}}
	w.settings = billing.MerchantSettings{CheckoutRouting: &routing}
	w.booted = func(e *openrails.Engine) {
		rt := engine.Graph(e).Runtime
		rt.CheckoutService.CustodianSaleService.BTBaseURLOverride = btURL
		rt.CollectionResolver.(*money.MerchantCollectionAdapterBuilder).Endpoints.BTBaseURL = btURL
	}
	w.start()
	// Behind the proxy, the gateway takes the card number.
	keyed := w.nmi.AddVault(nmimock.Card{Brand: "visa", Last4: "1111"})
	bt.gateway = func(form url.Values, number string) string {
		return w.nmi.AddSale(nmimock.Sale{Vault: keyed, OrderID: form.Get("orderid"), Amount: form.Get("amount"),
			InitiatedBy: form.Get("initiated_by"), Indicator: form.Get("stored_credential_indicator"), Initial: form.Get("initial_transaction_id")}).TransactionID
	}
	proxied := func(number string) [][]string {
		var out [][]string
		for _, s := range bt.salesOf(number) {
			out = append(out, []string{s.SecurityKey, s.InitiatedBy, s.Indicator, s.InitialTxnID})
		}
		return out
	}

	// The card is saved in the first account's vault and collects there.
	c := w.newCustomer()
	_, err := w.client[embedded].UpdateCustomer(t.Context(), c.cid(), billing.UpdateCustomerParams{CreditLimits: []billing.CreditLimit{{Currency: "USD", Amount: 50_000_000}}})
	require.NoError(t, err)
	method := c.saveCard("nmi", visa)
	c.setDefault(method)
	_, anchor := w.agreements(method)
	require.NotEmpty(t, anchor)
	first := w.arrearsInvoice(c)
	w.refreshProviders()
	w.collectInvoices()
	require.Equal(t, billing.OrderComplete, w.invoice(first).Status)
	attempts := w.nmi.Attempts()
	require.Equal(t, []string{"e2e-nmi-key", "merchant", "used", anchor}, sentFields(attempts[len(attempts)-1]))

	// The custodian ingests that vault; the export re-points the card at the
	// account that charges through the custodian.
	bt.mu.Lock()
	bt.tokens["tok_remapped"] = number
	bt.mu.Unlock()
	res := w.migrateCustody(hosttools.VaultExport{ExportedAt: w.clock.Now(), SourceRail: "nmi", SourcePSPID: w.psp["nmi"].UUID(), Custodian: "bt",
		PSP:    hosttools.CustodyPSPRef{Rail: "nmi", Environment: "test", AccountID: "e2e-nmi-bt"},
		Tokens: []hosttools.ImportedToken{{SourceRailCustomerRef: w.vaultOf(method), Token: "tok_remapped", Fingerprint: "fp_" + number}}})
	require.Equal(t, map[hosttools.CustodyOutcome]int{hosttools.CustodyRemapped: 1}, res.Counts, "%+v", res.Rows)
	require.Equal(t, "basis_theory", w.methodRow(method, "custodian"))
	for _, m := range w.mandates(embedded, c.cid()) {
		require.Equal(t, w.psp["nmi"], m.PSPID, "the card's agreements stay with the account that made them")
	}

	calls := len(w.nmi.Calls())
	second := w.arrearsInvoice(c)
	w.refreshProviders()
	w.collectInvoices()
	require.Equal(t, agreementRequired, w.invoice(second).Payment.LastPaymentError.Reason, "the new account has no agreement to cite")
	require.Len(t, w.nmi.Calls(), calls, "refused before any gateway request")
	require.Empty(t, proxied(number), "nothing went through the custodian's proxy")
	require.Equal(t, []string{"card_on_file/active", "unscheduled/active"}, w.mandateStates(method))
	status, body := c.call(http.MethodPut, "/default-payment-methods/USD", "", map[string]any{"payment_method_id": method})
	require.Equal(t, http.StatusBadRequest, status, "no agreement on the new account to collect under: %v", body)

	// The customer pays with a card on the new account: present, the sale
	// starts its agreement there.
	c.mustCheckout(embedded, order{price: w.permanent("content:remapped").ID, rail: "nmi-bt", token: bt.collect(replacement)})
	stored := bt.salesOf(replacement)
	require.Equal(t, [][]string{{"e2e-nmi-bt-key", "customer", "stored", ""}}, proxied(replacement))
	var card string
	require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT 'pm_' || id FROM billing.payment_methods WHERE fingerprint = $1`), "fp_"+replacement).Scan(&card))

	// Named the default, it collects the stopped invoice on the new account
	// under its own agreement; the remapped card's ends.
	c.setDefault(card)
	require.Equal(t, []string{"card_on_file/active", "unscheduled/ended/replaced"}, w.mandateStates(method))
	require.Equal(t, []string{"card_on_file/active", "unscheduled/active"}, w.mandateStates(card))
	w.refreshProviders()
	w.collectInvoices()
	require.Equal(t, billing.OrderComplete, w.invoice(second).Status)
	require.Equal(t, [][]string{{"e2e-nmi-bt-key", "customer", "stored", ""}, {"e2e-nmi-bt-key", "merchant", "used", stored[0].TransactionID}}, proxied(replacement))
	require.Empty(t, proxied(number))
	require.Len(t, w.nmi.Attempts(), len(attempts), "nothing more on the old account")
	require.Empty(t, w.nmi.Unexpected())
}
