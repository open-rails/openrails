package models

import (
	"time"

	"github.com/google/uuid"
)

// PaymentMethod represents a stored payment method across multiple rails
// This replaces rail-specific payment method tables
type PaymentMethod struct {
	ID uuid.UUID `json:"id"`
	// CustomerID is the OpenRails payable merchant subject for this row (#317).
	// Join openrails.customers for issuer/subject.
	CustomerID uuid.UUID `json:"customer_id,omitempty"`
	Rail       Rail      `json:"rail"` // Rail: nmi, ccbill, solana

	// PspID is the provider account that vaulted this method (#641).
	PspID uuid.UUID `json:"psp_id"`

	// Two-slot rail handle (#588): the customer-scope ref and the instrument-scope
	// ref, replacing the overloaded vault_id (+ NMI-ism billing_id).
	//
	// #682 honesty note: for NMI the "customer-scope" ref is INSTRUMENT-scoped in
	// our usage — OpenRails deliberately mints ONE vault customer PER CARD, so a
	// person with N cards is N unrelated NMI vault ids. NMI has no person-level
	// remote identity in our model; the person is the local customer_id UUID.
	RailCustomerRef      string `json:"-"` // customer-scope handle (NMI customer_vault_id — per-card by policy, see #682; "" for Stripe — see rail_customer_accounts)
	RailMethodRef        string `json:"-"` // instrument-scope handle (NMI billing_id — legacy imports only, see RebillDriver; Stripe pm_, Spreedly/HyperSwitch token)
	InitialTransactionID string `json:"-"` // Transaction that created this vault

	// RebillDriver (#682) is the EXPLICIT rebill-driver mode, decoupled from
	// identity: RebillDriverProvider = the rail's own recurring engine bills the
	// subscription; RebillDriverOpenRails = the OpenRails dunning worker drives
	// manual rebills. Previously inferred from RailMethodRef emptiness on NMI,
	// which made an identity field load-bearing as a behavior flag.
	RebillDriver string `json:"-"`

	// Stored-credential (CIT/MIT) replay references (#297), one per card-network
	// agreement type — the networks track separate credential-on-file sequences
	// for recurring vs unscheduled charges and the references are not
	// interchangeable. Rail-scoped value (NMI: the gateway transactionid of the
	// sequence's initial CIT, replayed as initial_transaction_id on MITs).
	// "" = not captured yet (legacy instrument, or no charge on that agreement
	// type); captures are write-once (CaptureStoredCredentialRef).
	StoredCredentialRecurringRef   string `json:"-"`
	StoredCredentialUnscheduledRef string `json:"-"`

	// Custodian (or#880) is WHO HOLDS this instrument — the axis orthogonal to
	// who charges it (Rail + PspID). Always stated, never empty; see the
	// Custodian* constants. "No stored instrument" (CCBill, Solana) is the
	// absence of a payment_methods row, not a custodian value.
	Custodian string `json:"-"`

	// Custodian-held instrument fields (#795, custodian='basis_theory').
	// Fingerprint is the custodian's stable PAN fingerprint (dedup/lookup);
	// ChargeVia routes pan_proxy|network_token; ParkReason non-empty =
	// instrument parked (custody-side problem; cancellation-last-resort,
	// never a terminal cancel).
	Fingerprint        string     `json:"-"`
	NetworkTokenID     string     `json:"-"`
	NetworkTokenStatus string     `json:"-"`
	NetworkTokenPAR    string     `json:"-"`
	ChargeVia          string     `json:"-"`
	ParkReason         string     `json:"-"`
	ParkedAt           *time.Time `json:"-"`

	// Payment method metadata
	LastFour   *string        `json:"last_four"`   // Last 4 digits of card
	CardType   *string        `json:"card_type"`   // "Visa", "MasterCard", etc.
	ExpiryDate *string        `json:"expiry_date"` // "MM/YY" format
	Metadata   map[string]any `json:"metadata,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// Relationships
	Subscriptions []*Subscription `json:"subscriptions,omitempty"`
}

// RebillDriver values (#682).
const (
	RebillDriverProvider  = "provider"
	RebillDriverOpenRails = "openrails"
)

// Custodian values (or#880) — payment_methods.custodian. Custody (who holds
// the card) is orthogonal to the processor (Rail + PspID, who charges it):
// psp/stripe, psp/nmi and basis_theory/nmi are all real combinations today.
// The DB CHECK pins the same set; adding a custodian is a migration.
const (
	// CustodianPSP: the instrument lives at the processor itself — a Stripe
	// pm_ on a Stripe Customer, an NMI customer_vault_id in the gateway.
	CustodianPSP = "psp"
	// CustodianBasisTheory: the PAN lives in the Basis Theory neutral vault
	// (#795) and is proxied to the processor at charge time.
	CustodianBasisTheory = "basis_theory"
)

// Custodians lists the declared custody values in stable order.
func Custodians() []string { return []string{CustodianPSP, CustodianBasisTheory} }

// PaymentMethodCharge is the DERIVED last-charge health for a payment method
// (#589) — computed at query time from openrails.payments, never a stored column.
type PaymentMethodCharge struct {
	LastChargedAt time.Time
	Status        string // raw payment_status: completed|failed|refunded|pending
}
