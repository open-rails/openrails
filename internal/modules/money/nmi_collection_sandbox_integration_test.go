//go:build integration

package money_test

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/stretchr/testify/require"
)

// TestChargeOutstanding_NMISandbox_CollectsRealCharge is the opt-in proof for
// openrails #619: it drives OpenRails' REAL invoice-collection path
// (MoneyService.ChargeOutstanding -> ScopedCharger -> NMICollectionAdapter ->
// nmi.NMIClient.RunSale) against the LIVE NMI sandbox gateway with the standard
// NMI test card, and asserts that OpenRails — the source of truth — records the
// settled invoice/payment with the REAL provider transaction id.
//
// Opt-in contract (#619): the ONLY gate is NMI_SANDBOX_SECURITY_KEY.
//   - SKIP cleanly when it is unset (default CI, no creds, stays green).
//   - FAIL LOUD on the real provider error when it IS set but the path is broken.
//   - Never silently skip when configured: unlike TestLiveNMIInvoiceCollection-
//     AgainstSandbox (double-gated behind OPENRAILS_LIVE_RAIL_TESTS=1), this test
//     runs the moment the sandbox key is present, so a configured CI cannot pass
//     while the collection path is quietly broken.
//
// Provider-limitation honesty (do NOT shim): NMI's Direct Post `sale` only moves
// money against a stored Customer Vault. The gateway has NO concept of an
// OpenRails catalog product/price/invoice — it stores only the charge (amount,
// vault, order_description/orderid) and returns an approved transaction id. All
// rich catalog semantics (product, price, invoice line items, arrears ledger,
// settlement state) live entirely in the OpenRails DB. This test asserts exactly
// that split: NMI approves a charge and hands back a transaction id; OpenRails
// owns and records everything else.
func TestChargeOutstanding_NMISandbox_CollectsRealCharge(t *testing.T) {
	securityKey := strings.TrimSpace(os.Getenv("NMI_SANDBOX_SECURITY_KEY"))
	if securityKey == "" {
		t.Skip("NMI_SANDBOX_SECURITY_KEY not set; skipping opt-in NMI sandbox collection proof (#619)")
	}

	svc, dbi, pool, payer, _, ctx := moneyInEnvWithDB(t)
	cleanupInvoiceRows(t, pool, ctx, payer)

	// The REAL NMI client, pointed at the REAL sandbox Direct Post endpoint (no URL
	// override). The nmi.NewClient testMode=true mirrors the TEST_MODE=sandbox
	// configuration.
	client, err := nmi.NewClient(string(models.RailNMI), &config.NMIProviderSettings{
		SecurityKey:   securityKey,
		WebhookSecret: strings.TrimSpace(os.Getenv("NMI_WEBHOOK_SIGNING_SECRET")),
	}, true)
	require.NoError(t, err)
	require.Equal(t, nmi.DefaultDirectPostURL, client.DirectPostURL, "must hit the real NMI gateway, not a stub")

	// ARRANGE (scaffolding, not the path under test): mint a real NMI sandbox
	// Customer Vault from the standard test card. In production the vault id arrives
	// via Collect.js tokenization; here the shared helper creates it server-to-
	// server with the test PAN so the collection path has a real, chargeable handle.
	vaultID := createNMISandboxVault(t, client)
	t.Logf("NMI sandbox customer vault created: %s", vaultID)
	t.Cleanup(func() {
		if derr := client.DeleteCustomerVault(nmi.DeleteCustomerVaultData{CustomerVaultID: vaultID}); derr != nil {
			t.Logf("sandbox vault cleanup (non-fatal): %v", derr)
		}
	})

	// The payment method OpenRails will collect against: rail=mobius (NMI-backed),
	// rail_customer_ref = the sandbox vault id.
	pm := seedPaymentMethodWithVault(t, pool, ctx, payer, string(models.RailNMI), vaultID)

	// Build the owed OpenRails invoice from real arrears state. owed is in ledger
	// internal units (1e4 per cent). Randomize within the simulator's approving
	// band ($1.10–$1.89) so repeated runs do not trip NMI duplicate detection.
	owedInternal := (int64(110) + time.Now().UnixNano()%80) * 10_000
	_, err = svc.UpsertAccountSettings(ctx, payer, money.DefaultCurrency, money.AccountSettingsInput{
		BillingMode: strptr(money.BillingModeArrears), AutoTopupPaymentMethod: &pm,
	})
	require.NoError(t, err)
	require.NoError(t, svc.SetCreditLimit(ctx, payer, money.DefaultCurrency, 2_000_000))
	_, err = svc.AccrueOwed(ctx, payer, money.DefaultCurrency, "usage", "nmi-sandbox-619-"+time.Now().UTC().Format("150405.000000000"), owedInternal)
	require.NoError(t, err)
	inv, err := svc.FinalizeInvoice(ctx, payer, money.DefaultCurrency, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, "open", inv.Status)
	require.Equal(t, owedInternal, inv.AmountDue)

	charger := money.NewScopedCharger(dbi, money.NewNMICollectionAdapters(map[string]*nmi.NMIClient{
		string(models.RailNMI): client,
	}))

	// ACT: collect the open invoice through the real path. A non-nil error here is
	// a genuine provider/transport failure — fail loud, never silently skip.
	n, err := svc.ChargeOutstanding(ctx, charger, 0)
	require.NoError(t, err, "real NMI sandbox collection failed (configured but broken: fail loud, do not skip)")
	if n != 1 {
		var failureCode, failureMessage string
		_ = pool.QueryRow(ctx, `
			SELECT COALESCE(failure_code, ''), COALESCE(failure_message, '')
			FROM openrails.invoice_payments
			WHERE invoice_id = $1 AND status = 'failed'
			ORDER BY created_at DESC
			LIMIT 1
		`, inv.ID).Scan(&failureCode, &failureMessage)
		t.Fatalf("expected exactly one settled NMI invoice collection, got %d; failure_code=%q failure_message=%q", n, failureCode, failureMessage)
	}

	// ASSERT: OpenRails is the source of truth — the invoice is fully paid.
	paid, err := svc.GetInvoiceByID(ctx, payer, inv.ID)
	require.NoError(t, err)
	require.Equal(t, "paid", paid.Status)
	require.Equal(t, owedInternal, paid.AmountPaid)
	require.Equal(t, int64(0), paid.AmountDue)

	// ASSERT: the settled invoice_payment carries the rail + the REAL NMI
	// transaction id returned by the sandbox (proof the money actually moved).
	var rail, railPaymentID string
	var settledCount int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*), COALESCE(MAX(rail), ''), COALESCE(MAX(rail_payment_id), '')
		FROM openrails.invoice_payments
		WHERE invoice_id = $1 AND status = 'settled'
	`, inv.ID).Scan(&settledCount, &rail, &railPaymentID))
	require.Equal(t, 1, settledCount)
	require.Equal(t, string(models.RailNMI), rail)
	require.NotEmpty(t, railPaymentID, "OpenRails must record the real NMI sandbox transaction id")

	// #297: the collection ran as a reference-less MIT on a legacy (unanchored)
	// instrument, so the success must back-fill the unscheduled sequence anchor
	// with the real sandbox transaction id.
	var storedRef string
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT stored_credential_unscheduled_ref FROM openrails.payment_methods WHERE id = $1", pm).Scan(&storedRef))
	require.Equal(t, railPaymentID, storedRef, "successful collection must anchor the stored-credential sequence (#297)")

	t.Logf("APPROVED: OpenRails invoice %s settled via NMI sandbox; real transaction id = %s (stored-credential anchor persisted)", inv.ID, railPaymentID)
}
