package service

import (
	"context"
	"crypto/sha256"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/internal/modules/admission/spendgate"
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
	OperationID             string // canonical, at most 255 bytes
	Payer                   identity.CustomerID
	RecordOwner             string // canonical, at most 255 bytes
	AuthorizedUSDMicros     int64
	ClaimReference          string // canonical, at most 1024 bytes
	AuthorizationBody       []byte // exact canonical bytes, 1..65536 bytes
	AuthorizationBodySHA256 [sha256.Size]byte
}

type OperationAuthorization struct {
	OperationID                string
	MerchantID                 uuid.UUID
	Payer                      identity.CustomerID
	RecordOwner                string
	LedgerAccountID            uuid.UUID
	AuthorizedUSDMicros        int64
	CapturedUSDMicros          int64
	RemainingReservedUSDMicros int64
	ClaimReference             string
	AuthorizationBody          []byte
	AuthorizationBodySHA256    [sha256.Size]byte
	State                      OperationAuthorizationState
	TerminalReference          string
	CreatedAt                  time.Time
	ReleasedAt                 *time.Time
	SettledAt                  *time.Time
	Replayed                   bool
}

// OperationAuthorizationSettlementRequest is one exact host-authored actual
// cost increment. A final request may carry amount zero for a host-proven
// zero-window conclusion; non-final increments must be positive.
type OperationAuthorizationSettlementRequest struct {
	OperationID          string // canonical, at most 255 bytes
	SettlementID         string // immutable within OperationID, at most 255 bytes
	AmountUSDMicros      int64
	SettlementBody       []byte // exact canonical bytes, 1..65536 bytes
	SettlementBodySHA256 [sha256.Size]byte
	Final                bool
	FinalReference       string // required iff Final; opaque canonical proof, at most 1024 bytes
}

type OperationAuthorizationSettlement struct {
	OperationID          string
	SettlementID         string
	AmountUSDMicros      int64
	SettlementBody       []byte
	SettlementBodySHA256 [sha256.Size]byte
	Final                bool
	FinalReference       string
	CreatedAt            time.Time
	Authorization        *OperationAuthorization
	Replayed             bool
}

type ReleaseOperationAuthorizationRequest struct {
	OperationID      string
	ReleaseReference string // canonical opaque proof reference, at most 1024 bytes
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
	if rt.RedisClient == nil {
		return nil, fmt.Errorf("operation authorization unavailable: redis admission capacity is not configured")
	}
	ctx, txDB, err := rt.DB.BindMerchantTx(ctx, tx, merchantID)
	if err != nil {
		return nil, err
	}
	gate := spendgate.New(rt.RedisClient)
	auth, err := s.moneyService().OpenOperationAuthorizationInTx(ctx, txDB, money.OperationAuthorizationInput{
		OperationID:             req.OperationID,
		Payer:                   req.Payer,
		RecordOwner:             req.RecordOwner,
		AuthorizedUSDMicros:     req.AuthorizedUSDMicros,
		ClaimReference:          req.ClaimReference,
		AuthorizationBody:       req.AuthorizationBody,
		AuthorizationBodySHA256: req.AuthorizationBodySHA256,
	}, func(ctx context.Context) (int64, error) {
		return gate.HeldAmount(ctx, merchantID.String(), req.Payer.UUID().String(), "USD")
	})
	if err != nil {
		return nil, err
	}
	return operationAuthorizationFromMoney(auth), nil
}

// SettleOperationAuthorizationTx posts one actual-cost increment through the
// existing ledger inside a transaction owned by the embedding host. It neither
// commits nor rolls back tx and does not touch request-admission state.
func (s *Service) SettleOperationAuthorizationTx(ctx context.Context, tx pgx.Tx, req OperationAuthorizationSettlementRequest) (*OperationAuthorizationSettlement, error) {
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
	settlement, err := s.moneyService().SettleOperationAuthorizationInTx(ctx, txDB, money.OperationAuthorizationSettlementInput{
		OperationID:          req.OperationID,
		SettlementID:         req.SettlementID,
		AmountUSDMicros:      req.AmountUSDMicros,
		SettlementBody:       req.SettlementBody,
		SettlementBodySHA256: req.SettlementBodySHA256,
		Final:                req.Final,
		FinalReference:       req.FinalReference,
	})
	if err != nil {
		return nil, err
	}
	return operationAuthorizationSettlementFromMoney(settlement), nil
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
		OperationID:                auth.OperationID,
		MerchantID:                 auth.MerchantID,
		Payer:                      auth.Payer,
		RecordOwner:                auth.RecordOwner,
		LedgerAccountID:            auth.LedgerAccountID,
		AuthorizedUSDMicros:        auth.AuthorizedUSDMicros,
		CapturedUSDMicros:          auth.CapturedUSDMicros,
		RemainingReservedUSDMicros: auth.RemainingReservedUSDMicros,
		ClaimReference:             auth.ClaimReference,
		AuthorizationBody:          auth.AuthorizationBody,
		AuthorizationBodySHA256:    auth.AuthorizationBodySHA256,
		State:                      auth.State,
		TerminalReference:          auth.TerminalReference,
		CreatedAt:                  auth.CreatedAt,
		ReleasedAt:                 auth.ReleasedAt,
		SettledAt:                  auth.SettledAt,
		Replayed:                   auth.Replayed,
	}
}

func operationAuthorizationSettlementFromMoney(settlement *money.OperationAuthorizationSettlement) *OperationAuthorizationSettlement {
	if settlement == nil {
		return nil
	}
	return &OperationAuthorizationSettlement{
		OperationID:          settlement.OperationID,
		SettlementID:         settlement.SettlementID,
		AmountUSDMicros:      settlement.AmountUSDMicros,
		SettlementBody:       settlement.SettlementBody,
		SettlementBodySHA256: settlement.SettlementBodySHA256,
		Final:                settlement.Final,
		FinalReference:       settlement.FinalReference,
		CreatedAt:            settlement.CreatedAt,
		Authorization:        operationAuthorizationFromMoney(settlement.Authorization),
		Replayed:             settlement.Replayed,
	}
}
