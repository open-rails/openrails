package embed

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/pkg/service"
)

type ProviderBillingQualificationState = service.ProviderBillingQualificationState
type ProviderBillingQualificationReason = service.ProviderBillingQualificationReason
type ProviderBillingLifecycleEvidence = service.ProviderBillingLifecycleEvidence
type ProviderBillingRecord = service.ProviderBillingRecord
type ProviderBillingEvidenceRefusalKind = service.ProviderBillingEvidenceRefusalKind
type ProviderBillingObservationRefusal = service.ProviderBillingObservationRefusal
type ProviderBillingObservationRequest = service.ProviderBillingObservationRequest
type ProviderBillingObservationConflict = service.ProviderBillingObservationConflict
type ProviderBillingQualification = service.ProviderBillingQualification

const (
	ProviderBillingQualificationPending  = service.ProviderBillingQualificationPending
	ProviderBillingQualificationRefused  = service.ProviderBillingQualificationRefused
	ProviderBillingQualificationEligible = service.ProviderBillingQualificationEligible

	ProviderBillingAwaitingEqualObservation = service.ProviderBillingAwaitingEqualObservation
	ProviderBillingAwaitingQuiescence       = service.ProviderBillingAwaitingQuiescence
	ProviderBillingCoverageIncomplete       = service.ProviderBillingCoverageIncomplete
	ProviderBillingObservationChanged       = service.ProviderBillingObservationChanged
	ProviderBillingProviderEvidenceRefused  = service.ProviderBillingProviderEvidenceRefused
	ProviderBillingNegativeOrCorrective     = service.ProviderBillingNegativeOrCorrective
	ProviderBillingDecreasingProviderCost   = service.ProviderBillingDecreasingProviderCost
	ProviderBillingEligible                 = service.ProviderBillingEligible

	ProviderBillingRefusalSchemaAmbiguity  = service.ProviderBillingRefusalSchemaAmbiguity
	ProviderBillingRefusalSubmicroAmount   = service.ProviderBillingRefusalSubmicroAmount
	ProviderBillingRefusalAmountOverflow   = service.ProviderBillingRefusalAmountOverflow
	ProviderBillingRefusalResponseTooLarge = service.ProviderBillingRefusalResponseTooLarge
)

var (
	ErrProviderBillingObservationConflict   = service.ErrProviderBillingObservationConflict
	ErrProviderBillingQualificationRefused  = service.ErrProviderBillingQualificationRefused
	ErrProviderBillingQualificationNotFound = service.ErrProviderBillingQualificationNotFound
)

// RecordProviderBillingObservationTx appends one provider-neutral read and
// lifecycle proof. OpenRails owns qualification, pass-through rating, and the
// final ledger settlement inside tx.
func (r *Runtime) RecordProviderBillingObservationTx(ctx context.Context, tx pgx.Tx, req ProviderBillingObservationRequest) (*ProviderBillingQualification, error) {
	ctx, err := r.operationAuthorizationContext(ctx)
	if err != nil {
		return nil, err
	}
	return r.svc.RecordProviderBillingObservationTx(ctx, tx, req)
}

// GetProviderBillingQualification reads durable pending/refused/eligible
// policy state. Eligible is not provider-attested finality.
func (r *Runtime) GetProviderBillingQualification(ctx context.Context, operationID string) (*ProviderBillingQualification, error) {
	ctx, err := r.operationAuthorizationContext(ctx)
	if err != nil {
		return nil, err
	}
	return r.svc.GetProviderBillingQualification(ctx, operationID)
}
