package service

import (
	"context"
	"crypto/sha256"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
)

type OperationAuthorizationState = money.OperationAuthorizationState

const (
	OperationAuthorizationOpen     = money.OperationAuthorizationOpen
	OperationAuthorizationReleased = money.OperationAuthorizationReleased
	OperationAuthorizationSettled  = money.OperationAuthorizationSettled
)

var (
	ErrOperationAuthorizationConflict = money.ErrOperationAuthorizationConflict
	ErrOperationAuthorizationNotFound = money.ErrOperationAuthorizationNotFound
	ErrOperationAuthorizationNotOpen  = money.ErrOperationAuthorizationNotOpen
)

type OperationAuthorizationConflict = money.OperationAuthorizationConflict

// OperationAuthorizationRequest is exact host-authored authority for one
// provider operation. OpenRails validates the digest but deliberately does not
// parse or reserialize AuthorizationBody.
type OperationAuthorizationRequest struct {
	OperationID             string
	Payer                   identity.CustomerID
	RecordOwner             string
	AuthorizedUSDMicros     int64
	ClaimReference          string
	AuthorizationBody       []byte
	AuthorizationBodySHA256 [sha256.Size]byte
}

type OperationAuthorization struct {
	OperationID             string
	MerchantID              uuid.UUID
	Payer                   identity.CustomerID
	RecordOwner             string
	LedgerAccountID         uuid.UUID
	AuthorizedUSDMicros     int64
	ClaimReference          string
	AuthorizationBody       []byte
	AuthorizationBodySHA256 [sha256.Size]byte
	State                   OperationAuthorizationState
	TerminalReference       string
	CreatedAt               time.Time
	ReleasedAt              *time.Time
	SettledAt               *time.Time
	Replayed                bool
}

type ReleaseOperationAuthorizationRequest struct {
	OperationID      string
	ReleaseReference string
}

// OpenOperationAuthorizationTx rides a transaction owned by the embedding
// host. It neither commits nor rolls back that transaction.
func (s *Service) OpenOperationAuthorizationTx(ctx context.Context, tx pgx.Tx, req OperationAuthorizationRequest) (*OperationAuthorization, error) {
	rt, err := s.runtime()
	if err != nil {
		return nil, err
	}
	merchantID, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	ctx, _, err = rt.DB.BindMerchantTx(ctx, tx, merchantID)
	if err != nil {
		return nil, err
	}
	auth, err := s.moneyService().OpenOperationAuthorizationTx(ctx, tx, money.OperationAuthorizationInput{
		OperationID:             req.OperationID,
		Payer:                   req.Payer,
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

func (s *Service) GetOperationAuthorization(ctx context.Context, operationID string) (*OperationAuthorization, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	auth, err := s.moneyService().GetOperationAuthorization(ctx, operationID)
	if err != nil || auth == nil {
		return nil, err
	}
	return operationAuthorizationFromMoney(auth), nil
}

func (s *Service) ReleaseOperationAuthorization(ctx context.Context, req ReleaseOperationAuthorizationRequest) (*OperationAuthorization, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	auth, err := s.moneyService().ReleaseOperationAuthorization(ctx, req.OperationID, req.ReleaseReference)
	if err != nil {
		return nil, err
	}
	return operationAuthorizationFromMoney(auth), nil
}

func operationAuthorizationFromMoney(auth *money.OperationAuthorization) *OperationAuthorization {
	if auth == nil {
		return nil
	}
	return &OperationAuthorization{
		OperationID:             auth.OperationID,
		MerchantID:              auth.MerchantID,
		Payer:                   auth.Payer,
		RecordOwner:             auth.RecordOwner,
		LedgerAccountID:         auth.LedgerAccountID,
		AuthorizedUSDMicros:     auth.AuthorizedUSDMicros,
		ClaimReference:          auth.ClaimReference,
		AuthorizationBody:       auth.AuthorizationBody,
		AuthorizationBodySHA256: auth.AuthorizationBodySHA256,
		State:                   auth.State,
		TerminalReference:       auth.TerminalReference,
		CreatedAt:               auth.CreatedAt,
		ReleasedAt:              auth.ReleasedAt,
		SettledAt:               auth.SettledAt,
		Replayed:                auth.Replayed,
	}
}
