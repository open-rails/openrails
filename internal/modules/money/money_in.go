package money

import (
	"context"
	"github.com/open-rails/openrails/internal/modules/payments/charge"

	"github.com/google/uuid"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// Charger arms an off-session (merchant-initiated) charge of a saved payment
// method in two phases: Prepare does every local check and credential
// resolution and read-only qualification; Submit is the one provider
// submission. The invoice_collection intent fences between the two, so a
// Prepare failure never consumes an attempt and every Submit error is a
// possible submission. Implemented by ScopedCharger; faked in tests.
type Charger interface {
	Prepare(ctx context.Context, req ChargeRequest) (PreparedCharge, error)
}

// PreparedCharge is one armed provider submission.
type PreparedCharge interface {
	Submit(ctx context.Context) (ChargeResult, error)
}

// PreparedChargeFunc adapts a closure to PreparedCharge.
type PreparedChargeFunc func(ctx context.Context) (ChargeResult, error)

func (f PreparedChargeFunc) Submit(ctx context.Context) (ChargeResult, error) { return f(ctx) }

type ChargeRequest struct {
	MerchantID      uuid.UUID
	Payer           identity.CustomerID
	Invoker         string
	InvoiceID       *uuid.UUID
	PaymentMethodID uuid.UUID
	// AmountCents is RAIL MINOR UNITS (typed Cents, #671): cents for USD/EUR,
	// whole yen for zero-decimal JPY — always produced via NativeToRailMinor,
	// never by an inline /10_000 or /100.
	AmountCents moneyutil.Cents
	Currency    string
	// IdempotencyKey is the operation's provider identity: the NMI order id and
	// the Stripe idempotency-key root. Stable for the life of the operation.
	IdempotencyKey      string
	ProviderCustomerRef string
	Description         string
	// Instrument is the method as the operation froze it; the charge is
	// refused before submission if the method no longer matches.
	Instrument  charge.FrozenInstrument
	HyperSwitch *charge.HyperSwitchBinding
	// Initiator is frozen by the verified command admission, never decoded from a public request.
	Initiator charge.Initiator
}

type ChargeResult struct {
	Rail              string
	TransactionID     string
	ExternalInvoiceID string
	// Declined = the provider definitively refused this submission without
	// moving money (card decline or request rejection); FailureCode says which.
	// An error from Submit instead means the outcome is unknown.
	Declined       bool
	FailureCode    *string
	FailureMessage *string
}
