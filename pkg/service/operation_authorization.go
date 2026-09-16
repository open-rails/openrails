package service

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Provider-operation commands exist in two forms with identical semantics: the
// plain form commits its own merchant transaction (the Client routes), and the
// Tx form rides a transaction owned by an embedding host, which alone commits
// or rolls it back.

func (s *Service) OpenOperationAuthorization(ctx context.Context, req openrails.OperationAuthorizationRequest) (*openrails.OperationAuthorization, error) {
	rt, err := s.runtime()
	if err != nil {
		return nil, err
	}
	var out *openrails.OperationAuthorization
	err = rt.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		out, err = s.OpenOperationAuthorizationTx(ctx, tx, req)
		return err
	})
	return out, err
}

func (s *Service) OpenOperationAuthorizationTx(ctx context.Context, tx pgx.Tx, req openrails.OperationAuthorizationRequest) (*openrails.OperationAuthorization, error) {
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
		Payer:                   identity.CustomerID(req.Payer),
		RecordOwner:             req.RecordOwner,
		AuthorizedUSDMicros:     req.AuthorizedUSDMicros,
		ClaimReference:          req.ClaimReference,
		AuthorizationBody:       req.AuthorizationBody,
		AuthorizationBodySHA256: req.AuthorizationBodySHA256,
	})
	if err != nil {
		return nil, err
	}
	return operationAuthorizationFromMoney(auth), nil
}

func (s *Service) GetOperationAuthorization(ctx context.Context, operationID string) (*openrails.OperationAuthorization, error) {
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

func (s *Service) GetOperationAuthorizationTx(ctx context.Context, tx pgx.Tx, operationID string) (*openrails.OperationAuthorization, error) {
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

func (s *Service) ReleaseOperationAuthorization(ctx context.Context, req openrails.ReleaseOperationAuthorizationRequest) (*openrails.OperationAuthorization, error) {
	rt, err := s.runtime()
	if err != nil {
		return nil, err
	}
	var out *openrails.OperationAuthorization
	err = rt.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		out, err = s.ReleaseOperationAuthorizationTx(ctx, tx, req)
		return err
	})
	return out, err
}

func (s *Service) ReleaseOperationAuthorizationTx(ctx context.Context, tx pgx.Tx, req openrails.ReleaseOperationAuthorizationRequest) (*openrails.OperationAuthorization, error) {
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

func operationAuthorizationFromMoney(auth *money.OperationAuthorization) *openrails.OperationAuthorization {
	out := &openrails.OperationAuthorization{
		OperationID:                     auth.OperationID,
		MerchantID:                      auth.MerchantID,
		Payer:                           openrails.CustomerID(auth.Payer),
		RecordOwner:                     auth.RecordOwner,
		AuthorizedUSDMicros:             auth.AuthorizedUSDMicros,
		ClaimReference:                  auth.ClaimReference,
		AuthorizationBody:               auth.AuthorizationBody,
		AuthorizationBodySHA256:         auth.AuthorizationBodySHA256,
		State:                           openrails.OperationAuthorizationState(auth.State),
		TerminalReference:               auth.TerminalReference,
		SettlementProviderCostUSDMicros: auth.SettlementProviderCostUSDMicros,
		SettlementRatedUSDMicros:        auth.SettlementRatedUSDMicros,
		CreatedAt:                       auth.CreatedAt,
		ReleasedAt:                      auth.ReleasedAt,
		SettledAt:                       auth.SettledAt,
		Replayed:                        auth.Replayed,
	}
	if auth.State == money.OperationAuthorizationSettled {
		digest := openrails.SHA256(auth.SettlementBodySHA256)
		out.SettlementBody = auth.SettlementBody
		out.SettlementBodySHA256 = &digest
	}
	return out
}
