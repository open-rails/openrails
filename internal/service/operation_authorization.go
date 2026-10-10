package service

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/money"
)

// Provider-operation commands exist in two forms with identical semantics: the
// plain form commits its own merchant transaction (the Client routes), and the
// Tx form rides a transaction owned by an embedding host, which alone commits
// or rolls it back. Every command answers the operation.

// inOperationTx runs fn in a merchant transaction of its own.
func (s *Service) inOperationTx(ctx context.Context, fn func(context.Context, pgx.Tx) (*billing.ProviderOperation, error)) (*billing.ProviderOperation, error) {
	rt, err := s.runtime()
	if err != nil {
		return nil, err
	}
	var out *billing.ProviderOperation
	err = rt.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		out, err = fn(ctx, tx)
		return err
	})
	return out, err
}

// bindOperationTx binds the merchant to the host's tx and runs fn on it.
func (s *Service) bindOperationTx(ctx context.Context, tx pgx.Tx, fn func(context.Context, *db.DB) (*money.OperationAuthorization, error)) (*billing.ProviderOperation, error) {
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
	auth, err := fn(ctx, txDB)
	if err != nil {
		return nil, err
	}
	return providerOperationFromMoney(auth), nil
}

func (s *Service) OpenProviderOperation(ctx context.Context, req billing.OpenProviderOperationParams) (*billing.ProviderOperation, error) {
	return s.inOperationTx(ctx, func(ctx context.Context, tx pgx.Tx) (*billing.ProviderOperation, error) {
		return s.OpenProviderOperationTx(ctx, tx, req)
	})
}

func (s *Service) OpenProviderOperationTx(ctx context.Context, tx pgx.Tx, req billing.OpenProviderOperationParams) (*billing.ProviderOperation, error) {
	return s.bindOperationTx(ctx, tx, func(ctx context.Context, txDB *db.DB) (*money.OperationAuthorization, error) {
		return s.moneyService().OpenOperationAuthorizationInTx(ctx, txDB, money.OperationAuthorizationInput{
			OperationID:             req.OperationID,
			CustomerID:              identity.CustomerID(req.CustomerID),
			RecordOwner:             req.RecordOwner,
			Currency:                req.Currency,
			Amount:                  req.Amount,
			ClaimReference:          req.ClaimReference,
			AuthorizationBody:       req.AuthorizationBody,
			AuthorizationBodySHA256: req.AuthorizationBodySHA256,
			OverdraftAmount:         req.OverdraftAmount,
		})
	})
}

func (s *Service) GetProviderOperation(ctx context.Context, operationID string) (*billing.ProviderOperation, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	auth, err := s.moneyService().GetOperationAuthorization(ctx, operationID)
	if err != nil {
		return nil, err
	}
	return providerOperationFromMoney(auth), nil
}

func (s *Service) GetProviderOperationTx(ctx context.Context, tx pgx.Tx, operationID string) (*billing.ProviderOperation, error) {
	return s.bindOperationTx(ctx, tx, func(ctx context.Context, txDB *db.DB) (*money.OperationAuthorization, error) {
		return s.moneyService().GetOperationAuthorizationInTx(ctx, txDB, operationID)
	})
}

func (s *Service) IncrementProviderOperation(ctx context.Context, req billing.IncrementProviderOperationParams) (*billing.ProviderOperation, error) {
	return s.inOperationTx(ctx, func(ctx context.Context, tx pgx.Tx) (*billing.ProviderOperation, error) {
		return s.IncrementProviderOperationTx(ctx, tx, req)
	})
}

func (s *Service) IncrementProviderOperationTx(ctx context.Context, tx pgx.Tx, req billing.IncrementProviderOperationParams) (*billing.ProviderOperation, error) {
	return s.bindOperationTx(ctx, tx, func(ctx context.Context, txDB *db.DB) (*money.OperationAuthorization, error) {
		return s.moneyService().ExtendOperationAuthorizationInTx(ctx, txDB, money.OperationAuthorizationExtensionInput{
			OperationID: req.OperationID, Ordinal: req.Ordinal, Amount: req.Amount, MinimumAmount: req.MinimumAmount,
			OverdraftAmount: req.OverdraftAmount,
		})
	})
}

func (s *Service) ReleaseProviderOperation(ctx context.Context, req billing.ReleaseProviderOperationParams) (*billing.ProviderOperation, error) {
	return s.inOperationTx(ctx, func(ctx context.Context, tx pgx.Tx) (*billing.ProviderOperation, error) {
		return s.ReleaseProviderOperationTx(ctx, tx, req)
	})
}

func (s *Service) ReleaseProviderOperationTx(ctx context.Context, tx pgx.Tx, req billing.ReleaseProviderOperationParams) (*billing.ProviderOperation, error) {
	return s.bindOperationTx(ctx, tx, func(ctx context.Context, txDB *db.DB) (*money.OperationAuthorization, error) {
		return s.moneyService().ReleaseOperationAuthorizationInTx(ctx, txDB, req.OperationID, req.ReleaseReference)
	})
}

// RecordProviderBillingObservation records exact provider/lifecycle facts, or
// the host's refusal to produce them, in an OpenRails-owned transaction.
// OpenRails alone qualifies, rates, and settles.
func (s *Service) RecordProviderBillingObservation(ctx context.Context, req billing.RecordProviderBillingObservationParams) (*billing.ProviderOperation, error) {
	return s.inOperationTx(ctx, func(ctx context.Context, tx pgx.Tx) (*billing.ProviderOperation, error) {
		return s.RecordProviderBillingObservationTx(ctx, tx, req)
	})
}

// RecordProviderBillingObservationTx is the host-transaction form. It never
// calls a provider and accepts no caller-rated amount.
func (s *Service) RecordProviderBillingObservationTx(ctx context.Context, tx pgx.Tx, req billing.RecordProviderBillingObservationParams) (*billing.ProviderOperation, error) {
	rt, err := s.runtime()
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
	return s.bindOperationTx(ctx, tx, func(ctx context.Context, txDB *db.DB) (*money.OperationAuthorization, error) {
		return s.moneyService().RecordProviderBillingObservationInTx(ctx, txDB, req, quiescence)
	})
}

// CloseProviderOperation closes a refused hold on an operator's attestation in
// an OpenRails-owned transaction.
func (s *Service) CloseProviderOperation(ctx context.Context, req billing.CloseProviderOperationParams) (*billing.ProviderOperation, error) {
	return s.inOperationTx(ctx, func(ctx context.Context, tx pgx.Tx) (*billing.ProviderOperation, error) {
		return s.CloseProviderOperationTx(ctx, tx, req)
	})
}

// CloseProviderOperationTx is the host-transaction form.
func (s *Service) CloseProviderOperationTx(ctx context.Context, tx pgx.Tx, req billing.CloseProviderOperationParams) (*billing.ProviderOperation, error) {
	return s.bindOperationTx(ctx, tx, func(ctx context.Context, txDB *db.DB) (*money.OperationAuthorization, error) {
		return s.moneyService().CloseOperationAuthorizationInTx(ctx, txDB, req)
	})
}

// ListProviderOperations pages operations, newest first.
func (s *Service) ListProviderOperations(ctx context.Context, filter billing.ProviderOperationListParams) (*billing.ListPage[billing.ProviderOperation], error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	page, err := s.moneyService().ListOperationAuthorizations(ctx, filter)
	if err != nil {
		return nil, err
	}
	out := billing.ListPage[billing.ProviderOperation]{Items: make([]billing.ProviderOperation, len(page.Items)), Next: page.Next}
	for i, item := range page.Items {
		out.Items[i] = *providerOperationFromMoney(item)
	}
	return &out, nil
}

func providerOperationFromMoney(auth *money.OperationAuthorization) *billing.ProviderOperation {
	out := &billing.ProviderOperation{
		OperationID:             auth.OperationID,
		MerchantID:              billing.MerchantID(auth.MerchantID),
		CustomerID:              billing.CustomerID(auth.CustomerID),
		RecordOwner:             auth.RecordOwner,
		Currency:                auth.Currency,
		Amount:                  auth.Amount,
		AuthorizedAmount:        auth.AuthorizedAmount,
		ClaimReference:          auth.ClaimReference,
		AuthorizationBody:       auth.AuthorizationBody,
		AuthorizationBodySHA256: auth.AuthorizationBodySHA256,
		State:                   billing.ProviderOperationState(auth.State),
		TerminalReference:       auth.TerminalReference,
		SettlementCostAmount:    auth.SettlementCostAmount,
		SettlementAmount:        auth.SettlementAmount,
		CreatedAt:               auth.CreatedAt,
		ReleasedAt:              auth.ReleasedAt,
		SettledAt:               auth.SettledAt,
		Replayed:                auth.Replayed,
	}
	if auth.State == money.OperationAuthorizationSettled {
		digest := billing.SHA256(auth.SettlementBodySHA256)
		out.SettlementBody = auth.SettlementBody
		out.SettlementBodySHA256 = &digest
	}
	if inc := auth.LastIncrement; inc != nil {
		out.LastIncrement = &billing.ProviderOperationIncrement{
			Ordinal: inc.Ordinal, Amount: inc.RequestedAmount, MinimumAmount: inc.MinimumAmount,
			GrantedAmount: inc.GrantedAmount, CreatedAt: inc.CreatedAt,
		}
	}
	if q := auth.Qualification; q != nil {
		out.Qualification = &billing.ProviderBillingQualification{
			Lifecycle: billing.ProviderBillingLifecycleEvidence{
				Provider:                 q.Provider,
				ProviderResourceID:       q.ProviderResourceID,
				ProviderLifetimeStartsAt: q.ProviderLifetimeStartsAt,
				ProviderLifetimeEndsAt:   q.ProviderLifetimeEndsAt,
				ProviderAbsentAt:         q.ProviderAbsentAt,
				ProviderAbsenceReference: q.ProviderAbsenceReference,
				BillingStopReference:     q.BillingStopReference,
				WindowsClosedAt:          q.WindowsClosedAt,
				WindowsClosedReference:   q.WindowsClosedReference,
				LifecycleEvidenceBody:    q.LifecycleEvidenceBody,
			},
			LifecycleEvidenceSHA256: q.LifecycleEvidenceSHA256,
			QuiescenceSeconds:       int64(q.Quiescence.Seconds()),
			State:                   billing.ProviderBillingQualificationState(q.State),
			Reason:                  billing.ProviderBillingQualificationReason(q.Reason),
			BaselineObservationID:   q.BaselineObservationID,
			QualifiedObservationID:  q.QualifiedObservationID,
			QualifiedCostAmount:     q.QualifiedCostAmount,
			QualifiedAt:             q.QualifiedAt,
			CreatedAt:               q.CreatedAt,
			UpdatedAt:               q.UpdatedAt,
		}
	}
	if r := auth.Refusal; r != nil {
		out.Refusal = &billing.ProviderBillingRefusal{Reason: billing.ProviderBillingQualificationReason(r.Reason), Detail: r.Detail, RefusedAt: r.RefusedAt}
	}
	if r := auth.Resolution; r != nil {
		out.Resolution = &billing.ProviderBillingResolution{
			Kind: r.Kind, CostAmount: r.CostAmount, AttestedBy: r.AttestedBy, Reference: r.Reference, Note: r.Note, ResolvedAt: r.ResolvedAt,
		}
	}
	return out
}
