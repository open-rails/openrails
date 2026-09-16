package money

import (
	"context"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/pkg/identity"
)

// Charger performs an off-session (merchant-initiated) charge of a saved
// payment method and returns a rail transaction id. It is implemented by
// the rail layer (Stripe MIT / NMI stored rebill) and faked in tests.
// Used by arrears invoice settlement.
type Charger interface {
	ChargeSavedMethod(ctx context.Context, req ChargeRequest) (ChargeResult, error)
}

type ChargeRequest struct {
	MerchantID      uuid.UUID
	Payer           identity.CustomerID
	Invoker         string
	InvoiceID       *uuid.UUID
	PaymentMethodID uuid.UUID
	// AmountCents is RAIL MINOR UNITS (typed Cents, #671): cents for USD/EUR,
	// whole yen for zero-decimal JPY — always produced via NativeToRailMinor,
	// never by an inline /10_000 or /100.
	AmountCents    moneyutil.Cents
	Currency       string
	IdempotencyKey string
	Description    string
}

type ChargeResult struct {
	Rail              string
	TransactionID     string
	ExternalInvoiceID string
	Declined          bool // true = hard decline (don't keep retrying); false+err = transient
	FailureCode       *string
	FailureMessage    *string
	// CapturedStoredCredentialRef is the rail-scoped stored-credential replay
	// reference this charge established for the instrument's UNSCHEDULED
	// agreement sequence (#297) — set when the instrument had none (first use
	// or legacy). ScopedCharger persists it write-once; "" = nothing captured.
	CapturedStoredCredentialRef string
}
