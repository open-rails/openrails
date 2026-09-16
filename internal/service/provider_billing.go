package service

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/merchant"
)

// RecordProviderBillingObservation records exact provider/lifecycle facts in an
// OpenRails-owned transaction. OpenRails alone qualifies, rates, and settles.
func (s *Service) RecordProviderBillingObservation(ctx context.Context, req openrails.ProviderBillingObservationRequest) (*openrails.ProviderBillingQualification, error) {
	rt, err := s.runtime()
	if err != nil {
		return nil, err
	}
	var out *openrails.ProviderBillingQualification
	err = rt.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		out, err = s.RecordProviderBillingObservationTx(ctx, tx, req)
		return err
	})
	return out, err
}

// RecordProviderBillingObservationTx is the host-transaction form. It never
// calls a provider and accepts no caller-rated amount.
func (s *Service) RecordProviderBillingObservationTx(ctx context.Context, tx pgx.Tx, req openrails.ProviderBillingObservationRequest) (*openrails.ProviderBillingQualification, error) {
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

func (s *Service) GetProviderBillingQualification(ctx context.Context, operationID string) (*openrails.ProviderBillingQualification, error) {
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

func (s *Service) GetProviderBillingQualificationTx(ctx context.Context, tx pgx.Tx, operationID string) (*openrails.ProviderBillingQualification, error) {
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

func providerBillingQualificationFromMoney(result *money.ProviderBillingQualification) *openrails.ProviderBillingQualification {
	return &openrails.ProviderBillingQualification{
		OperationID: result.OperationID,
		MerchantID:  result.MerchantID,
		Lifecycle: openrails.ProviderBillingLifecycleEvidence{
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
		LifecycleEvidenceSHA256:        result.LifecycleEvidenceSHA256,
		QuiescenceSeconds:              int64(result.Quiescence.Seconds()),
		State:                          openrails.ProviderBillingQualificationState(result.State),
		Reason:                         openrails.ProviderBillingQualificationReason(result.Reason),
		BaselineObservationID:          result.BaselineObservationID,
		QualifiedObservationID:         result.QualifiedObservationID,
		QualifiedProviderCostUSDMicros: result.QualifiedProviderCostUSDMicros,
		QualifiedAt:                    result.QualifiedAt,
		Authorization:                  *operationAuthorizationFromMoney(result.Authorization),
		CreatedAt:                      result.CreatedAt,
		UpdatedAt:                      result.UpdatedAt,
		Replayed:                       result.Replayed,
	}
}
