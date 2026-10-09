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

// ResolveProviderBillingQualification is CloseOperationAuthorization answered as
// the hold's qualification, in an OpenRails-owned transaction.
func (s *Service) ResolveProviderBillingQualification(ctx context.Context, req billing.ResolveProviderBillingQualificationParams) (*billing.ProviderBillingQualification, error) {
	rt, err := s.runtime()
	if err != nil {
		return nil, err
	}
	var out *billing.ProviderBillingQualification
	err = rt.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		out, err = s.ResolveProviderBillingQualificationTx(ctx, tx, req)
		return err
	})
	return out, err
}

// ResolveProviderBillingQualificationTx is the host-transaction form.
func (s *Service) ResolveProviderBillingQualificationTx(ctx context.Context, tx pgx.Tx, req billing.ResolveProviderBillingQualificationParams) (*billing.ProviderBillingQualification, error) {
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
	result, err := s.moneyService().ResolveProviderBillingQualificationInTx(ctx, txDB, req)
	if err != nil {
		return nil, err
	}
	return providerBillingQualificationFromMoney(result), nil
}

// ListProviderBillingQualifications pages qualifications, newest first.
func (s *Service) ListProviderBillingQualifications(ctx context.Context, filter billing.ProviderBillingQualificationListParams) (*billing.ListPage[billing.ProviderBillingQualification], error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	page, err := s.moneyService().ListProviderBillingQualifications(ctx, filter)
	if err != nil {
		return nil, err
	}
	out := billing.ListPage[billing.ProviderBillingQualification]{Items: make([]billing.ProviderBillingQualification, len(page.Items)), Next: page.Next}
	for i, item := range page.Items {
		out.Items[i] = *providerBillingQualificationFromMoney(item)
	}
	return &out, nil
}

func providerBillingQualificationFromMoney(result *money.ProviderBillingQualification) *billing.ProviderBillingQualification {
	resolution := providerBillingResolutionFromMoney(result.Resolution)
	return &billing.ProviderBillingQualification{
		OperationID: result.OperationID,
		MerchantID:  billing.MerchantID(result.MerchantID),
		Lifecycle: billing.ProviderBillingLifecycleEvidence{
			Provider:                 result.Provider,
			ProviderResourceID:       result.ProviderResourceID,
			ProviderLifetimeStartsAt: result.ProviderLifetimeStartsAt,
			ProviderLifetimeEndsAt:   result.ProviderLifetimeEndsAt,
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
		Resolution:              resolution,
		Authorization:           *operationAuthorizationFromMoney(result.Authorization),
		CreatedAt:               result.CreatedAt,
		UpdatedAt:               result.UpdatedAt,
		Replayed:                result.Replayed,
	}
}
