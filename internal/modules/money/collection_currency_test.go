package money

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/payments/rails/nmidirect"
	"github.com/open-rails/openrails/internal/modules/payments/rails/nmiproxy"
)

// or#864. Both collection adapters used to substitute money.DefaultCurrency
// for a missing currency in the statement immediately before charging a card.
// These tests charge through the REAL adapter entry points with an
// uninitialised rail client: if the currency gate holds, the call never
// reaches the client and the error names the currency; if it regresses, the
// call falls through and the error names the client instead. There is no
// third outcome, so the assertion cannot pass for the wrong reason.

func chargeRequestWithCurrency(currency string) ChargeRequest {
	return ChargeRequest{
		MerchantID:      uuid.New(),
		Payer:           identity.CustomerID(uuid.New()),
		PaymentMethodID: uuid.New(),
		AmountCents:     1999,
		Currency:        currency,
		IdempotencyKey:  "test-key",
	}
}

func TestNMICollectionRefusesUnestablishedCurrency(t *testing.T) {
	t.Parallel()

	// An unconfigured adapter: the currency gate must answer before it does.
	adapter := &NMICollectionAdapter{Charger: nmidirect.New((*nmi.NMIClient)(nil))}
	method := gen.OpenrailsPaymentMethod{
		ID:                             uuid.New(),
		Rail:                           "nmi",
		RailCustomerRef:                "vault-123",
		StoredCredentialUnscheduledRef: "approved-unscheduled",
	}

	for _, currency := range []string{"", "   ", "XXX", "usdd"} {
		_, err := adapter.Prepare(context.Background(), method, chargeRequestWithCurrency(currency))
		if err == nil {
			t.Fatalf("currency %q: charge must be refused", currency)
		}
		if !strings.Contains(err.Error(), "established currency") {
			t.Fatalf("currency %q: expected the currency gate to refuse, got %v", currency, err)
		}
	}

	// Control: a registered currency passes the gate and is armed for the
	// (nil) client — proving the gate is what refused above.
	adapter = &NMICollectionAdapter{Charger: nmidirect.New(&nmi.NMIClient{})}
	if _, err := adapter.Prepare(context.Background(), method, chargeRequestWithCurrency("usd")); err != nil {
		t.Fatalf("a registered currency must pass the gate, got %v", err)
	}
}

func TestCustodianProxyCollectionRefusesUnestablishedCurrency(t *testing.T) {
	t.Parallel()

	adapter := &CustodianProxyCollectionAdapter{Charger: nmiproxy.New(nil, nmiproxy.GatewayConfig{})}
	method := gen.OpenrailsPaymentMethod{
		ID:                             uuid.New(),
		Rail:                           nmiproxy.Rail,
		RailMethodRef:                  "bt-token-123",
		StoredCredentialUnscheduledRef: "approved-unscheduled",
	}

	for _, currency := range []string{"", "   ", "XXX"} {
		_, err := adapter.Prepare(context.Background(), method, chargeRequestWithCurrency(currency))
		if err == nil {
			t.Fatalf("currency %q: charge must be refused", currency)
		}
		if !strings.Contains(err.Error(), "established currency") {
			t.Fatalf("currency %q: expected the currency gate to refuse, got %v", currency, err)
		}
	}

	if _, err := adapter.Prepare(context.Background(), method, chargeRequestWithCurrency("USD")); err != nil {
		t.Fatalf("a registered currency must pass the gate, got %v", err)
	}
}

// TestScopedChargerRefusesUnestablishedCurrency pins the shared chokepoint: no
// rail adapter is even resolved, and no payment method is loaded, for a charge
// whose currency was never established. The DB handle is a bare struct — if
// the gate ever moved below the payment-method lookup this test would panic
// rather than pass.
func TestScopedChargerRefusesUnestablishedCurrency(t *testing.T) {
	t.Parallel()

	charger := NewScopedCharger(&db.DB{}, map[string]CollectionAdapter{})
	for _, currency := range []string{"", "  ", "XXX", "EURO"} {
		_, err := charger.Prepare(context.Background(), chargeRequestWithCurrency(currency))
		if err == nil {
			t.Fatalf("currency %q: charge must be refused", currency)
		}
		if !strings.Contains(err.Error(), "established currency") {
			t.Fatalf("currency %q: expected the currency gate to refuse, got %v", currency, err)
		}
	}
}

// TestCollectionCurrencyIsNormalizedNotDefaulted pins the distinction the
// audit turned on: normalisation (usd -> USD) is fine, substitution is not.
func TestCollectionCurrencyIsNormalizedNotDefaulted(t *testing.T) {
	t.Parallel()

	if got := normalizeCurrency(" jpy "); got != "JPY" {
		t.Fatalf("expected JPY, got %q", got)
	}
	if got := normalizeCurrency(""); got != "" {
		t.Fatalf("a blank currency must stay blank, not become %q", got)
	}
}
