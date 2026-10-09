// Package charge is the narrow, rail-agnostic charge seam (#297): "charge an
// instrument, an amount, under a flow". Callers state the flow (who initiates
// and which agreement the charge runs under); each rail derives its network
// and stored-credential flags from it (nmidirect.StoredCredentialFor, the
// Stripe engine's flags) and never takes them from callers.
//
// Network model (NMI integration portal + docs.nmi.com, 2026-07-06):
//   - A charge on a stored credential declares who initiated it (customer =
//     CIT, merchant = MIT) and whether it is the storing transaction of an
//     agreement or a later use.
//   - An agreement is a mandate (internal/modules/mandates): recurring for one
//     subscription, unscheduled for collection in one currency, card_on_file
//     for one-click reuse. Its storing transaction's references are its
//     lineage, scoped to the gateway account that ran it; later charges send
//     them. The networks keep the recurring and unscheduled sequences apart,
//     and card_on_file rides the unscheduled one.
//
// The NMI schedule lanes (add_subscription, rebill_subscription) charge NMI's
// own schedule objects rather than (instrument, amount), so they derive their
// wire fields from a Context through nmidirect instead of calling Charger.
package charge

import (
	"context"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// Initiator is who initiates a charge on a stored credential.
type Initiator string

const (
	// InitiatorCustomer: the cardholder is present and acting (CIT).
	InitiatorCustomer Initiator = "customer"
	// InitiatorMerchant: the engine charges off-session (MIT) — renewals,
	// dunning retries and collection.
	InitiatorMerchant Initiator = "merchant"
)

// Agreement is the kind of mandate a charge runs under.
type Agreement string

const (
	// AgreementNone: a purchase that keeps nothing for reuse; no stored-
	// credential indicator.
	AgreementNone Agreement = ""
	// AgreementRecurring: one subscription's fixed-cadence charges.
	AgreementRecurring Agreement = "recurring"
	// AgreementUnscheduled: collection in one currency (invoices, top-ups).
	AgreementUnscheduled Agreement = "unscheduled"
	// AgreementCardOnFile: the customer's own one-click reuse; never
	// merchant-initiated.
	AgreementCardOnFile Agreement = "card_on_file"
)

// Sequence is the network credential-on-file sequence an agreement's
// references belong to: recurring, or unscheduled for the other two.
func (a Agreement) Sequence() Agreement {
	if a == AgreementCardOnFile {
		return AgreementUnscheduled
	}
	return a
}

// Mandate is an agreement's lineage as a charge cites it.
type Mandate struct {
	ID   uuid.UUID `json:"id"`
	Kind Agreement `json:"kind"`
	// InitialTransactionID is the provider's id of the storing transaction.
	InitialTransactionID string `json:"initial_transaction_id"`
	NetworkTransactionID string `json:"network_transaction_id,omitempty"`
	TransactionLinkID    string `json:"transaction_link_id,omitempty"`
}

// Context is the flow of one charge.
type Context struct {
	Initiator Initiator
	Agreement Agreement
	// Cites is the lineage a later charge references; nil when the charge is
	// its agreement's storing transaction.
	Cites *Mandate
}

// Purchase is a customer-present charge that stores nothing.
func Purchase() Context { return Context{Initiator: InitiatorCustomer} }

// Customer is a customer-present charge under an agreement: the storing
// transaction when cites is nil, else a use of the cited lineage.
func Customer(a Agreement, cites *Mandate) Context {
	return Context{Initiator: InitiatorCustomer, Agreement: a, Cites: cites}
}

// Merchant is a merchant-initiated charge under its mandate.
func Merchant(a Agreement, mandate *Mandate) Context {
	return Context{Initiator: InitiatorMerchant, Agreement: a, Cites: mandate}
}

// Storing reports whether the charge establishes its agreement's lineage.
func (c Context) Storing() bool {
	return c.Initiator == InitiatorCustomer && c.Agreement != AgreementNone && c.Cites == nil
}

// SentInitialTransactionID is the reference the charge sends, if any.
func (c Context) SentInitialTransactionID() string {
	if c.Cites == nil {
		return ""
	}
	return c.Cites.InitialTransactionID
}

// Instrument identifies the stored payment credential to charge, by its
// rail-scoped handles (mirrors billing.payment_methods).
type Instrument struct {
	// PaymentMethodID is the local payment_methods row id (uuid.Nil when the
	// caller only holds rail handles).
	PaymentMethodID uuid.UUID
	Rail            string
	// CustomerRef is the customer-scope rail handle (NMI customer_vault_id).
	CustomerRef string
	// MethodRef is the instrument-scope rail handle (NMI billing_id; "" =
	// the vault's priority-1 entry).
	MethodRef string
}

// Request charges one instrument for one amount under one CIT/MIT context.
type Request struct {
	Instrument Instrument
	// AmountMinor is rail minor units (typed Cents, #671).
	AmountMinor moneyutil.Cents
	Currency    string
	Description string
	// OrderRef is the rail correlation handle (NMI order id) — the reference
	// verify legs answer "did this charge land?" by.
	OrderRef string
	Context  Context
}

// TokenType is the credential form the rail presented to the network for one
// charge (#796): the token_type dimension that makes the network-token
// uplift measurable. Stamped by the rail at charge time; "" = unknown.
const (
	// TokenTypePSPToken: the PSP holds the card and charged its own stored
	// credential (NMI customer vault, a Stripe pm_).
	TokenTypePSPToken = "psp_token"
	// TokenTypePANViaProxy: a custodian-held FPAN detokenized through its
	// proxy into the gateway (charge_via=pan_proxy).
	TokenTypePANViaProxy = "pan_via_proxy"
	// TokenTypeNetworkToken: a network token (DPAN) was presented.
	TokenTypeNetworkToken = "network_token"
)

// Result is the normalized outcome of an executed charge. A (Result, nil)
// return means the gateway answered: either an approval or a parsed hard
// decline. Transport failures and transient gateway errors return an error
// (callers classify ambiguity rail-side, e.g. nmi.IsTransportAmbiguous).
type Result struct {
	TransactionID string

	// TokenType is the credential form presented (TokenType* consts, #796).
	// Set on approvals AND parsed declines; "" only when the rail predates
	// the instrumentation.
	TokenType string

	// CapturedRef is the initial transaction id an approved storing charge
	// established; the caller records it on the agreement's mandate. "" =
	// nothing to record.
	CapturedRef string

	// Declined: parsed hard decline — do not retry with the same instrument.
	Declined       bool
	FailureCode    *string
	FailureMessage *string
}

// Charger is the seam: one interface per card rail. Implementations derive
// the rail's stored-credential wire fields from Context and normalize the
// outcome.
type Charger interface {
	Charge(ctx context.Context, req Request) (Result, error)
}
