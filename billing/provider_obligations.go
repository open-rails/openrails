package billing

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"
)

// Provider obligations reserve customer capacity for one provider operation and
// settle it from immutable provider observations. Amounts are native units of
// the authorization's currency, USD for now. Callers never supply a rated
// customer amount: OpenRails qualifies the evidence, rates it, and posts the
// one final settlement.

// ProviderBillingObservationMaxBytes bounds the canonical JSON encoding of one
// RecordProviderBillingObservationParams in every deployment, so a request accepted
// embedded also fits the HTTP body limit. An adapter whose provider response
// cannot fit submits ProviderBillingRefusalResponseTooLarge instead.
const ProviderBillingObservationMaxBytes = 768 << 10

// SHA256 is a digest encoded on the wire as 64 lowercase hex characters.
type SHA256 [sha256.Size]byte

func (d SHA256) String() string { return hex.EncodeToString(d[:]) }

func (d SHA256) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

func (d *SHA256) UnmarshalText(text []byte) error {
	if len(text) != 2*sha256.Size {
		return fmt.Errorf("sha256 digest must be %d lowercase hex characters", 2*sha256.Size)
	}
	for _, c := range text {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return fmt.Errorf("sha256 digest must be lowercase hex")
		}
	}
	_, err := hex.Decode(d[:], text)
	return err
}

type ProviderOperationState string

const (
	ProviderOperationOpen     ProviderOperationState = "open"
	ProviderOperationReleased ProviderOperationState = "released"
	ProviderOperationSettled  ProviderOperationState = "settled"
)

// OpenProviderOperationParams is exact host-authored authority for one
// provider operation, reserving Amount of the customer's capacity in
// Currency (USD is the only currency accepted for now). OperationID is also
// the provider operation's idempotency identity. OpenRails verifies the
// digest but never parses AuthorizationBody.
//
// OverdraftAmount lets a prepaid customer's capacity reach below zero: the hold
// is granted while balance - holds - owed - Amount >= -OverdraftAmount. Spend past
// the balance settles as owed, and the next funding repays it. It is policy for
// this call, not part of the operation's identity; arrears accounts use their
// credit line instead.
type OpenProviderOperationParams struct {
	OperationID             string     `json:"operation_id"` // canonical, at most 255 bytes
	CustomerID              CustomerID `json:"customer_id"`
	RecordOwner             string     `json:"record_owner"` // canonical, at most 255 bytes
	Currency                string     `json:"currency"`
	Amount                  int64      `json:"amount,string"`
	ClaimReference          string     `json:"claim_reference"`    // canonical, at most 1024 bytes
	AuthorizationBody       []byte     `json:"authorization_body"` // exact bytes, 1..65536
	AuthorizationBodySHA256 SHA256     `json:"authorization_body_sha256"`
	OverdraftAmount         int64      `json:"overdraft_amount,omitempty,string"`
}

// ProviderOperation is the durable hold on a customer's capacity for one
// upstream provider operation, and every route on it answers this shape.
// Amount is the opening hold and AuthorizedAmount the hold now: Amount plus
// every increment's grant, the latest of which is LastIncrement. Qualification
// is how far the provider's billing evidence qualifies the operation for
// settlement, null before the first observation. Settlement fields are null
// until OpenRails settles: SettlementCostAmount is the qualified provider
// cost, SettlementAmount what the customer is charged (equal under the
// pass-through contract, and never clamped to AuthorizedAmount). Refusal is
// set once the provider cost will not qualify automatically, and Resolution
// once an operator closed the hold. Replayed: the call repeated one already
// committed and changed nothing.
type ProviderOperation struct {
	OperationID             string                        `json:"operation_id"`
	MerchantID              MerchantID                    `json:"merchant_id"`
	CustomerID              CustomerID                    `json:"customer_id"`
	RecordOwner             string                        `json:"record_owner"`
	Currency                string                        `json:"currency"`
	Amount                  int64                         `json:"amount,string"`
	AuthorizedAmount        int64                         `json:"authorized_amount,string"`
	LastIncrement           *ProviderOperationIncrement   `json:"last_increment"`
	ClaimReference          string                        `json:"claim_reference"`
	AuthorizationBody       []byte                        `json:"authorization_body"`
	AuthorizationBodySHA256 SHA256                        `json:"authorization_body_sha256"`
	State                   ProviderOperationState        `json:"state"`
	TerminalReference       string                        `json:"terminal_reference"`
	Qualification           *ProviderBillingQualification `json:"qualification"`
	SettlementCostAmount    *int64                        `json:"settlement_cost_amount,string"`
	SettlementAmount        *int64                        `json:"settlement_amount,string"`
	SettlementBody          []byte                        `json:"settlement_body"`
	SettlementBodySHA256    *SHA256                       `json:"settlement_body_sha256"`
	Refusal                 *ProviderBillingRefusal       `json:"refusal"`
	Resolution              *ProviderBillingResolution    `json:"resolution"`
	CreatedAt               time.Time                     `json:"created_at"`
	ReleasedAt              *time.Time                    `json:"released_at"`
	SettledAt               *time.Time                    `json:"settled_at"`
	Replayed                bool                          `json:"replayed"`
}

// IncrementProviderOperationParams grows an open hold by up to Amount,
// accepting no less than MinimumAmount (0 < MinimumAmount <= Amount), under the
// same capacity rule as opening it. Ordinal numbers the operation's increments
// from 1 without gaps; repeating a committed ordinal with the same amounts
// replays it. OverdraftAmount is as for opening.
type IncrementProviderOperationParams struct {
	OperationID     string `json:"-"` // carried by the route path
	Ordinal         int64  `json:"ordinal"`
	Amount          int64  `json:"amount,string"`
	MinimumAmount   int64  `json:"minimum_amount,string"`
	OverdraftAmount int64  `json:"overdraft_amount,omitempty,string"`
}

// ProviderOperationIncrement is one committed growth of a hold: what the host
// asked for (Amount, at least MinimumAmount) and what it was granted.
type ProviderOperationIncrement struct {
	Ordinal       int64     `json:"ordinal"`
	Amount        int64     `json:"amount,string"`
	MinimumAmount int64     `json:"minimum_amount,string"`
	GrantedAmount int64     `json:"granted_amount,string"`
	CreatedAt     time.Time `json:"created_at"`
}

// ReleaseProviderOperationParams releases an open hold after the host proves
// the provider operation never happened. Any billing evidence or refusal
// refuses release.
type ReleaseProviderOperationParams struct {
	OperationID      string `json:"-"`                 // carried by the route path
	ReleaseReference string `json:"release_reference"` // canonical opaque proof, at most 1024 bytes
}

type ProviderBillingQualificationState string

const (
	ProviderBillingQualificationPending  ProviderBillingQualificationState = "pending"
	ProviderBillingQualificationRefused  ProviderBillingQualificationState = "refused"
	ProviderBillingQualificationEligible ProviderBillingQualificationState = "eligible"
)

type ProviderBillingQualificationReason string

const (
	ProviderBillingAwaitingEqualObservation ProviderBillingQualificationReason = "awaiting_equal_observation"
	ProviderBillingAwaitingQuiescence       ProviderBillingQualificationReason = "awaiting_quiescence"
	ProviderBillingCoverageIncomplete       ProviderBillingQualificationReason = "coverage_incomplete"
	ProviderBillingObservationChanged       ProviderBillingQualificationReason = "observation_changed"
	ProviderBillingProviderEvidenceRefused  ProviderBillingQualificationReason = "provider_evidence_refused"
	ProviderBillingNegativeOrCorrective     ProviderBillingQualificationReason = "negative_or_corrective_record"
	ProviderBillingDecreasingProviderCost   ProviderBillingQualificationReason = "decreasing_provider_cost"
	ProviderBillingEligible                 ProviderBillingQualificationReason = "eligible"

	// The host's refusal kinds are also the reasons of the refusal they record.
	// They appear only on a ProviderBillingRefusal.
	ProviderBillingLifecycleUnprovable ProviderBillingQualificationReason = "lifecycle_unprovable"
	ProviderBillingUnavailable         ProviderBillingQualificationReason = "provider_billing_unavailable"
	ProviderBillingObservationRejected ProviderBillingQualificationReason = "observation_rejected"
)

// ProviderBillingLifecycleEvidence is immutable per operation: the first
// observation fixes it and later observations must repeat it exactly.
type ProviderBillingLifecycleEvidence struct {
	Provider                 string    `json:"provider"`
	ProviderResourceID       string    `json:"provider_resource_id"`
	ProviderLifetimeStartsAt time.Time `json:"provider_lifetime_starts_at"`
	ProviderLifetimeEndsAt   time.Time `json:"provider_lifetime_ends_at"`
	ProviderAbsentAt         time.Time `json:"provider_absent_at"`
	ProviderAbsenceReference string    `json:"provider_absence_reference"`
	BillingStopReference     string    `json:"billing_stop_reference"`
	WindowsClosedAt          time.Time `json:"windows_closed_at"`
	WindowsClosedReference   string    `json:"windows_closed_reference"`
	LifecycleEvidenceBody    []byte    `json:"lifecycle_evidence_body"`
}

// ProviderBillingRecord is one provider-reported cost bucket, decoded exactly by
// the provider adapter, in the operation's currency. It is evidence, not a
// customer charge.
type ProviderBillingRecord struct {
	ProviderResourceID string    `json:"provider_resource_id"`
	BucketStart        time.Time `json:"bucket_start"`
	Amount             int64     `json:"amount,string"`
	TimeBilledMS       int64     `json:"time_billed_ms,string"`
}

// ProviderBillingRefusalKind is why an observation carries no usable records.
type ProviderBillingRefusalKind string

const (
	// The adapter read the provider but cannot decode its answer exactly. The
	// observation carries the lifecycle and query, and the raw body except for
	// response_too_large.
	ProviderBillingRefusalSchemaAmbiguity  ProviderBillingRefusalKind = "schema_ambiguity"
	ProviderBillingRefusalSubmicroAmount   ProviderBillingRefusalKind = "submicro_amount"
	ProviderBillingRefusalAmountOverflow   ProviderBillingRefusalKind = "amount_overflow"
	ProviderBillingRefusalResponseTooLarge ProviderBillingRefusalKind = "response_too_large"

	// The host cannot produce evidence at all: it cannot prove the provider
	// resource's lifecycle, the provider reports no billing it can read, or
	// OpenRails rejected its evidence. The observation carries no evidence (no
	// lifecycle, query, raw body or records) and refuses the hold at once.
	ProviderBillingRefusalLifecycleUnprovable ProviderBillingRefusalKind = "lifecycle_unprovable"
	ProviderBillingRefusalUnavailable         ProviderBillingRefusalKind = "provider_billing_unavailable"
	ProviderBillingRefusalObservationRejected ProviderBillingRefusalKind = "observation_rejected"
)

// HostRefusal reports whether the kind is the host's statement that it has no
// evidence, rather than an adapter's refusal of evidence it read.
func (k ProviderBillingRefusalKind) HostRefusal() bool {
	switch k {
	case ProviderBillingRefusalLifecycleUnprovable, ProviderBillingRefusalUnavailable, ProviderBillingRefusalObservationRejected:
		return true
	}
	return false
}

// ProviderBillingObservationRefusal is a typed refusal. OpenRails persists it
// and never parses a raw provider body. Detail is an optional canonical note
// (at most 4096 bytes) for a host refusal.
type ProviderBillingObservationRefusal struct {
	Kind   ProviderBillingRefusalKind `json:"kind"`
	Detail string                     `json:"detail"`
}

// RecordProviderBillingObservationParams appends one immutable provider billing
// read, or the host's refusal to produce one. It carries no rated amount;
// eligible evidence settles inside the same commit. ObservationID names the
// observation: repeating it with the same terms replays, with a changed term
// it is refused.
type RecordProviderBillingObservationParams struct {
	OperationID     string                             `json:"-"` // carried by the route path
	ObservationID   string                             `json:"observation_id"`
	Lifecycle       ProviderBillingLifecycleEvidence   `json:"lifecycle"`
	NormalizedQuery string                             `json:"normalized_query"`
	QueryStartsAt   time.Time                          `json:"query_starts_at"`
	QueryEndsAt     time.Time                          `json:"query_ends_at"`
	RawBody         []byte                             `json:"raw_body"`
	Records         []ProviderBillingRecord            `json:"records"`
	Refusal         *ProviderBillingObservationRefusal `json:"refusal"`
}

// ProviderBillingQualification is whether an operation's provider evidence
// qualifies for settlement. QualifiedCostAmount is in the operation's
// currency, null until eligible.
type ProviderBillingQualification struct {
	Lifecycle               ProviderBillingLifecycleEvidence   `json:"lifecycle"`
	LifecycleEvidenceSHA256 SHA256                             `json:"lifecycle_evidence_sha256"`
	QuiescenceSeconds       int64                              `json:"quiescence_seconds"`
	State                   ProviderBillingQualificationState  `json:"state"`
	Reason                  ProviderBillingQualificationReason `json:"reason"`
	BaselineObservationID   string                             `json:"baseline_observation_id"`
	QualifiedObservationID  string                             `json:"qualified_observation_id"`
	QualifiedCostAmount     *int64                             `json:"qualified_cost_amount,string"`
	QualifiedAt             *time.Time                         `json:"qualified_at"`
	CreatedAt               time.Time                          `json:"created_at"`
	UpdatedAt               time.Time                          `json:"updated_at"`
}

type ProviderBillingResolutionKind string

const (
	// ProviderBillingResolutionSettled settles the hold at the operator-attested
	// provider cost, under the pass-through contract: the customer is charged
	// that cost, above the hold as owed.
	ProviderBillingResolutionSettled ProviderBillingResolutionKind = "settled"
	// ProviderBillingResolutionWrittenOff releases the hold without charging the
	// customer; the merchant absorbs whatever the provider billed.
	ProviderBillingResolutionWrittenOff ProviderBillingResolutionKind = "written_off"
)

// ProviderBillingResolution is the operator's recorded close of a refused
// hold. Immutable.
type ProviderBillingResolution struct {
	Kind       ProviderBillingResolutionKind `json:"kind"`
	CostAmount *int64                        `json:"cost_amount,string"`
	AttestedBy string                        `json:"attested_by"`
	Reference  string                        `json:"reference"`
	Note       string                        `json:"note"`
	ResolvedAt time.Time                     `json:"resolved_at"`
}

// ProviderBillingRefusal records that an operation's provider cost will not
// qualify automatically: refused by OpenRails' qualifier (Reason is the
// qualification's) or by the host (Reason is its refusal kind). Detail names
// the observation that refused it. Its hold waits for an operator. Immutable.
type ProviderBillingRefusal struct {
	Reason    ProviderBillingQualificationReason `json:"reason"`
	Detail    string                             `json:"detail"`
	RefusedAt time.Time                          `json:"refused_at"`
}

// CloseProviderOperationParams closes a refused hold on an operator's
// attestation, since automatic settlement never will. Kind settled charges
// CostAmount (in the operation's currency) under the pass-through contract,
// above the hold as owed; written_off releases the hold without charging the
// customer, and CostAmount is null. AttestedBy names the operator and Reference
// the evidence (a provider invoice or ticket), both opaque and canonical; Note
// is optional. Repeating the same close replays; a changed term is refused.
type CloseProviderOperationParams struct {
	OperationID string                        `json:"-"` // carried by the route path
	Kind        ProviderBillingResolutionKind `json:"kind"`
	CostAmount  *int64                        `json:"cost_amount,string"`
	AttestedBy  string                        `json:"attested_by"` // at most 255 bytes
	Reference   string                        `json:"reference"`   // at most 1024 bytes
	Note        string                        `json:"note"`        // at most 4096 bytes
}

// ProviderOperationListParams filters ListProviderOperations. An empty State
// admits every state and a nil Refused both; Refused true with State open lists
// the holds waiting for an operator.
type ProviderOperationListParams struct {
	PageRequest
	State   []ProviderOperationState
	Refused *bool
}
