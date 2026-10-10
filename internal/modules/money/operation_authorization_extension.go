package money

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

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

// OperationAuthorizationExtension is one committed increment.
type OperationAuthorizationExtension struct {
	Ordinal         int64
	RequestedAmount int64
	MinimumAmount   int64
	GrantedAmount   int64
	CreatedAt       time.Time
}

// ExtendOperationAuthorizationInTx grows an open authorization by
// min(Amount, capacity) when capacity covers MinimumAmount, under the same
// payer lock and capacity rule as opening it, and answers the operation. A
// refusal writes nothing. It never commits or rolls back the caller's
// transaction.
func (s *MoneyService) ExtendOperationAuthorizationInTx(ctx context.Context, txDB *db.DB, in OperationAuthorizationExtensionInput) (*OperationAuthorization, error) {
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
		return nil, ErrProviderOperationNotFound
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
		if err := replayOperationAuthorizationExtension(committed, in); err != nil {
			return nil, err
		}
		auth := operationAuthorizationFromRow(row, true)
		return auth, attachOperationDetails(ctx, q, merchantID.UUID(), auth)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	if OperationAuthorizationState(row.State) != OperationAuthorizationOpen {
		return nil, ErrProviderOperationNotOpen
	}
	// A refused hold waits for an operator's close; nothing grows it meanwhile.
	if refused, err := providerBillingRefused(ctx, q, merchantID.UUID(), in.OperationID); err != nil {
		return nil, err
	} else if refused {
		return nil, ErrProviderOperationRefused
	}
	last, err := q.GetLastOperationAuthorizationExtensionOrdinal(ctx, gen.GetLastOperationAuthorizationExtensionOrdinalParams{
		MerchantID: merchantID.UUID(), OperationID: in.OperationID,
	})
	if err != nil {
		return nil, err
	}
	if in.Ordinal != last+1 {
		return nil, &ProviderOperationConflict{Field: "ordinal"}
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
	if _, err := q.InsertOperationAuthorizationExtension(ctx, gen.InsertOperationAuthorizationExtensionParams{
		MerchantID: merchantID.UUID(), OperationID: in.OperationID, Ordinal: in.Ordinal,
		RequestedAmount: in.Amount, MinimumAmount: in.MinimumAmount,
		GrantedAmount: granted, AuthorizedAmount: grown.AuthorizedAmount, CreatedAt: s.now(),
	}); err != nil {
		return nil, err
	}
	auth := operationAuthorizationFromRow(grown, false)
	return auth, attachOperationDetails(ctx, q, merchantID.UUID(), auth)
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

func replayOperationAuthorizationExtension(row gen.BillingOperationAuthorizationExtension, in OperationAuthorizationExtensionInput) error {
	if row.RequestedAmount != in.Amount {
		return &ProviderOperationConflict{Field: "amount"}
	}
	if row.MinimumAmount != in.MinimumAmount {
		return &ProviderOperationConflict{Field: "minimum_amount"}
	}
	return nil
}

func operationAuthorizationExtensionFromRow(row gen.BillingOperationAuthorizationExtension) *OperationAuthorizationExtension {
	return &OperationAuthorizationExtension{
		Ordinal:         row.Ordinal,
		RequestedAmount: row.RequestedAmount,
		MinimumAmount:   row.MinimumAmount,
		GrantedAmount:   row.GrantedAmount,
		CreatedAt:       row.CreatedAt,
	}
}
