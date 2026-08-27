package service

import (
	"context"
	"crypto/sha256"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/merchant"
)

type ProviderBillingQualificationState = money.ProviderBillingQualificationState
type ProviderBillingQualificationReason = money.ProviderBillingQualificationReason
type ProviderBillingLifecycleEvidence = money.ProviderBillingLifecycleEvidence
type ProviderBillingRecord = money.ProviderBillingRecord
type ProviderBillingEvidenceRefusalKind = money.ProviderBillingEvidenceRefusalKind
type ProviderBillingObservationRefusal = money.ProviderBillingObservationRefusal
type ProviderBillingObservationRequest = money.ProviderBillingObservationInput
type ProviderBillingObservationConflict = money.ProviderBillingObservationConflict

const (
	ProviderBillingQualificationPending  = money.ProviderBillingQualificationPending
	ProviderBillingQualificationRefused  = money.ProviderBillingQualificationRefused
	ProviderBillingQualificationEligible = money.ProviderBillingQualificationEligible

	ProviderBillingAwaitingEqualObservation = money.ProviderBillingAwaitingEqualObservation
	ProviderBillingAwaitingQuiescence       = money.ProviderBillingAwaitingQuiescence
	ProviderBillingCoverageIncomplete       = money.ProviderBillingCoverageIncomplete
	ProviderBillingObservationChanged       = money.ProviderBillingObservationChanged
	ProviderBillingProviderEvidenceRefused  = money.ProviderBillingProviderEvidenceRefused
	ProviderBillingNegativeOrCorrective     = money.ProviderBillingNegativeOrCorrective
	ProviderBillingDecreasingProviderCost   = money.ProviderBillingDecreasingProviderCost
	ProviderBillingEligible                 = money.ProviderBillingEligible

	ProviderBillingRefusalSchemaAmbiguity  = money.ProviderBillingRefusalSchemaAmbiguity
	ProviderBillingRefusalSubmicroAmount   = money.ProviderBillingRefusalSubmicroAmount
	ProviderBillingRefusalAmountOverflow   = money.ProviderBillingRefusalAmountOverflow
	ProviderBillingRefusalResponseTooLarge = money.ProviderBillingRefusalResponseTooLarge
)

var (
	ErrProviderBillingObservationConflict   = money.ErrProviderBillingObservationConflict
	ErrProviderBillingQualificationRefused  = money.ErrProviderBillingQualificationRefused
	ErrProviderBillingQualificationNotFound = money.ErrProviderBillingQualificationNotFound
)

type ProviderBillingQualification struct {
	OperationID                    string
	MerchantID                     uuid.UUID
	Provider                       string
	ProviderResourceID             string
	ProviderLifetimeStart          time.Time
	ProviderLifetimeEnd            time.Time
	ProviderAbsentAt               time.Time
	ProviderAbsenceReference       string
	BillingStopReference           string
	WindowsClosedAt                time.Time
	WindowsClosedReference         string
	LifecycleEvidenceBody          []byte
	LifecycleEvidenceSHA256        [sha256.Size]byte
	Quiescence                     time.Duration
	State                          ProviderBillingQualificationState
	Reason                         ProviderBillingQualificationReason
	BaselineObservationID          string
	QualifiedObservationID         string
	QualifiedProviderCostUSDMicros *int64
	QualifiedAt                    *time.Time
	Authorization                  *OperationAuthorization
	CreatedAt                      time.Time
	UpdatedAt                      time.Time
	Replayed                       bool
}

// RecordProviderBillingObservationTx records exact provider/lifecycle facts
// and lets OpenRails alone qualify and settle them in the caller-owned
// transaction. It never calls a provider and accepts no caller-rated amount.
func (s *Service) RecordProviderBillingObservationTx(ctx context.Context, tx pgx.Tx, req ProviderBillingObservationRequest) (*ProviderBillingQualification, error) {
	rt, err := s.runtime()
	if err != nil {
		return nil, err
	}
	merchantID, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	if rt.Config == nil {
		return nil, fmt.Errorf("provider billing qualification requires runtime config")
	}
	quiescence, err := rt.Config.ProviderBillingQuiescence()
	if err != nil {
		return nil, err
	}
	ctx, txDB, err := rt.DB.BindMerchantTx(ctx, tx, merchantID)
	if err != nil {
		return nil, err
	}
	result, err := s.moneyService().RecordProviderBillingObservationInTx(ctx, txDB, req, quiescence)
	if err != nil {
		return nil, err
	}
	return providerBillingQualificationFromMoney(result), nil
}

func (s *Service) GetProviderBillingQualification(ctx context.Context, operationID string) (*ProviderBillingQualification, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	result, err := s.moneyService().GetProviderBillingQualification(ctx, operationID)
	if err != nil {
		return nil, err
	}
	return providerBillingQualificationFromMoney(result), nil
}

func providerBillingQualificationFromMoney(result *money.ProviderBillingQualification) *ProviderBillingQualification {
	if result == nil {
		return nil
	}
	return &ProviderBillingQualification{
		OperationID:                    result.OperationID,
		MerchantID:                     result.MerchantID,
		Provider:                       result.Provider,
		ProviderResourceID:             result.ProviderResourceID,
		ProviderLifetimeStart:          result.ProviderLifetimeStart,
		ProviderLifetimeEnd:            result.ProviderLifetimeEnd,
		ProviderAbsentAt:               result.ProviderAbsentAt,
		ProviderAbsenceReference:       result.ProviderAbsenceReference,
		BillingStopReference:           result.BillingStopReference,
		WindowsClosedAt:                result.WindowsClosedAt,
		WindowsClosedReference:         result.WindowsClosedReference,
		LifecycleEvidenceBody:          result.LifecycleEvidenceBody,
		LifecycleEvidenceSHA256:        result.LifecycleEvidenceSHA256,
		Quiescence:                     result.Quiescence,
		State:                          result.State,
		Reason:                         result.Reason,
		BaselineObservationID:          result.BaselineObservationID,
		QualifiedObservationID:         result.QualifiedObservationID,
		QualifiedProviderCostUSDMicros: result.QualifiedProviderCostUSDMicros,
		QualifiedAt:                    result.QualifiedAt,
		Authorization:                  operationAuthorizationFromMoney(result.Authorization),
		CreatedAt:                      result.CreatedAt,
		UpdatedAt:                      result.UpdatedAt,
		Replayed:                       result.Replayed,
	}
}
