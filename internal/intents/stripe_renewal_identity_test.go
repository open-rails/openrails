package intents

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

func TestStripeRenewalKeepsExistingOperationIdentity(t *testing.T) {
	merchant, psp, sub, customer, price, product := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	boundary := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	p := subscriptions.SubscriptionCollectionPayload{
		Initiator: charge.InitiatorMerchant, PreviousPeriodEnd: boundary, AcceptedAt: boundary, PaymentMethodID: uuid.New(), AmountMinor: 100,
		OrderReference: subscriptions.ObligationOrderReference(sub, boundary),
		Instrument:     charge.FrozenInstrument{PSPID: psp, Custodian: "psp", RailCustomerRef: "cus_saved", RailMethodRef: "pm_saved", StoredCredentialRecurringRef: "pi_initial"},
		Renewal: subscriptions.RenewalTerms{PSPID: psp, SubscriptionID: sub, CustomerID: customer, FromPriceID: price, FromProductID: product,
			PriceID: price, ProductID: product, Amount: 1_000_000, Currency: "USD", PeriodStart: boundary, PeriodEnd: boundary.Add(24 * time.Hour)},
	}
	raw, err := json.Marshal(p)
	require.NoError(t, err)
	old := gen.BillingProviderIntent{ID: uuid.New(), MerchantID: merchant, PspID: &psp, SubscriptionID: &sub, PriceID: &price,
		Rail: "stripe", IntentType: subscriptions.TypeSubscriptionCollection, Origin: "system", Payload: raw,
		IdempotencyKey: subscriptions.SubscriptionCollectionKey(sub, boundary, 0)}
	oldID := old.ID
	params, err := StripeEngineParams(old)
	require.NoError(t, err)
	require.Equal(t, oldID, params.OperationID)
	require.Nil(t, params.Renewal, "existing provider parameters and receipts must not acquire new metadata")
	require.Equal(t, oldID, old.ID)

	current := old
	current.ID = subscriptions.SubscriptionCollectionOperationID(merchant, psp, p)
	params, err = StripeEngineParams(current)
	require.NoError(t, err)
	require.Equal(t, current.ID, params.OperationID)
	require.NotNil(t, params.Renewal)
	require.Equal(t, p.OrderReference, params.Renewal.Obligation)
	require.Len(t, params.Renewal.TermsSHA256, 64)
}
