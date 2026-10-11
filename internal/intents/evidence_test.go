package intents

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// A receipt binds the exact accepted integer liability; a float digest would
// collapse adjacent values above 2^53.
func TestCollectionReceiptBindsExactAcceptedAmount(t *testing.T) {
	psp := uuid.New()
	payload := OrderCollectionPayload{Initiator: charge.InitiatorMerchant, OrderID: uuid.New(), CustomerID: uuid.New(), PaymentID: uuid.New(), PaymentMethodID: uuid.New(),
		Rail: "nmi", Currency: "USD", Amount: 9_007_199_254_740_992, Instrument: charge.FrozenInstrument{PSPID: psp, Custodian: "psp", RailCustomerRef: "vault"}}
	minor, err := moneyutil.NativeToRailMinor(payload.Currency, payload.Amount)
	require.NoError(t, err)
	payload.AmountMinor = minor
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	in := gen.BillingProviderIntent{ID: uuid.New(), MerchantID: uuid.New(), PspID: &psp, IntentType: TypeOrderCollection, Rail: "nmi", Payload: raw}
	binding, err := collectionBinding(in)
	require.NoError(t, err)
	receipt := CollectedReceipt{data: collectedReceipt{Version: 1, Family: "collected_payment", Binding: binding,
		NMI: &nmi.SaleEvidence{TransactionID: "paid", OrderReference: in.ID.String(), CustomerVaultID: "vault", Amount: minor, Currency: "USD", Approved: true}}}
	require.NoError(t, receipt.Validate(in))

	payload.Amount++ // same rail charge, different accepted liability
	changed := in
	changed.Payload, err = json.Marshal(payload)
	require.NoError(t, err)
	require.Error(t, receipt.Validate(changed))
}

// Ambiguous or malformed provider proof is refused before custody storage, without echoing raw provider text.
func TestInitialMembershipDeclineRejectsAmbiguousRawProof(t *testing.T) {
	for _, raw := range []string{
		"response=2&response=1&response_code=202&response_code=100",
		"response=2&response=2&response_code=202",
		"response=2&response_code=202&response_code=202",
		"response=2&response_code=100",
		"response=2&response_code=202&transactionid=one&transactionid=two",
		"response=2&response_code=202&responsetext=%QRAW_PROVIDER_SENTINEL",
	} {
		var store *Store // nil: any storage attempt panics
		err := store.RetainInitialMembershipDecline(t.Context(), gen.BillingProviderIntent{}, &nmi.CustomerVaultError{ResponseCode: 202, RawResponse: raw})
		require.Error(t, err, raw)
		require.NotContains(t, err.Error(), "RAW_PROVIDER_SENTINEL")
	}
}

// A collection accepted before usage orders names its order invoice_id; its
// receipt stays bound to that payload and kind.
func TestLegacyCollectionKeepsItsBinding(t *testing.T) {
	psp, order := uuid.New(), uuid.New()
	payload := OrderCollectionPayload{Initiator: charge.InitiatorMerchant, CustomerID: uuid.New(), PaymentID: uuid.New(), PaymentMethodID: uuid.New(),
		Rail: "nmi", Currency: "USD", Amount: 50_000, AmountMinor: 5, Instrument: charge.FrozenInstrument{PSPID: psp, Custodian: "psp", RailCustomerRef: "vault"}}
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	var fields map[string]any
	require.NoError(t, json.Unmarshal(raw, &fields))
	delete(fields, "order_id")
	fields["invoice_id"] = order.String()
	raw, err = json.Marshal(fields)
	require.NoError(t, err)
	in := gen.BillingProviderIntent{ID: uuid.New(), MerchantID: uuid.New(), PspID: &psp, IntentType: TypeOrderCollection, Rail: "nmi", Payload: raw}
	decoded, err := DecodeOrderCollectionPayload(in)
	require.NoError(t, err)
	require.Equal(t, order, decoded.OrderID)
	binding, err := collectionBinding(in)
	require.NoError(t, err)
	require.Equal(t, legacyCollectionKind, binding.Kind)

	fields["order_id"] = order.String()
	in.Payload, err = json.Marshal(fields)
	require.NoError(t, err)
	_, err = DecodeOrderCollectionPayload(in)
	require.Error(t, err, "a payload naming its order twice")
}
