package service

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/money"
)

// RecordProviderBillingObservation records exact provider/lifecycle facts in an
// OpenRails-owned transaction. OpenRails alone qualifies, rates, and settles.
func (s *Service) RecordProviderBillingObservation(ctx context.Context, req billing.RecordProviderBillingObservationParams) (*billing.ProviderBillingQualification, error) {
	rt, err := s.runtime()
	if err != nil {
		return nil, err
	}
	var out *billing.ProviderBillingQualification
	err = rt.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		out, err = s.RecordProviderBillingObservationTx(ctx, tx, req)
		return err
	})
	return out, err
}

// RecordProviderBillingObservationTx is the host-transaction form. It never
// calls a provider and accepts no caller-rated amount.
func (s *Service) RecordProviderBillingObservationTx(ctx context.Context, tx pgx.Tx, req billing.RecordProviderBillingObservationParams) (*billing.ProviderBillingQualification, error) {
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
	quiescence, err := config.ProviderBillingQuiescence(rt.Config)
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

func (s *Service) GetProviderBillingQualification(ctx context.Context, operationID string) (*billing.ProviderBillingQualification, error) {
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

func (s *Service) GetProviderBillingQualificationTx(ctx context.Context, tx pgx.Tx, operationID string) (*billing.ProviderBillingQualification, error) {
	rt, err := s.runtime()
	if err != nil {
		return nil, err
	}
	merchantID, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	ctx, txDB, err := rt.DB.BindMerchantTx(ctx, tx, merchantID)
	if err != nil {
		return nil, err
	}
	result, err := s.moneyService().GetProviderBillingQualificationInTx(ctx, txDB, operationID)
	if err != nil {
		return nil, err
	}
	return providerBillingQualificationFromMoney(result), nil
}

func providerBillingQualificationFromMoney(result *money.ProviderBillingQualification) *billing.ProviderBillingQualification {
	return &billing.ProviderBillingQualification{
		OperationID: result.OperationID,
		MerchantID:  billing.MerchantID(result.MerchantID),
		Lifecycle: billing.ProviderBillingLifecycleEvidence{
			Provider:                 result.Provider,
			ProviderResourceID:       result.ProviderResourceID,
			ProviderLifetimeStart:    result.ProviderLifetimeStart,
			ProviderLifetimeEnd:      result.ProviderLifetimeEnd,
			ProviderAbsentAt:         result.ProviderAbsentAt,
			ProviderAbsenceReference: result.ProviderAbsenceReference,
			BillingStopReference:     result.BillingStopReference,
			WindowsClosedAt:          result.WindowsClosedAt,
			WindowsClosedReference:   result.WindowsClosedReference,
			LifecycleEvidenceBody:    result.LifecycleEvidenceBody,
		},
		LifecycleEvidenceSHA256: result.LifecycleEvidenceSHA256,
		QuiescenceSeconds:       int64(result.Quiescence.Seconds()),
		State:                   billing.ProviderBillingQualificationState(result.State),
		Reason:                  billing.ProviderBillingQualificationReason(result.Reason),
		BaselineObservationID:   result.BaselineObservationID,
		QualifiedObservationID:  result.QualifiedObservationID,
		QualifiedCostAmount:     result.QualifiedCostAmount,
		QualifiedAt:             result.QualifiedAt,
		Authorization:           *operationAuthorizationFromMoney(result.Authorization),
		CreatedAt:               result.CreatedAt,
		UpdatedAt:               result.UpdatedAt,
		Replayed:                result.Replayed,
	}
}
