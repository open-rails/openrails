package intents

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/stretchr/testify/require"
)

func TestCollectionReceiptBindingPreservesLargeAcceptedIntegers(t *testing.T) {
	psp := uuid.New()
	payload := InvoiceCollectionPayload{InvoiceID: uuid.New(), CustomerID: uuid.New(), AttemptID: uuid.New(), PaymentMethodID: uuid.New(), Rail: "nmi", Currency: "USD", Amount: 9_007_199_254_740_992, Instrument: charge.FrozenInstrument{PSPID: psp, Custodian: "psp", RailCustomerRef: "vault"}}
	minor, err := moneyutil.NativeToRailMinor(payload.Currency, payload.Amount)
	require.NoError(t, err)
	payload.AmountMinor = minor
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	in := gen.OpenrailsRailIntent{ID: uuid.New(), MerchantID: uuid.New(), PspID: &psp, IntentType: "invoice_collection", Rail: "nmi", Payload: raw}
	binding, err := collectionBinding(in)
	require.NoError(t, err)
	receipt := CollectedReceipt{data: collectedReceipt{Version: 1, Family: "collected_payment", Binding: binding, NMI: &nmi.SaleEvidence{TransactionID: "paid", OrderReference: in.ID.String(), CustomerVaultID: "vault", Amount: minor, Currency: "USD", Approved: true}}}
	require.NoError(t, receipt.Validate(in))
	// Both exact native dues round to the same charge, but are different accepted
	// liabilities. A float64 payload digest would collapse these adjacent values.
	payload.Amount++
	changed := in
	changed.Payload, err = json.Marshal(payload)
	require.NoError(t, err)
	require.Error(t, receipt.Validate(changed))
}
