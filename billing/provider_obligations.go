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

type OperationAuthorizationState string

const (
	OperationAuthorizationOpen     OperationAuthorizationState = "open"
	OperationAuthorizationReleased OperationAuthorizationState = "released"
	OperationAuthorizationSettled  OperationAuthorizationState = "settled"
)

// OpenOperationAuthorizationParams is exact host-authored authority for one
// provider operation, reserving Amount of the customer's capacity in
// Currency (USD is the only currency accepted for now). OperationID is also
// the provider operation's idempotency identity. OpenRails verifies the
// digest but never parses AuthorizationBody.
//
// OverdraftAmount lets a prepaid customer's capacity reach below zero: the hold
// is granted while balance - holds - owed - Amount >= -OverdraftAmount. Spend past
// the balance settles as owed, and the next funding repays it. It is policy for
// this call, not part of the authorization's identity; arrears accounts use their
// credit line instead.
type OpenOperationAuthorizationParams struct {
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

// OperationAuthorization is the durable reservation. Amount is the opening
// hold and AuthorizedAmount the hold now: Amount plus every extension grant.
// Settlement fields are null until OpenRails settles qualified provider
// evidence: SettlementCostAmount is the qualified provider cost,
// SettlementAmount what the customer is charged (equal under the pass-through
// contract, and never clamped to AuthorizedAmount). Refusal is set once the
// hold's provider cost will not qualify automatically, and Resolution once an
// operator closed it.
type OperationAuthorization struct {
	OperationID             string                      `json:"operation_id"`
	MerchantID              MerchantID                  `json:"merchant_id"`
	CustomerID              CustomerID                  `json:"customer_id"`
	RecordOwner             string                      `json:"record_owner"`
	Currency                string                      `json:"currency"`
	Amount                  int64                       `json:"amount,string"`
	AuthorizedAmount        int64                       `json:"authorized_amount,string"`
	ClaimReference          string                      `json:"claim_reference"`
	AuthorizationBody       []byte                      `json:"authorization_body"`
	AuthorizationBodySHA256 SHA256                      `json:"authorization_body_sha256"`
	State                   OperationAuthorizationState `json:"state"`
	TerminalReference       string                      `json:"terminal_reference"`
	SettlementCostAmount    *int64                      `json:"settlement_cost_amount,string"`
	SettlementAmount        *int64                      `json:"settlement_amount,string"`
	SettlementBody          []byte                      `json:"settlement_body"`
	SettlementBodySHA256    *SHA256                     `json:"settlement_body_sha256"`
	Refusal                 *ProviderBillingRefusal     `json:"refusal"`
	Resolution              *ProviderBillingResolution  `json:"resolution"`
	CreatedAt               time.Time                   `json:"created_at"`
	ReleasedAt              *time.Time                  `json:"released_at"`
	SettledAt               *time.Time                  `json:"settled_at"`
	Replayed                bool                        `json:"replayed"`
}

// ExtendOperationAuthorizationParams grows an open reservation by up to Amount,
// accepting no less than MinimumAmount (0 < MinimumAmount <= Amount), under the
// same capacity rule as opening it. Ordinal numbers the operation's extensions
// from 1 without gaps; repeating a committed ordinal with the same amounts
// replays its grant. OverdraftAmount is as for opening.
type ExtendOperationAuthorizationParams struct {
	OperationID     string `json:"-"` // carried by the route path
	Ordinal         int64  `json:"ordinal"`
	Amount          int64  `json:"amount,string"`
	MinimumAmount   int64  `json:"minimum_amount,string"`
	OverdraftAmount int64  `json:"overdraft_amount,omitempty,string"`
}

// OperationAuthorizationExtension is one committed grant. AuthorizedAmount is
// the reservation's total after it: the opening Amount plus every grant.
type OperationAuthorizationExtension struct {
	OperationID      string `json:"operation_id"`
	Ordinal          int64  `json:"ordinal"`
	GrantedAmount    int64  `json:"granted_amount,string"`
	AuthorizedAmount int64  `json:"authorized_amount,string"`
	Replayed         bool   `json:"replayed"`
}

// ReleaseOperationAuthorizationParams releases an open reservation after the
// host proves the provider operation never happened. Any billing evidence or
// refusal refuses release.
type ReleaseOperationAuthorizationParams struct {
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

	// The host's reasons for refusing qualification (RefuseProviderBillingQualification).
	// They appear only on a ProviderBillingRefusal.

	// ProviderBillingLifecycleUnprovable: the host cannot prove the provider
	// resource's lifecycle (its lifetime, absence and closed windows).
	ProviderBillingLifecycleUnprovable ProviderBillingQualificationReason = "lifecycle_unprovable"
	// ProviderBillingUnavailable: the provider reports no billing the host can
	// read for the resource.
	ProviderBillingUnavailable ProviderBillingQualificationReason = "provider_billing_unavailable"
	// ProviderBillingObservationRejected: OpenRails rejects the host's evidence
	// as invalid or conflicting, so it can never qualify.
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
// the provider adapter, in the authorization's currency. It is evidence, not a
// customer charge.
type ProviderBillingRecord struct {
	ProviderResourceID string    `json:"provider_resource_id"`
	BucketStart        time.Time `json:"bucket_start"`
	Amount             int64     `json:"amount,string"`
	TimeBilledMS       int64     `json:"time_billed_ms,string"`
}

type ProviderBillingEvidenceRefusalKind string

const (
	ProviderBillingRefusalSchemaAmbiguity  ProviderBillingEvidenceRefusalKind = "schema_ambiguity"
	ProviderBillingRefusalSubmicroAmount   ProviderBillingEvidenceRefusalKind = "submicro_amount"
	ProviderBillingRefusalAmountOverflow   ProviderBillingEvidenceRefusalKind = "amount_overflow"
	ProviderBillingRefusalResponseTooLarge ProviderBillingEvidenceRefusalKind = "response_too_large"
)

// ProviderBillingObservationRefusal is a typed adapter refusal. OpenRails
// persists it and never parses the raw provider body. RawBody is empty only for
// ProviderBillingRefusalResponseTooLarge.
type ProviderBillingObservationRefusal struct {
	Kind ProviderBillingEvidenceRefusalKind `json:"kind"`
}

// RecordProviderBillingObservationParams appends one immutable provider billing read.
// It carries no rated amount; eligible evidence settles inside the same commit.
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
// qualifies for settlement. QualifiedCostAmount is in the authorization's
// currency, null until eligible. Resolution is null unless an operator closed
// the hold (as is Authorization.Resolution).
type ProviderBillingQualification struct {
	OperationID             string                             `json:"operation_id"`
	MerchantID              MerchantID                         `json:"merchant_id"`
	Lifecycle               ProviderBillingLifecycleEvidence   `json:"lifecycle"`
	LifecycleEvidenceSHA256 SHA256                             `json:"lifecycle_evidence_sha256"`
	QuiescenceSeconds       int64                              `json:"quiescence_seconds"`
	State                   ProviderBillingQualificationState  `json:"state"`
	Reason                  ProviderBillingQualificationReason `json:"reason"`
	BaselineObservationID   string                             `json:"baseline_observation_id"`
	QualifiedObservationID  string                             `json:"qualified_observation_id"`
	QualifiedCostAmount     *int64                             `json:"qualified_cost_amount,string"`
	QualifiedAt             *time.Time                         `json:"qualified_at"`
	Resolution              *ProviderBillingResolution         `json:"resolution"`
	Authorization           OperationAuthorization             `json:"authorization"`
	CreatedAt               time.Time                          `json:"created_at"`
	UpdatedAt               time.Time                          `json:"updated_at"`
	Replayed                bool                               `json:"replayed"`
}

// ProviderBillingQualificationListParams filters ListProviderBillingQualifications.
// An empty filter admits every value; State refused with AuthorizationState
// open lists the holds only an operator can close.
type ProviderBillingQualificationListParams struct {
	PageRequest
	State              []ProviderBillingQualificationState
	AuthorizationState []OperationAuthorizationState
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

// ResolveProviderBillingQualificationParams is CloseOperationAuthorizationParams
// for ResolveProviderBillingQualification, which answers the hold's
// qualification and so needs one.
type ResolveProviderBillingQualificationParams struct {
	OperationID string                        `json:"-"` // carried by the route path
	Kind        ProviderBillingResolutionKind `json:"kind"`
	CostAmount  *int64                        `json:"cost_amount,string"`
	AttestedBy  string                        `json:"attested_by"` // at most 255 bytes
	Reference   string                        `json:"reference"`   // at most 1024 bytes
	Note        string                        `json:"note"`        // at most 4096 bytes
}

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

// RefuseProviderBillingQualificationParams records that the host cannot qualify
// an open authorization's provider cost, so automatic settlement never will:
// Reason is ProviderBillingLifecycleUnprovable, ProviderBillingUnavailable or
// ProviderBillingObservationRejected, Detail an optional canonical note (at
// most 4096 bytes). The hold then accepts no observation, extension or release;
// it waits for CloseOperationAuthorization. Repeating the same refusal replays.
type RefuseProviderBillingQualificationParams struct {
	OperationID string                             `json:"-"` // carried by the route path
	Reason      ProviderBillingQualificationReason `json:"reason"`
	Detail      string                             `json:"detail"`
}

// ProviderBillingRefusal records that an operation's provider cost will not
// qualify automatically: refused by OpenRails' qualifier (Reason is the
// qualification's) or by the host. Its hold waits for an operator. Immutable.
type ProviderBillingRefusal struct {
	Reason    ProviderBillingQualificationReason `json:"reason"`
	Detail    string                             `json:"detail"`
	RefusedAt time.Time                          `json:"refused_at"`
}

// CloseOperationAuthorizationParams closes a refused hold on an operator's
// attestation, since automatic settlement never will. Kind settled charges
// CostAmount (in the authorization's currency) under the pass-through contract,
// above the hold as owed; written_off releases the hold without charging the
// customer, and CostAmount is null. AttestedBy names the operator and Reference
// the evidence (a provider invoice or ticket), both opaque and canonical; Note
// is optional. Repeating the same close replays; a changed term is refused.
type CloseOperationAuthorizationParams struct {
	OperationID string                        `json:"-"` // carried by the route path
	Kind        ProviderBillingResolutionKind `json:"kind"`
	CostAmount  *int64                        `json:"cost_amount,string"`
	AttestedBy  string                        `json:"attested_by"` // at most 255 bytes
	Reference   string                        `json:"reference"`   // at most 1024 bytes
	Note        string                        `json:"note"`        // at most 4096 bytes
}

// OperationAuthorizationListParams filters ListOperationAuthorizations. An empty
// State admits every state and a nil Refused both; Refused true with State open
// lists the holds waiting for an operator.
type OperationAuthorizationListParams struct {
	PageRequest
	State   []OperationAuthorizationState
	Refused *bool
}
