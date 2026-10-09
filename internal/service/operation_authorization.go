package service

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/money"
)

// Provider-operation commands exist in two forms with identical semantics: the
// plain form commits its own merchant transaction (the Client routes), and the
// Tx form rides a transaction owned by an embedding host, which alone commits
// or rolls it back.

func (s *Service) OpenOperationAuthorization(ctx context.Context, req billing.OpenOperationAuthorizationParams) (*billing.OperationAuthorization, error) {
	rt, err := s.runtime()
	if err != nil {
		return nil, err
	}
	var out *billing.OperationAuthorization
	err = rt.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		out, err = s.OpenOperationAuthorizationTx(ctx, tx, req)
		return err
	})
	return out, err
}

func (s *Service) OpenOperationAuthorizationTx(ctx context.Context, tx pgx.Tx, req billing.OpenOperationAuthorizationParams) (*billing.OperationAuthorization, error) {
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
	auth, err := s.moneyService().OpenOperationAuthorizationInTx(ctx, txDB, money.OperationAuthorizationInput{
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
	if err != nil {
		return nil, err
	}
	return operationAuthorizationFromMoney(auth), nil
}

func (s *Service) GetOperationAuthorization(ctx context.Context, operationID string) (*billing.OperationAuthorization, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	auth, err := s.moneyService().GetOperationAuthorization(ctx, operationID)
	if err != nil {
		return nil, err
	}
	return operationAuthorizationFromMoney(auth), nil
}

func (s *Service) GetOperationAuthorizationTx(ctx context.Context, tx pgx.Tx, operationID string) (*billing.OperationAuthorization, error) {
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
	auth, err := s.moneyService().GetOperationAuthorizationInTx(ctx, txDB, operationID)
	if err != nil {
		return nil, err
	}
	return operationAuthorizationFromMoney(auth), nil
}

func (s *Service) ExtendOperationAuthorization(ctx context.Context, req billing.ExtendOperationAuthorizationParams) (*billing.OperationAuthorizationExtension, error) {
	rt, err := s.runtime()
	if err != nil {
		return nil, err
	}
	var out *billing.OperationAuthorizationExtension
	err = rt.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		out, err = s.ExtendOperationAuthorizationTx(ctx, tx, req)
		return err
	})
	return out, err
}

func (s *Service) ExtendOperationAuthorizationTx(ctx context.Context, tx pgx.Tx, req billing.ExtendOperationAuthorizationParams) (*billing.OperationAuthorizationExtension, error) {
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
	ext, err := s.moneyService().ExtendOperationAuthorizationInTx(ctx, txDB, money.OperationAuthorizationExtensionInput{
		OperationID: req.OperationID, Ordinal: req.Ordinal, Amount: req.Amount, MinimumAmount: req.MinimumAmount,
		OverdraftAmount: req.OverdraftAmount,
	})
	if err != nil {
		return nil, err
	}
	return &billing.OperationAuthorizationExtension{
		OperationID: ext.OperationID, Ordinal: ext.Ordinal, GrantedAmount: ext.GrantedAmount,
		AuthorizedAmount: ext.AuthorizedAmount, Replayed: ext.Replayed,
	}, nil
}

func (s *Service) ReleaseOperationAuthorization(ctx context.Context, req billing.ReleaseOperationAuthorizationParams) (*billing.OperationAuthorization, error) {
	rt, err := s.runtime()
	if err != nil {
		return nil, err
	}
	var out *billing.OperationAuthorization
	err = rt.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		out, err = s.ReleaseOperationAuthorizationTx(ctx, tx, req)
		return err
	})
	return out, err
}

func (s *Service) ReleaseOperationAuthorizationTx(ctx context.Context, tx pgx.Tx, req billing.ReleaseOperationAuthorizationParams) (*billing.OperationAuthorization, error) {
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
	auth, err := s.moneyService().ReleaseOperationAuthorizationInTx(ctx, txDB, req.OperationID, req.ReleaseReference)
	if err != nil {
		return nil, err
	}
	return operationAuthorizationFromMoney(auth), nil
}

func operationAuthorizationFromMoney(auth *money.OperationAuthorization) *billing.OperationAuthorization {
	out := &billing.OperationAuthorization{
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
		State:                   billing.OperationAuthorizationState(auth.State),
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
	if r := auth.Refusal; r != nil {
		out.Refusal = &billing.ProviderBillingRefusal{Reason: billing.ProviderBillingQualificationReason(r.Reason), Detail: r.Detail, RefusedAt: r.RefusedAt}
	}
	out.Resolution = providerBillingResolutionFromMoney(auth.Resolution)
	return out
}

func providerBillingResolutionFromMoney(r *money.ProviderBillingResolution) *billing.ProviderBillingResolution {
	if r == nil {
		return nil
	}
	return &billing.ProviderBillingResolution{
		Kind: r.Kind, CostAmount: r.CostAmount, AttestedBy: r.AttestedBy, Reference: r.Reference, Note: r.Note, ResolvedAt: r.ResolvedAt,
	}
}

// RefuseProviderBillingQualification records, in an OpenRails-owned
// transaction, that the host cannot qualify a hold's provider cost.
func (s *Service) RefuseProviderBillingQualification(ctx context.Context, req billing.RefuseProviderBillingQualificationParams) (*billing.OperationAuthorization, error) {
	rt, err := s.runtime()
	if err != nil {
		return nil, err
	}
	var out *billing.OperationAuthorization
	err = rt.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		out, err = s.RefuseProviderBillingQualificationTx(ctx, tx, req)
		return err
	})
	return out, err
}

// RefuseProviderBillingQualificationTx is the host-transaction form.
func (s *Service) RefuseProviderBillingQualificationTx(ctx context.Context, tx pgx.Tx, req billing.RefuseProviderBillingQualificationParams) (*billing.OperationAuthorization, error) {
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
	auth, err := s.moneyService().RefuseProviderBillingQualificationInTx(ctx, txDB, req)
	if err != nil {
		return nil, err
	}
	return operationAuthorizationFromMoney(auth), nil
}

// CloseOperationAuthorization closes a refused hold on an operator's
// attestation in an OpenRails-owned transaction.
func (s *Service) CloseOperationAuthorization(ctx context.Context, req billing.CloseOperationAuthorizationParams) (*billing.OperationAuthorization, error) {
	rt, err := s.runtime()
	if err != nil {
		return nil, err
	}
	var out *billing.OperationAuthorization
	err = rt.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		out, err = s.CloseOperationAuthorizationTx(ctx, tx, req)
		return err
	})
	return out, err
}

// CloseOperationAuthorizationTx is the host-transaction form.
func (s *Service) CloseOperationAuthorizationTx(ctx context.Context, tx pgx.Tx, req billing.CloseOperationAuthorizationParams) (*billing.OperationAuthorization, error) {
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
	auth, err := s.moneyService().CloseOperationAuthorizationInTx(ctx, txDB, billing.ResolveProviderBillingQualificationParams(req))
	if err != nil {
		return nil, err
	}
	return operationAuthorizationFromMoney(auth), nil
}

// ListOperationAuthorizations pages holds, newest first.
func (s *Service) ListOperationAuthorizations(ctx context.Context, filter billing.OperationAuthorizationListParams) (*billing.ListPage[billing.OperationAuthorization], error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	page, err := s.moneyService().ListOperationAuthorizations(ctx, filter)
	if err != nil {
		return nil, err
	}
	out := billing.ListPage[billing.OperationAuthorization]{Items: make([]billing.OperationAuthorization, len(page.Items)), Next: page.Next}
	for i, item := range page.Items {
		out.Items[i] = *operationAuthorizationFromMoney(item)
	}
	return &out, nil
}
