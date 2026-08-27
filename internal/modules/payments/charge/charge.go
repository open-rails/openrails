// Package charge is the narrow, rail-agnostic charge seam (#297 Phase A):
// "charge instrument, amount, CIT/MIT context". It models card-network
// stored-credential compliance once, in the engine, so every card rail —
// the direct NMI transport today, a custodian-proxied one later (#297 Phase B) —
// implements ONE interface and the vault lands as a rail, not a rewrite.
//
// Network model (verified against the NMI integration portal + docs.nmi.com,
// 2026-07-06):
//   - Every charge on a stored credential declares WHO initiated it
//     (customer = CIT, merchant = MIT) and whether this is the INITIAL
//     transaction of a credential-on-file sequence or a subsequent use.
//   - The networks track a SEPARATE sequence per agreement type (recurring vs
//     unscheduled); the initial transaction's reference anchors that sequence
//     and is replayed on every subsequent MIT of the SAME type only.
//   - The replay reference is RAIL-SCOPED: NMI uses its own gateway
//     transactionid (mapped to the network transaction identifier inside the
//     gateway); a vault rail forwards its vault-native reference. The engine
//     persists whatever the rail hands back (payment_methods.
//     stored_credential_{recurring,unscheduled}_ref) and never interprets it.
//
// Sites that cannot ride the generic seam still share this vocabulary: the
// NMI subscription-engine lanes (add_subscription, rebill_subscription) charge
// against NMI's own schedule objects rather than (instrument, amount), so they
// derive their wire fields from a Context via the nmidirect mapping instead of
// calling Charger.
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
	// dunning retries, auto top-ups, arrears collection.
	InitiatorMerchant Initiator = "merchant"
)

// Agreement is the card-network credential-on-file agreement type. The
// networks keep independent stored-credential sequences per agreement, so a
// reference captured under one type must never be replayed under the other.
type Agreement string

const (
	// AgreementRecurring: fixed-cadence subscription charges and their dunning
	// retries.
	AgreementRecurring Agreement = "recurring"
	// AgreementUnscheduled: no fixed cadence — one-time checkout on a stored
	// card, auto top-ups, arrears/invoice collection.
	AgreementUnscheduled Agreement = "unscheduled"
)

// Context is the CIT/MIT stored-credential posture of one charge.
type Context struct {
	Initiator Initiator
	Agreement Agreement

	// FirstUse: this customer-present charge is the initial transaction of the
	// credential's agreement sequence (the anchor later transactions reference).
	FirstUse bool

	// PriorRef is the rail-scoped replay reference captured from the
	// sequence's initial transaction. It is required for a compliant subsequent
	// CIT or MIT. Historical merchant-initiated charges may leave it empty only
	// for the observable best-effort recovery path; new integrations must
	// always capture and replay it.
	PriorRef string

	// UnanchoredBestEffort is an explicit availability-first exception for a
	// merchant-initiated charge whose original reference cannot be recovered.
	// Rail adapters must never infer this posture from an empty PriorRef.
	UnanchoredBestEffort bool
}

// InitialOneTime: cardholder-present charge anchoring the unscheduled
// sequence (first checkout charge on a newly stored or never-anchored card).
func InitialOneTime() Context {
	return Context{Initiator: InitiatorCustomer, Agreement: AgreementUnscheduled, FirstUse: true}
}

// OneTimeReuse: cardholder-present charge on an already-anchored stored
// credential (returning customer, saved-card checkout, upgrade proration).
func OneTimeReuse(priorRef string) Context {
	return Context{Initiator: InitiatorCustomer, Agreement: AgreementUnscheduled, PriorRef: priorRef}
}

// InitialRecurring: cardholder-present enrollment charge anchoring the
// recurring sequence (subscription signup / upgrade successor).
func InitialRecurring() Context {
	return Context{Initiator: InitiatorCustomer, Agreement: AgreementRecurring, FirstUse: true}
}

// RecurringReuse: cardholder-present recurring enrollment on a card whose
// recurring sequence is already anchored (second subscription on one card).
func RecurringReuse(priorRef string) Context {
	return Context{Initiator: InitiatorCustomer, Agreement: AgreementRecurring, PriorRef: priorRef}
}

// RecurringMIT: merchant-initiated recurring charge (renewal, dunning retry).
// priorRef must identify the approved initial recurring CIT. Use the explicitly
// named legacy constructor when no reference can be recovered.
func RecurringMIT(priorRef string) Context {
	return Context{Initiator: InitiatorMerchant, Agreement: AgreementRecurring, PriorRef: priorRef}
}

// UnscheduledMIT: merchant-initiated unscheduled charge (auto top-up,
// arrears/invoice collection). priorRef must identify the approved initial
// unscheduled CIT. Use the explicitly named legacy constructor when no
// reference can be recovered.
func UnscheduledMIT(priorRef string) Context {
	return Context{Initiator: InitiatorMerchant, Agreement: AgreementUnscheduled, PriorRef: priorRef}
}

// LegacyUnanchoredRecurringMIT is the explicit recovery path for historical
// recurring instruments whose original CIT reference is unrecoverable.
func LegacyUnanchoredRecurringMIT() Context {
	return Context{
		Initiator:            InitiatorMerchant,
		Agreement:            AgreementRecurring,
		UnanchoredBestEffort: true,
	}
}

// LegacyUnanchoredUnscheduledMIT is the explicit recovery path for historical
// unscheduled instruments whose original CIT reference is unrecoverable.
func LegacyUnanchoredUnscheduledMIT() Context {
	return Context{
		Initiator:            InitiatorMerchant,
		Agreement:            AgreementUnscheduled,
		UnanchoredBestEffort: true,
	}
}

// Instrument identifies the stored payment credential to charge, by its
// rail-scoped handles (mirrors openrails.payment_methods).
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
// charge (#796): the approval_rate dimension that makes the network-token
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

	// CapturedRef is the rail-scoped stored-credential replay reference an
	// approved initial customer-present charge established for the request's
	// agreement type. The caller persists it on the instrument (write-once).
	// "" = nothing to persist.
	CapturedRef string

	// Declined: parsed hard decline — do not retry with the same instrument.
	Declined       bool
	FailureCode    *string
	FailureMessage *string
}

// Charger is the seam: one interface per card rail. Implementations translate
// Context into the rail's stored-credential wire fields and normalize the
// outcome.
type Charger interface {
	Charge(ctx context.Context, req Request) (Result, error)
}
