package openrails

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Provider obligations reserve customer capacity for one provider operation and
// settle it from immutable provider observations. Amounts are exact integer USD
// micros. Callers never supply a rated customer amount: OpenRails qualifies the
// evidence, rates it, and posts the one final settlement.

// ProviderBillingObservationMaxBytes bounds the canonical JSON encoding of one
// ProviderBillingObservationRequest in every deployment, so a request accepted
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

// OperationAuthorizationRequest is exact host-authored authority for one
// provider operation. OperationID is also the provider operation's idempotency
// identity. OpenRails verifies the digest but never parses AuthorizationBody.
type OperationAuthorizationRequest struct {
	OperationID             string     `json:"operation_id"` // canonical, at most 255 bytes
	Payer                   CustomerID `json:"payer"`
	RecordOwner             string     `json:"record_owner"` // canonical, at most 255 bytes
	AuthorizedUSDMicros     int64      `json:"authorized_usd_micros,string"`
	ClaimReference          string     `json:"claim_reference"`    // canonical, at most 1024 bytes
	AuthorizationBody       []byte     `json:"authorization_body"` // exact bytes, 1..65536
	AuthorizationBodySHA256 SHA256     `json:"authorization_body_sha256"`
}

// OperationAuthorization is the durable reservation. Settlement fields are
// null until OpenRails settles qualified provider evidence.
type OperationAuthorization struct {
	OperationID                     string                      `json:"operation_id"`
	MerchantID                      uuid.UUID                   `json:"merchant_id"`
	Payer                           CustomerID                  `json:"payer"`
	RecordOwner                     string                      `json:"record_owner"`
	AuthorizedUSDMicros             int64                       `json:"authorized_usd_micros,string"`
	ClaimReference                  string                      `json:"claim_reference"`
	AuthorizationBody               []byte                      `json:"authorization_body"`
	AuthorizationBodySHA256         SHA256                      `json:"authorization_body_sha256"`
	State                           OperationAuthorizationState `json:"state"`
	TerminalReference               string                      `json:"terminal_reference"`
	SettlementProviderCostUSDMicros *int64                      `json:"settlement_provider_cost_usd_micros,string"`
	SettlementRatedUSDMicros        *int64                      `json:"settlement_rated_usd_micros,string"`
	SettlementBody                  []byte                      `json:"settlement_body"`
	SettlementBodySHA256            *SHA256                     `json:"settlement_body_sha256"`
	CreatedAt                       time.Time                   `json:"created_at"`
	ReleasedAt                      *time.Time                  `json:"released_at"`
	SettledAt                       *time.Time                  `json:"settled_at"`
	Replayed                        bool                        `json:"replayed"`
}

// ReleaseOperationAuthorizationRequest releases an open reservation after the
// host proves the provider operation never happened. Any billing evidence
// refuses release.
type ReleaseOperationAuthorizationRequest struct {
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
)

// ProviderBillingLifecycleEvidence is immutable per operation: the first
// observation fixes it and later observations must repeat it exactly.
type ProviderBillingLifecycleEvidence struct {
	Provider                 string    `json:"provider"`
	ProviderResourceID       string    `json:"provider_resource_id"`
	ProviderLifetimeStart    time.Time `json:"provider_lifetime_start"`
	ProviderLifetimeEnd      time.Time `json:"provider_lifetime_end"`
	ProviderAbsentAt         time.Time `json:"provider_absent_at"`
	ProviderAbsenceReference string    `json:"provider_absence_reference"`
	BillingStopReference     string    `json:"billing_stop_reference"`
	WindowsClosedAt          time.Time `json:"windows_closed_at"`
	WindowsClosedReference   string    `json:"windows_closed_reference"`
	LifecycleEvidenceBody    []byte    `json:"lifecycle_evidence_body"`
}

// ProviderBillingRecord is one provider-reported cost bucket, decoded exactly by
// the provider adapter. It is evidence, not a customer charge.
type ProviderBillingRecord struct {
	ProviderResourceID string    `json:"provider_resource_id"`
	BucketStart        time.Time `json:"bucket_start"`
	AmountUSDMicros    int64     `json:"amount_usd_micros,string"`
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

// ProviderBillingObservationRequest appends one immutable provider billing read.
// It carries no rated amount; eligible evidence settles inside the same commit.
type ProviderBillingObservationRequest struct {
	OperationID     string                             `json:"-"` // carried by the route path
	ObservationID   string                             `json:"observation_id"`
	Lifecycle       ProviderBillingLifecycleEvidence   `json:"lifecycle"`
	NormalizedQuery string                             `json:"normalized_query"`
	QueryStart      time.Time                          `json:"query_start"`
	QueryEnd        time.Time                          `json:"query_end"`
	RawBody         []byte                             `json:"raw_body"`
	Records         []ProviderBillingRecord            `json:"records"`
	Refusal         *ProviderBillingObservationRefusal `json:"refusal"`
}

type ProviderBillingQualification struct {
	OperationID                    string                             `json:"operation_id"`
	MerchantID                     uuid.UUID                          `json:"merchant_id"`
	Lifecycle                      ProviderBillingLifecycleEvidence   `json:"lifecycle"`
	LifecycleEvidenceSHA256        SHA256                             `json:"lifecycle_evidence_sha256"`
	QuiescenceSeconds              int64                              `json:"quiescence_seconds"`
	State                          ProviderBillingQualificationState  `json:"state"`
	Reason                         ProviderBillingQualificationReason `json:"reason"`
	BaselineObservationID          string                             `json:"baseline_observation_id"`
	QualifiedObservationID         string                             `json:"qualified_observation_id"`
	QualifiedProviderCostUSDMicros *int64                             `json:"qualified_provider_cost_usd_micros,string"`
	QualifiedAt                    *time.Time                         `json:"qualified_at"`
	Authorization                  OperationAuthorization             `json:"authorization"`
	CreatedAt                      time.Time                          `json:"created_at"`
	UpdatedAt                      time.Time                          `json:"updated_at"`
	Replayed                       bool                               `json:"replayed"`
}
