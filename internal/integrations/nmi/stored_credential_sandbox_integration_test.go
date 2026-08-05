//go:build integration

package nmi

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// TestLiveSandboxStoredCredentialCITThenMIT is the #297 live proof against the
// REAL NMI sandbox (opt-in via NMI_SANDBOX_SECURITY_KEY; skips otherwise):
//
//  1. the initial cardholder-initiated charge carries the portal's unscheduled
//     initial-CIT fields (initiated_by=customer +
//     stored_credential_indicator=stored) and the gateway APPROVES it,
//     returning the transactionid that becomes the sequence anchor;
//  2. a merchant-initiated charge replaying that anchor
//     (initiated_by=merchant + stored_credential_indicator=used +
//     initial_transaction_id=<CIT txn>) is APPROVED — the gateway accepts
//     NMI's own transaction id as the stored-credential reference, exactly as
//     the docs specify (no raw network NTID anywhere).
func TestLiveSandboxStoredCredentialCITThenMIT(t *testing.T) {
	key := strings.TrimSpace(os.Getenv("NMI_SANDBOX_SECURITY_KEY"))
	if key == "" {
		t.Skip("NMI_SANDBOX_SECURITY_KEY not set; skipping live sandbox stored-credential proof (#297)")
	}

	client, err := NewClient("live-sandbox", &config.NMIProviderSettings{SecurityKey: key}, true)
	require.NoError(t, err)
	require.Equal(t, DefaultDirectPostURL, client.DirectPostURL, "must hit the real gateway, not a stub")

	// Vault the standard test card (raw-PAN classic shortcut — production
	// vaulting takes a Collect.js token; see TestLiveSandboxClientSurface).
	form := url.Values{
		"security_key":   {key},
		"customer_vault": {"add_customer"},
		"ccnumber":       {"4111111111111111"},
		"ccexp":          {"1029"},
		"first_name":     {"StoredCred"},
		"last_name":      {"Probe297"},
	}
	resp, err := http.PostForm(client.DirectPostURL, form)
	require.NoError(t, err)
	body := make([]byte, 4096)
	n, _ := resp.Body.Read(body)
	_ = resp.Body.Close()
	vals, err := url.ParseQuery(string(body[:n]))
	require.NoError(t, err)
	vaultID := vals.Get("customer_vault_id")
	require.NotEmpty(t, vaultID, "sandbox vault create failed: %s", string(body[:n]))
	t.Cleanup(func() {
		_ = client.DeleteCustomerVault(context.Background(), DeleteCustomerVaultData{CustomerVaultID: vaultID})
	})

	// Randomize inside the simulator's approving band (>= $1.00) so repeated
	// runs dodge duplicate-transaction detection.
	amount := moneyutil.Cents(110 + time.Now().UnixNano()%80)

	// Leg 1 — initial CIT (unscheduled sequence anchor).
	cit, err := client.RunSale(context.Background(), SaleParams{
		CustomerVaultID:  vaultID,
		Amount:           amount,
		Currency:         "USD",
		OrderDescription: "openrails #297 CIT probe",
		StoredCredential: &StoredCredential{
			InitiatedBy: InitiatedByCustomer,
			Indicator:   IndicatorStored,
		},
	})
	require.NoError(t, err, "sandbox must approve the initial CIT carrying stored-credential fields")
	require.NotEmpty(t, cit.TransactionID)
	t.Logf("CIT approved: transactionid=%s (the stored-credential anchor)", cit.TransactionID)

	// Leg 2 — MIT replaying the anchor as initial_transaction_id.
	mit, err := client.RunSale(context.Background(), SaleParams{
		CustomerVaultID:  vaultID,
		Amount:           amount + 1,
		Currency:         "USD",
		OrderDescription: "openrails #297 MIT probe",
		StoredCredential: &StoredCredential{
			InitiatedBy:          InitiatedByMerchant,
			Indicator:            IndicatorUsed,
			InitialTransactionID: cit.TransactionID,
		},
	})
	require.NoError(t, err, "sandbox must approve the MIT replaying the CIT transactionid as initial_transaction_id")
	require.NotEmpty(t, mit.TransactionID)
	t.Logf("MIT approved: transactionid=%s (anchored on %s)", mit.TransactionID, cit.TransactionID)
}
