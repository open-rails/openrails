package subscriptions

import (
	"encoding/json"
	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestEngineReplacementRequiresAcceptedCustomerCIT(t *testing.T) {
	now := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	old, newMethod, psp, customer, sub, price, product := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	p := SubscriptionCollectionPayload{Initiator: charge.InitiatorCustomer, ReplacePaymentMethod: true, PreviousPaymentMethodID: &old, RequestedPaymentMethodID: &newMethod, PaymentMethodID: newMethod, Instrument: charge.FrozenInstrument{PSPID: psp, Custodian: "psp", RailCustomerRef: "new-vault", RailMethodRef: "new-billing"}, Renewal: RenewalTerms{PSPID: psp, SubscriptionID: sub, CustomerID: customer, FromPriceID: price, PriceID: price, FromProductID: product, ProductID: product, Amount: 9990000, Currency: "USD", PeriodStart: now, PeriodEnd: now.Add(720 * time.Hour)}, PreviousPeriodEnd: now, AcceptedAt: now, AmountMinor: 999, Attempt: 1, FailureCount: 1}
	key := charge.CustomerPaymentKey(TypeSubscriptionCollection, customer, "new-card")
	p.OrderReference = RebillOrderReference(key)
	actor := customer.String()
	in := gen.OpenrailsRailIntent{ID: uuid.New(), MerchantID: uuid.New(), Rail: "nmi", PspID: &psp, SubscriptionID: &sub, PriceID: &price, Origin: "user", Actor: &actor, IntentType: TypeSubscriptionCollection, IdempotencyKey: key}
	decode := func(t *testing.T, p SubscriptionCollectionPayload, in gen.OpenrailsRailIntent) error {
		t.Helper()
		raw, err := json.Marshal(p)
		require.NoError(t, err)
		in.Payload = raw
		_, err = DecodeSubscriptionCollectionPayload(in)
		return err
	}
	require.NoError(t, decode(t, p, in))
	require.True(t, p.MatchesSubscriptionMethod(&old))
	require.False(t, p.MatchesSubscriptionMethod(&newMethod))
	absent := p
	absent.PreviousPaymentMethodID = nil
	require.NoError(t, decode(t, absent, in))
	require.True(t, absent.MatchesSubscriptionMethod(nil))
	require.False(t, absent.MatchesSubscriptionMethod(&old))
	for _, tc := range []struct {
		name   string
		mutate func(*SubscriptionCollectionPayload, *gen.OpenrailsRailIntent)
	}{
		{"merchant cannot assert presence", func(p *SubscriptionCollectionPayload, in *gen.OpenrailsRailIntent) {
			p.Initiator = charge.InitiatorMerchant
			in.Origin = "system"
		}},
		{"another actor", func(p *SubscriptionCollectionPayload, in *gen.OpenrailsRailIntent) {
			v := uuid.NewString()
			in.Actor = &v
		}},
		{"replacement requires chosen method", func(p *SubscriptionCollectionPayload, in *gen.OpenrailsRailIntent) { p.RequestedPaymentMethodID = nil }},
		{"ordinary renewal requires anchor", func(p *SubscriptionCollectionPayload, in *gen.OpenrailsRailIntent) {
			p.ReplacePaymentMethod = false
			p.PreviousPaymentMethodID = nil
		}},
		{"no fake replacement", func(p *SubscriptionCollectionPayload, in *gen.OpenrailsRailIntent) {
			p.PreviousPaymentMethodID = &newMethod
		}},
		{"Stripe needs qualified setup", func(p *SubscriptionCollectionPayload, in *gen.OpenrailsRailIntent) { in.Rail = "stripe" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate, envelope := p, in
			tc.mutate(&candidate, &envelope)
			require.Error(t, decode(t, candidate, envelope))
		})
	}
}
