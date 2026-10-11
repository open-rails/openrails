package money

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/payments/rails/nmidirect"
	"github.com/open-rails/openrails/internal/modules/payments/rails/nmiproxy"
)

// Every off-session charge path refuses a currency that is not registered;
// normalisation is allowed, substitution never.
func TestCollectionRefusesUnestablishedCurrency(t *testing.T) {
	ctx := context.Background()
	method := gen.BillingPaymentMethod{
		ID: uuid.New(), Rail: "nmi", Status: "active", RailCustomerRef: new("vault-123"), RailMethodRef: new("bt-token-123"),
	}
	request := func(currency string) ChargeRequest {
		return ChargeRequest{
			Initiator: charge.InitiatorMerchant, MerchantID: uuid.New(), Payer: identity.CustomerID(uuid.New()),
			PaymentMethodID: uuid.New(), AmountCents: 1999, Currency: currency, IdempotencyKey: "test-key",
			Instrument: charge.FrozenInstrument{Mandate: &charge.Mandate{ID: uuid.New(), Kind: charge.AgreementUnscheduled, InitialTransactionID: "approved-unscheduled"}},
		}
	}
	nmiAdapter := &NMICollectionAdapter{Charger: nmidirect.New(&nmi.NMIClient{})}
	proxyAdapter := &CustodianProxyCollectionAdapter{Charger: nmiproxy.New(nil, nmiproxy.GatewayConfig{})}
	// A bare DB handle: if the gate moved below the payment-method load this panics.
	scoped := NewScopedCharger(&db.DB{}, nil)
	paths := map[string]func(ChargeRequest) (PreparedCharge, error){
		"nmi":             func(r ChargeRequest) (PreparedCharge, error) { return nmiAdapter.Prepare(ctx, method, r) },
		"custodian_proxy": func(r ChargeRequest) (PreparedCharge, error) { return proxyAdapter.Prepare(ctx, method, r) },
		"scoped_charger":  func(r ChargeRequest) (PreparedCharge, error) { return scoped.Prepare(ctx, r) },
	}
	for name, prepare := range paths {
		for _, currency := range []string{"", "   ", "XXX", "usdd", "EURO", "SOL", "USDC"} {
			_, err := prepare(request(currency))
			require.ErrorContains(t, err, "established currency", "%s currency %q", name, currency)
		}
		_, err := prepare(request(" usd "))
		if name == "scoped_charger" {
			// Passes the gate, then stops at the missing merchant scope.
			require.Error(t, err)
			require.NotContains(t, err.Error(), "established currency")
			continue
		}
		require.NoError(t, err, name)
	}

	require.Equal(t, "JPY", NormalizeCurrency(" jpy "))
	require.Empty(t, NormalizeCurrency(""), "blank must stay blank, never defaulted")
	for code, want := range map[string]int{"USD": 6, "usd": 6, "JPY": 4} {
		got, err := CurrencyDecimals(code)
		require.NoError(t, err)
		require.Equal(t, want, got, code)
	}
	_, err := CurrencyDecimals("DOGE")
	require.Error(t, err)
	for _, code := range []string{"", "host-four/gold", "credit:00000000-0000-0000-0000-000000000000", "doge"} {
		require.Error(t, RequireBillingCurrency(code), code)
	}
}

func TestUsageCollectionEligibility(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	past, future := now.Add(-time.Hour), now.Add(time.Hour)
	order := func(edit func(*gen.BillingOrder)) gen.BillingOrder {
		paid, failures, method := int64(0), int32(0), CollectionChargeAutomatically
		o := gen.BillingOrder{Origin: "usage", Status: "open", CollectionMethod: &method, Total: 100, AmountPaid: &paid, CollectionFailureCount: &failures, DueAt: &past}
		edit(&o)
		return o
	}
	failed := func(n int32) *int32 { return &n }
	for _, tt := range []struct {
		name      string
		order     gen.BillingOrder
		threshold int64
		retry     bool // the merchant's retry
		scheduled bool // automatic collection
	}{
		{name: "new order past due", order: order(func(*gen.BillingOrder) {}), retry: true, scheduled: true},
		{name: "not yet due", order: order(func(o *gen.BillingOrder) { o.DueAt = &future })},
		{name: "after a failure", order: order(func(o *gen.BillingOrder) {
			o.CollectionFailureCount, o.NextCollectionAttemptAt, o.DueAt = failed(1), &past, &future
		}), retry: true},
		{name: "retry due", order: order(func(o *gen.BillingOrder) { o.CollectionFailureCount, o.NextCollectionAttemptAt = failed(1), &now }), retry: true, scheduled: true},
		{name: "retry in future", order: order(func(o *gen.BillingOrder) { o.CollectionFailureCount, o.NextCollectionAttemptAt = failed(1), &future }), retry: true},
		{name: "failed without a scheduled retry", order: order(func(o *gen.BillingOrder) { o.CollectionFailureCount = failed(1) }), retry: true},
		{name: "uncollectible", order: order(func(o *gen.BillingOrder) { o.Status = "uncollectible" }), retry: true},
		{name: "below minimum threshold", order: order(func(*gen.BillingOrder) {}), threshold: 101, retry: true},
		{name: "at minimum threshold", order: order(func(*gen.BillingOrder) {}), threshold: 100, retry: true, scheduled: true},
		{name: "collection live", order: order(func(o *gen.BillingOrder) { o.Status = "processing" })},
		{name: "manual remittance", order: order(func(o *gen.BillingOrder) {
			m := CollectionSendInvoice
			o.CollectionMethod = &m
		})},
		{name: "paid", order: order(func(o *gen.BillingOrder) { o.Status, o.AmountPaid = "complete", &o.Total })},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.retry, usageRetryable(tt.order, now), "merchant retry")
			require.Equal(t, tt.scheduled, scheduledUsageCollectionEligible(tt.order, tt.threshold, now), "scheduled")
		})
	}

	orderID := uuid.New()
	key := intents.OrderCollectionRetryKey(orderID, "client-key")
	require.Equal(t, key, intents.OrderCollectionRetryKey(orderID, "client-key"))
	require.True(t, intents.OrderCollectionRetryKeyValid(orderID, key))
	require.NotEqual(t, key, intents.OrderCollectionRetryKey(uuid.New(), "client-key"), "scoped to the order")
	require.NotEqual(t, key, intents.OrderCollectionRetryKey(orderID, "other-key"), "scoped to the client key")
}
