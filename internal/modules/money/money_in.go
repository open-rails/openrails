package money

import (
	"context"

	"github.com/open-rails/openrails/internal/modules/payments/charge"

	"github.com/google/uuid"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// PreparedCharge is one armed provider submission.
type PreparedCharge func(ctx context.Context) (ChargeResult, error)

func (f PreparedCharge) Submit(ctx context.Context) (ChargeResult, error) { return f(ctx) }

type ChargeRequest struct {
	MerchantID      uuid.UUID
	Payer           identity.CustomerID
	Invoker         string
	InvoiceID       *uuid.UUID
	PaymentMethodID uuid.UUID
	// AmountCents is rail minor units (cents for USD, whole yen for JPY),
	// always from NativeToRailMinor, never an inline /10_000 or /100.
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
	// Advice, NetworkAdvice and IssuerCode are a decline's advice, as
	// decline.Evidence names them.
	Advice, NetworkAdvice, IssuerCode string
}
