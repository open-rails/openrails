package credits

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/tenant"
	"github.com/uptrace/bun"
)

// Unified credit-line model (issue #302). Credit is ONE dial: an account spends
// its prepaid balance FIRST, then accrues to outstanding_owed up to its credit
// limit. Prepay-only = credit limit 0 (cannot go negative). Arrears = credit
// limit > 0 (a credit line). The limit is BillingMode + MaxOutstandingOwedCents:
// prepaid -> 0; arrears -> MaxOutstandingOwedCents (nil = unlimited line).
//
// This unifies the two IMMEDIATE-debit paths (prepaid Withdraw + arrears
// AccrueOwed) into one balance-first-then-owed spend. The hold->capture path and
// making BillingMode a pure display label are tracked as remaining #302 work.

// SpendParams is a unified immediate spend (balance first, then owed).
type SpendParams struct {
	Owner      *identity.OwnerOrgID
	UserID     string
	CreditType string
	Amount     int64
	Source     string
	SourceID   string
}

// SpendCredits debits an account balance-first-then-owed in one transaction,
// gated by the credit line. Idempotent on (tenant, owner, credit_type, source,
// source_id). Returns ErrInsufficientCredits when balance + remaining credit
// line cannot cover the amount.
func (s *CreditsService) SpendCredits(ctx context.Context, params SpendParams) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("credits service not initialized")
	}
	if params.Amount <= 0 {
		return fmt.Errorf("amount must be positive")
	}
	params.Source = strings.TrimSpace(params.Source)
	params.SourceID = strings.TrimSpace(params.SourceID)
	ct, err := s.GetCreditTypeByName(ctx, params.CreditType)
	if err != nil {
		return err
	}
	if !ct.IsActive {
		return ErrCreditTypeInactive
	}
	owner, err := resolveOwner(params.Owner, params.UserID)
	if err != nil {
		return err
	}

	tx, err := s.db.BeginTenantTx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// Serialize per account and guard idempotency on the spend coordinates.
	if _, err := s.lockBalance(ctx, tx, owner, params.UserID, ct.ID); err != nil {
		return err
	}
	if params.SourceID != "" {
		tenantID := tenant.FromContextOrDefault(ctx).UUID()
		existing := 0
		if err := tx.NewSelect().
			Model((*models.CreditTransaction)(nil)).
			ColumnExpr("count(*)").
			Where("tenant_id = ? AND owner_id = ?", tenantID, owner.UUID()).
			Where("credit_type_id = ?", ct.ID).
			Where("transaction_type IN ('withdrawal', ?)", txOwedAccrual).
			Where("source = ? AND source_id = ?", params.Source, params.SourceID).
			Scan(ctx, &existing); err != nil {
			return err
		}
		if existing > 0 {
			return tx.Commit() // already spent; idempotent no-op
		}
	}

	if _, _, err := s.spendBalanceThenOwedTx(ctx, tx, ct, owner, params.UserID, params.Source, params.SourceID, params.Amount); err != nil {
		return err
	}
	return tx.Commit()
}

// spendBalanceThenOwedTx debits `amount` within an existing tx: it draws the
// prepaid balance first (FIFO blocks) and accrues any remainder to
// outstanding_owed, gated by the account's credit line. The caller must have
// already locked the balance row (serialization point) and handled idempotency.
// Returns the balance-debit and owed-accrual transaction ids (either may be nil).
func (s *CreditsService) spendBalanceThenOwedTx(
	ctx context.Context, tx bun.Tx, ct *models.CreditType, owner identity.OwnerOrgID,
	userID, source, sourceID string, amount int64,
) (balanceTxnID, owedTxnID *uuid.UUID, err error) {
	if amount <= 0 {
		return nil, nil, fmt.Errorf("amount must be positive")
	}
	now := s.now()
	tenantID := tenant.FromContextOrDefault(ctx).UUID()
	ownerID := owner.UUID()

	bal, err := s.lockBalance(ctx, tx, owner, userID, ct.ID)
	if err != nil {
		return nil, nil, err
	}
	available := bal.Balance - bal.HeldBalance
	if available < 0 {
		available = 0
	}
	fromBalance := amount
	if fromBalance > available {
		fromBalance = available
	}
	fromOwed := amount - fromBalance

	// The remainder can only go to owed when the account has a credit line.
	if fromOwed > 0 {
		settings := new(models.CreditAccountSettings)
		serr := tx.NewSelect().Model(settings).
			Where("tenant_id = ? AND owner_id = ? AND credit_type_id = ?", tenantID, ownerID, ct.ID).
			For("UPDATE").
			Scan(ctx)
		if errors.Is(serr, sql.ErrNoRows) {
			// No settings row => prepaid default => no credit line.
			return nil, nil, ErrInsufficientCredits
		}
		if serr != nil {
			return nil, nil, serr
		}
		if settings.BillingMode != BillingModeArrears {
			return nil, nil, ErrInsufficientCredits // prepay-only: credit limit 0
		}
		if settings.MaxOutstandingOwedCents != nil &&
			settings.OutstandingOwedCents+fromOwed > *settings.MaxOutstandingOwedCents {
			return nil, nil, ErrInsufficientCredits // would exceed the credit line
		}
	}

	if fromBalance > 0 {
		if _, err := s.withdrawBalanceAndBlocks(ctx, tx, owner, userID, ct.ID, fromBalance); err != nil {
			return nil, nil, err
		}
		newBal := bal.Balance - fromBalance
		neg := -fromBalance
		trx := &models.CreditTransaction{
			ID: uuidutil.NewV7(), TenantID: tenantID, OwnerID: ownerID, UserID: userID,
			CreditTypeID: ct.ID, Amount: neg, BalanceAfter: &newBal,
			TransactionType: "withdrawal", Status: "posted",
			Source: source, SourceID: nullStr(sourceID), CreatedAt: now, UpdatedAt: now,
		}
		if _, err := tx.NewInsert().Model(trx).Exec(ctx); err != nil {
			return nil, nil, err
		}
		balanceTxnID = &trx.ID
	}

	if fromOwed > 0 {
		if _, err := tx.NewUpdate().Model((*models.CreditAccountSettings)(nil)).
			Set("outstanding_owed_cents = outstanding_owed_cents + ?", fromOwed).
			Set("updated_at = ?", now).
			Where("tenant_id = ? AND owner_id = ? AND credit_type_id = ?", tenantID, ownerID, ct.ID).
			Exec(ctx); err != nil {
			return nil, nil, err
		}
		trx := &models.CreditTransaction{
			ID: uuidutil.NewV7(), TenantID: tenantID, OwnerID: ownerID, UserID: userID,
			CreditTypeID: ct.ID, Amount: fromOwed, TransactionType: txOwedAccrual, Status: "posted",
			Source: source, SourceID: nullStr(sourceID), CreatedAt: now, UpdatedAt: now,
		}
		if _, err := tx.NewInsert().Model(trx).Exec(ctx); err != nil {
			return nil, nil, err
		}
		owedTxnID = &trx.ID
	}

	return balanceTxnID, owedTxnID, nil
}

func nullStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// captureSettleTx settles a captured hold's actual amount balance-first-then-owed
// (#302): it draws min(amount, availableAfter) from the prepaid balance/blocks
// and spills any remainder to outstanding_owed (the arrears credit line). This is
// what lets a hold placed against an arrears credit line capture even when the
// balance can't cover it. availableAfter is the available balance AFTER this
// hold's reservation has been released. The capture is pre-authorized, so this
// never re-gates. Returns the post-settlement balance.
func (s *CreditsService) captureSettleTx(ctx context.Context, tx bun.Tx, owner identity.OwnerOrgID, userID string, creditTypeID uuid.UUID, amount, availableAfter int64) (int64, error) {
	now := s.now()
	tenantID := tenant.FromContextOrDefault(ctx).UUID()
	ownerID := owner.UUID()
	if availableAfter < 0 {
		availableAfter = 0
	}
	fromBalance := amount
	if fromBalance > availableAfter {
		fromBalance = availableAfter
	}
	fromOwed := amount - fromBalance

	var newBal int64
	if fromBalance > 0 {
		nb, err := s.withdrawBalanceAndBlocks(ctx, tx, owner, userID, creditTypeID, fromBalance)
		if err != nil {
			return 0, err
		}
		newBal = nb
	} else {
		b := new(models.UserCreditBalance)
		if err := tx.NewSelect().Model(b).
			Where("tenant_id = ? AND owner_id = ? AND credit_type_id = ?", tenantID, ownerID, creditTypeID).
			Limit(1).Scan(ctx); err != nil {
			return 0, err
		}
		newBal = b.Balance
	}

	if fromOwed > 0 {
		// Only reachable for an arrears credit line (a prepaid hold reserves balance).
		if err := s.ensureSettingsRowTx(ctx, tx, tenantID, ownerID, creditTypeID, BillingModeArrears, now); err != nil {
			return 0, err
		}
		if _, err := tx.NewUpdate().Model((*models.CreditAccountSettings)(nil)).
			Set("outstanding_owed_cents = outstanding_owed_cents + ?", fromOwed).
			Set("updated_at = ?", now).
			Where("tenant_id = ? AND owner_id = ? AND credit_type_id = ?", tenantID, ownerID, creditTypeID).
			Exec(ctx); err != nil {
			return 0, err
		}
	}
	return newBal, nil
}
