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
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/payments/rails/nmidirect"
	"github.com/open-rails/openrails/internal/modules/payments/rails/nmiproxy"
)

// CUR-8/CUR-9 (or#864): every off-session charge path refuses a currency that
// is not registered; normalisation is allowed, substitution never.
func TestCollectionRefusesUnestablishedCurrency(t *testing.T) {
	ctx := context.Background()
	method := gen.OpenrailsPaymentMethod{
		ID: uuid.New(), Rail: "nmi", RailCustomerRef: "vault-123", RailMethodRef: "bt-token-123",
		StoredCredentialUnscheduledRef: "approved-unscheduled",
	}
	request := func(currency string) ChargeRequest {
		return ChargeRequest{
			Initiator: charge.InitiatorMerchant, MerchantID: uuid.New(), Payer: identity.CustomerID(uuid.New()),
			PaymentMethodID: uuid.New(), AmountCents: 1999, Currency: currency, IdempotencyKey: "test-key",
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
		for _, currency := range []string{"", "   ", "XXX", "usdd", "EURO"} {
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

func TestInvoiceCollectionEligibility(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	past, future := now.Add(-time.Hour), now.Add(time.Hour)
	live := uuid.New()
	inv := func(edit func(*models.Invoice)) *models.Invoice {
		i := &models.Invoice{Status: "open", CollectionMethod: CollectionChargeAutomatically, AmountDue: 100}
		edit(i)
		return i
	}
	for _, tt := range []struct {
		name      string
		invoice   *models.Invoice
		threshold int64
		retry     bool // manual retry surface
		scheduled bool // automatic sweep
	}{
		{name: "new open invoice due", invoice: inv(func(i *models.Invoice) { i.DueAt = &past }), scheduled: true},
		{name: "open invoice not yet due", invoice: inv(func(i *models.Invoice) { i.DueAt = &future })},
		{name: "open after a failure", invoice: inv(func(i *models.Invoice) {
			i.CollectionFailureCount = 1
			i.NextCollectionAttemptAt = &past
		}), retry: true, scheduled: true},
		{name: "past due retry at exactly now", invoice: inv(func(i *models.Invoice) {
			i.Status, i.CollectionFailureCount, i.NextCollectionAttemptAt = "past_due", 1, &now
		}), retry: true, scheduled: true},
		{name: "past due retry in future", invoice: inv(func(i *models.Invoice) {
			i.Status, i.CollectionFailureCount, i.NextCollectionAttemptAt = "past_due", 1, &future
		}), retry: true},
		{name: "failed without a scheduled retry", invoice: inv(func(i *models.Invoice) {
			i.Status, i.CollectionFailureCount = "past_due", 1
		}), retry: true},
		{name: "uncollectible", invoice: inv(func(i *models.Invoice) { i.Status = "uncollectible" }), retry: true},
		{name: "below minimum threshold", invoice: inv(func(i *models.Invoice) {}), threshold: 101},
		{name: "at minimum threshold", invoice: inv(func(i *models.Invoice) {}), threshold: 100, scheduled: true},
		{name: "collection operation live", invoice: inv(func(i *models.Invoice) {
			i.Status, i.CollectionIntentID = "past_due", &live
		})},
		{name: "manual remittance", invoice: inv(func(i *models.Invoice) {
			i.Status, i.CollectionMethod = "past_due", CollectionSendInvoice
		})},
		{name: "paid", invoice: inv(func(i *models.Invoice) { i.Status, i.AmountDue = "paid", 0 })},
		{name: "past due with nothing owed", invoice: inv(func(i *models.Invoice) { i.Status, i.AmountDue = "past_due", 0 })},
		{name: "nil invoice"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.retry, invoiceCollectionRetryable(tt.invoice), "manual retry")
			require.Equal(t, tt.scheduled, scheduledInvoiceCollectionEligible(tt.invoice, tt.threshold, now), "scheduled")
		})
	}

	invoiceID := uuid.New()
	key := intents.InvoiceCollectionRetryKey(invoiceID, "client-key")
	require.Equal(t, key, intents.InvoiceCollectionRetryKey(invoiceID, "client-key"))
	require.NotEqual(t, key, intents.InvoiceCollectionRetryKey(uuid.New(), "client-key"), "scoped to the invoice")
	require.NotEqual(t, key, intents.InvoiceCollectionRetryKey(invoiceID, "other-key"), "scoped to the client key")
}
