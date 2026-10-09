package money

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"

	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
)

type OperationAuthorizationExtensionInput struct {
	OperationID   string
	Ordinal       int64
	Amount        int64
	MinimumAmount int64
	// OverdraftAmount is how far below zero this call lets prepaid capacity reach.
	OverdraftAmount int64
}

type OperationAuthorizationExtension struct {
	OperationID      string
	Ordinal          int64
	GrantedAmount    int64
	AuthorizedAmount int64
	Replayed         bool
}

// ExtendOperationAuthorizationInTx grows an open authorization by
// min(Amount, capacity) when capacity covers MinimumAmount, under the same
// payer lock and capacity rule as opening it. A refusal writes nothing. It
// never commits or rolls back the caller's transaction.
func (s *MoneyService) ExtendOperationAuthorizationInTx(ctx context.Context, txDB *db.DB, in OperationAuthorizationExtensionInput) (*OperationAuthorizationExtension, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	if txDB == nil {
		return nil, fmt.Errorf("operation authorization requires a bound transaction")
	}
	if err := validateOperationAuthorizationExtension(in); err != nil {
		return nil, fmt.Errorf("%w: %v", billing.ErrInvalid, err)
	}
	merchantID, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	txSvc := &MoneyService{db: txDB, clock: s.clock}
	q := txDB.Gen(ctx)
	params := gen.GetOperationAuthorizationParams{MerchantID: merchantID.UUID(), OperationID: in.OperationID}
	row, err := q.GetOperationAuthorization(ctx, params)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrOperationAuthorizationNotFound
	}
	if err != nil {
		return nil, err
	}
	payer := identity.CustomerID(row.CustomerID)
	bal, err := txSvc.lockBalance(ctx, q, payer, row.RecordOwner, operationAuthorizationCurrency)
	if err != nil {
		return nil, err
	}
	if row, err = q.GetOperationAuthorization(ctx, params); err != nil {
		return nil, err
	}

	committed, err := q.GetOperationAuthorizationExtension(ctx, gen.GetOperationAuthorizationExtensionParams{
		MerchantID: merchantID.UUID(), OperationID: in.OperationID, Ordinal: in.Ordinal,
	})
	if err == nil {
		return replayOperationAuthorizationExtension(committed, in)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	if OperationAuthorizationState(row.State) != OperationAuthorizationOpen {
		return nil, ErrOperationAuthorizationNotOpen
	}
	last, err := q.GetLastOperationAuthorizationExtensionOrdinal(ctx, gen.GetLastOperationAuthorizationExtensionOrdinalParams{
		MerchantID: merchantID.UUID(), OperationID: in.OperationID,
	})
	if err != nil {
		return nil, err
	}
	if in.Ordinal != last+1 {
		return nil, &OperationAuthorizationConflict{Field: "ordinal"}
	}
	if row.AuthorizedAmount > math.MaxInt64-in.Amount {
		return nil, fmt.Errorf("%w: authorized amount plus amount exceeds int64", billing.ErrInvalid)
	}

	capacity, err := txSvc.operationCapacity(ctx, q, merchantID.UUID(), payer, bal, in.OverdraftAmount)
	if err != nil {
		return nil, err
	}
	if capacity < in.MinimumAmount {
		return nil, ErrInsufficientCredits
	}
	granted := min(in.Amount, capacity)
	grown, err := q.ExtendOperationAuthorization(ctx, gen.ExtendOperationAuthorizationParams{
		GrantedAmount: granted, MerchantID: merchantID.UUID(), OperationID: in.OperationID,
		ExtendedAmount: row.ExtendedAmount,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("operation authorization changed under the payer lock")
	}
	if err != nil {
		return nil, err
	}
	ext, err := q.InsertOperationAuthorizationExtension(ctx, gen.InsertOperationAuthorizationExtensionParams{
		MerchantID: merchantID.UUID(), OperationID: in.OperationID, Ordinal: in.Ordinal,
		RequestedAmount: in.Amount, MinimumAmount: in.MinimumAmount,
		GrantedAmount: granted, AuthorizedAmount: grown.AuthorizedAmount,
	})
	if err != nil {
		return nil, err
	}
	return operationAuthorizationExtensionFromRow(ext, false), nil
}

func validateOperationAuthorizationExtension(in OperationAuthorizationExtensionInput) error {
	if err := validateOperationID(in.OperationID); err != nil {
		return err
	}
	if in.Ordinal < 1 {
		return fmt.Errorf("ordinal must be at least 1")
	}
	if in.MinimumAmount <= 0 {
		return fmt.Errorf("minimum_amount must be positive")
	}
	if in.Amount < in.MinimumAmount {
		return fmt.Errorf("amount must be at least minimum_amount")
	}
	if in.OverdraftAmount < 0 {
		return fmt.Errorf("overdraft_amount must be nonnegative")
	}
	return nil
}

func replayOperationAuthorizationExtension(row gen.BillingOperationAuthorizationExtension, in OperationAuthorizationExtensionInput) (*OperationAuthorizationExtension, error) {
	if row.RequestedAmount != in.Amount {
		return nil, &OperationAuthorizationConflict{Field: "amount"}
	}
	if row.MinimumAmount != in.MinimumAmount {
		return nil, &OperationAuthorizationConflict{Field: "minimum_amount"}
	}
	return operationAuthorizationExtensionFromRow(row, true), nil
}

func operationAuthorizationExtensionFromRow(row gen.BillingOperationAuthorizationExtension, replayed bool) *OperationAuthorizationExtension {
	return &OperationAuthorizationExtension{
		OperationID:      row.OperationID,
		Ordinal:          row.Ordinal,
		GrantedAmount:    row.GrantedAmount,
		AuthorizedAmount: row.AuthorizedAmount,
		Replayed:         replayed,
	}
}
