package credits

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/uptrace/bun"
)

var (
	ErrInsufficientCredits   = errors.New("insufficient_credits")
	ErrInvalidNegativePolicy = errors.New("invalid_negative_policy")
	// ErrNegativeBalanceLimitExceeded indicates the request would exceed the caller-provided max_negative bound.
	ErrNegativeBalanceLimitExceeded = errors.New("negative_balance_limit_exceeded")
)

type CreditsService struct {
	db    *db.DB
	Clock clockwork.Clock
}

func NewCreditsService(database *db.DB) *CreditsService {
	return &CreditsService{db: database, Clock: clockwork.NewRealClock()}
}

func (s *CreditsService) now() time.Time {
	if s.Clock != nil {
		return s.Clock.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *CreditsService) GetCreditTypeByName(ctx context.Context, name string) (*models.CreditType, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("credits service not initialized")
	}
	ct := new(models.CreditType)
	err := s.db.GetDB().NewSelect().Model(ct).Where("name = ?", name).Limit(1).Scan(ctx)
	if err != nil {
		return nil, err
	}
	return ct, nil
}

func (s *CreditsService) GetBalance(ctx context.Context, userID string, creditType string) (*models.UserCreditBalance, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("credits service not initialized")
	}
	ct, err := s.GetCreditTypeByName(ctx, creditType)
	if err != nil {
		return nil, err
	}
	bal := new(models.UserCreditBalance)
	err = s.db.GetDB().NewSelect().
		Model(bal).
		Where("user_id = ? AND credit_type_id = ?", userID, ct.ID).
		Limit(1).
		Scan(ctx)
	if err == nil {
		return bal, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return &models.UserCreditBalance{
			UserID:       userID,
			CreditTypeID: ct.ID,
			Balance:      0,
			HeldBalance:  0,
		}, nil
	}
	return nil, err
}

func (s *CreditsService) GetTransactions(ctx context.Context, userID string, creditType string, limit, offset int) ([]models.CreditTransaction, int, error) {
	if s == nil || s.db == nil {
		return nil, 0, fmt.Errorf("credits service not initialized")
	}
	ct, err := s.GetCreditTypeByName(ctx, creditType)
	if err != nil {
		return nil, 0, err
	}
	var items []models.CreditTransaction
	q := s.db.GetDB().NewSelect().Model(&items).
		Where("user_id = ? AND credit_type_id = ?", userID, ct.ID)
	total, err := q.Count(ctx)
	if err != nil {
		return nil, 0, err
	}
	if limit <= 0 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	err = q.OrderExpr("created_at DESC").Limit(limit).Offset(offset).Scan(ctx)
	if err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

// GetTransactionBySource looks up a single credit transaction by its idempotency key.
// For API-credit usage tracking, this is commonly used with transactionType="hold" and
// (source, sourceID) = (metering_source, request_id).
func (s *CreditsService) GetTransactionBySource(ctx context.Context, userID string, creditType string, transactionType string, source string, sourceID string) (*models.CreditTransaction, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("credits service not initialized")
	}
	userID = strings.TrimSpace(userID)
	creditType = strings.TrimSpace(creditType)
	transactionType = strings.TrimSpace(transactionType)
	source = strings.TrimSpace(source)
	sourceID = strings.TrimSpace(sourceID)
	if userID == "" {
		return nil, fmt.Errorf("user_id required")
	}
	if creditType == "" {
		return nil, fmt.Errorf("credit_type required")
	}
	if transactionType == "" {
		return nil, fmt.Errorf("transaction_type required")
	}
	if source == "" {
		return nil, fmt.Errorf("source required")
	}
	if sourceID == "" {
		return nil, fmt.Errorf("source_id required")
	}

	ct, err := s.GetCreditTypeByName(ctx, creditType)
	if err != nil {
		return nil, err
	}

	trx := new(models.CreditTransaction)
	if err := s.db.GetDB().NewSelect().
		Model(trx).
		Where("user_id = ? AND credit_type_id = ?", userID, ct.ID).
		Where("transaction_type = ?", transactionType).
		Where("source = ? AND source_id = ?", source, sourceID).
		Limit(1).
		Scan(ctx); err != nil {
		return nil, err
	}
	return trx, nil
}

type CreditDepositParams struct {
	UserID      string
	CreditType  string
	Amount      int64
	Source      string
	SourceID    *uuid.UUID
	ExpiresAt   *time.Time
	Description *string
}

func (s *CreditsService) Deposit(ctx context.Context, params CreditDepositParams) (*models.CreditTransaction, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("credits service not initialized")
	}
	if params.Amount <= 0 {
		return nil, fmt.Errorf("amount must be positive")
	}
	ct, err := s.GetCreditTypeByName(ctx, params.CreditType)
	if err != nil {
		return nil, err
	}
	if !ct.IsActive {
		return nil, ErrCreditTypeInactive
	}

	tx, err := s.db.GetDB().(*bun.DB).BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	trx, err := s.depositTx(ctx, tx, ct, params)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return trx, nil
}

func (s *CreditsService) getCreditTypeByNameTx(ctx context.Context, tx bun.Tx, name string) (*models.CreditType, error) {
	ct := new(models.CreditType)
	if err := tx.NewSelect().Model(ct).Where("name = ?", name).Limit(1).Scan(ctx); err != nil {
		return nil, err
	}
	return ct, nil
}

func (s *CreditsService) depositTx(ctx context.Context, tx bun.Tx, ct *models.CreditType, params CreditDepositParams) (*models.CreditTransaction, error) {
	now := s.now()
	bal, err := s.lockBalance(ctx, tx, params.UserID, ct.ID)
	if err != nil {
		return nil, err
	}

	// Idempotency: if caller provides SourceID, treat (user_id, credit_type_id, source, source_id)
	// as an idempotency key for deposits. This is safe because we lock the user_credit_balances row
	// for UPDATE, serializing deposits per user+credit_type.
	sourceIDText := (*string)(nil)
	if params.SourceID != nil {
		v := params.SourceID.String()
		sourceIDText = &v
		existing := new(models.CreditTransaction)
		err := tx.NewSelect().
			Model(existing).
			Where("user_id = ? AND credit_type_id = ?", params.UserID, ct.ID).
			Where("transaction_type = 'deposit'").
			Where("source = ? AND source_id = ?", params.Source, v).
			Limit(1).
			Scan(ctx)
		if err == nil {
			return existing, nil
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
	}

	newBal := bal.Balance + params.Amount

	if _, err := tx.NewUpdate().Model((*models.UserCreditBalance)(nil)).
		Set("balance = ?", newBal).
		Set("updated_at = ?", now).
		Where("user_id = ? AND credit_type_id = ?", params.UserID, ct.ID).
		Exec(ctx); err != nil {
		return nil, err
	}

	trx := &models.CreditTransaction{
		ID:              uuid.New(),
		UserID:          params.UserID,
		CreditTypeID:    ct.ID,
		Amount:          params.Amount,
		BalanceAfter:    &newBal,
		TransactionType: "deposit",
		Status:          "posted",
		Source:          params.Source,
		SourceID:        sourceIDText,
		ExpiresAt:       params.ExpiresAt,
		Description:     params.Description,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if _, err := tx.NewInsert().Model(trx).Exec(ctx); err != nil {
		return nil, err
	}
	block := &models.CreditBlock{
		ID:                  uuid.New(),
		UserID:              params.UserID,
		CreditTypeID:        ct.ID,
		OriginalAmount:      params.Amount,
		RemainingAmount:     params.Amount,
		ExpiresAt:           params.ExpiresAt,
		SourceTransactionID: &trx.ID,
		CreatedAt:           now,
	}
	if _, err := tx.NewInsert().Model(block).Exec(ctx); err != nil {
		return nil, err
	}

	return trx, nil
}

type CreditWithdrawParams struct {
	UserID        string
	CreditType    string
	Amount        int64
	Source        string
	SourceID      *uuid.UUID
	AllowNegative bool
	MaxNegative   int64
}

func (s *CreditsService) Withdraw(ctx context.Context, params CreditWithdrawParams) (*models.CreditTransaction, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("credits service not initialized")
	}
	if params.Amount <= 0 {
		return nil, fmt.Errorf("amount must be positive")
	}
	ct, err := s.GetCreditTypeByName(ctx, params.CreditType)
	if err != nil {
		return nil, err
	}
	if !ct.IsActive {
		return nil, ErrCreditTypeInactive
	}
	if params.AllowNegative {
		// Keep this intentionally narrow: only api_credits supports bounded negative in internal mode.
		if strings.TrimSpace(ct.Name) != "api_credits" {
			return nil, ErrInvalidNegativePolicy
		}
		if params.MaxNegative <= 0 {
			return nil, ErrInvalidNegativePolicy
		}
	}

	tx, err := s.db.GetDB().(*bun.DB).BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	trx, err := s.withdrawTx(ctx, tx, ct.ID, params)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return trx, nil
}

func (s *CreditsService) Hold(ctx context.Context, userID string, creditType string, amount int64, source, sourceID string, expiresAt time.Time) (*models.CreditTransaction, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("credits service not initialized")
	}
	if amount <= 0 {
		return nil, fmt.Errorf("amount must be positive")
	}
	ct, err := s.GetCreditTypeByName(ctx, creditType)
	if err != nil {
		return nil, err
	}
	if !ct.IsActive {
		return nil, ErrCreditTypeInactive
	}

	tx, err := s.db.GetDB().(*bun.DB).BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	now := s.now()

	// Idempotency: treat (user_id, credit_type_id, source, source_id) as a unique key for holds.
	// This prevents duplicate held_balance reservations on caller retries.
	existing := new(models.CreditTransaction)
	err = tx.NewSelect().
		Model(existing).
		Where("transaction_type = 'hold'").
		Where("user_id = ? AND credit_type_id = ?", userID, ct.ID).
		Where("source = ? AND source_id = ?", source, sourceID).
		Limit(1).
		Scan(ctx)
	if err == nil {
		return existing, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	bal, err := s.lockBalance(ctx, tx, userID, ct.ID)
	if err != nil {
		return nil, err
	}
	available := bal.Balance - bal.HeldBalance
	if available < amount {
		return nil, ErrInsufficientCredits
	}

	if _, err := tx.NewUpdate().Model((*models.UserCreditBalance)(nil)).
		Set("held_balance = ?", bal.HeldBalance+amount).
		Set("updated_at = ?", now).
		Where("user_id = ? AND credit_type_id = ?", userID, ct.ID).
		Exec(ctx); err != nil {
		return nil, err
	}

	exp := expiresAt.UTC()
	auth := amount
	hold := &models.CreditTransaction{
		ID:              uuid.New(),
		UserID:          userID,
		CreditTypeID:    ct.ID,
		Amount:          0,
		Source:          source,
		SourceID:        &sourceID,
		Status:          "active",
		Authorized:      &auth,
		ExpiresAt:       &exp,
		TransactionType: "hold",
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if _, err := tx.NewInsert().Model(hold).Exec(ctx); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return hold, nil
}

func (s *CreditsService) CaptureHold(ctx context.Context, holdID uuid.UUID, actualAmount int64) (*models.CreditTransaction, error) {
	return s.CaptureHoldWithPolicy(ctx, holdID, actualAmount, false, 0)
}

func (s *CreditsService) CaptureHoldWithPolicy(ctx context.Context, holdID uuid.UUID, actualAmount int64, allowNegative bool, maxNegative int64) (*models.CreditTransaction, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("credits service not initialized")
	}
	if allowNegative && maxNegative <= 0 {
		return nil, ErrInvalidNegativePolicy
	}
	tx, err := s.db.GetDB().(*bun.DB).BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	hold := new(models.CreditTransaction)
	if err := tx.NewSelect().Model(hold).Where("id = ?", holdID).For("UPDATE").Scan(ctx); err != nil {
		return nil, err
	}
	if hold.TransactionType != "hold" {
		return nil, fmt.Errorf("transaction is not a hold")
	}
	if hold.Status != "active" {
		return nil, fmt.Errorf("hold is not active")
	}
	if hold.Authorized == nil || *hold.Authorized <= 0 {
		return nil, fmt.Errorf("hold missing authorized amount")
	}
	authorized := *hold.Authorized
	if actualAmount <= 0 || actualAmount > authorized {
		return nil, fmt.Errorf("invalid capture amount")
	}

	now := s.now()
	bal, err := s.lockBalance(ctx, tx, hold.UserID, hold.CreditTypeID)
	if err != nil {
		return nil, err
	}
	if _, err := tx.NewUpdate().Model((*models.UserCreditBalance)(nil)).
		Set("held_balance = ?", bal.HeldBalance-authorized).
		Set("updated_at = ?", now).
		Where("user_id = ? AND credit_type_id = ?", hold.UserID, hold.CreditTypeID).
		Exec(ctx); err != nil {
		return nil, err
	}

	// Apply the actual withdrawal and update the existing hold transaction row to reflect capture.
	if allowNegative {
		// Resolve credit type name for policy enforcement.
		ct := new(models.CreditType)
		if err := tx.NewSelect().Model(ct).Where("id = ?", hold.CreditTypeID).Limit(1).Scan(ctx); err != nil {
			return nil, err
		}
		if strings.TrimSpace(ct.Name) != "api_credits" {
			return nil, ErrInvalidNegativePolicy
		}
	}
	newBal, debt, err := s.withdrawBalanceAndBlocksWithPolicy(ctx, tx, hold.UserID, hold.CreditTypeID, actualAmount, allowNegative, maxNegative)
	if err != nil {
		return nil, err
	}
	hold.Status = "captured"
	hold.Captured = &actualAmount
	hold.Amount = -actualAmount
	hold.BalanceAfter = &newBal
	hold.UpdatedAt = now
	if _, err := tx.NewUpdate().Model(hold).
		Column("status", "captured_amount", "amount", "balance_after", "updated_at").
		WherePK().
		Exec(ctx); err != nil {
		return nil, err
	}
	if debt > 0 {
		debt := &models.CreditBlock{
			ID:                  uuid.New(),
			UserID:              hold.UserID,
			CreditTypeID:        hold.CreditTypeID,
			OriginalAmount:      -debt,
			RemainingAmount:     -debt,
			ExpiresAt:           nil,
			SourceTransactionID: &hold.ID,
			CreatedAt:           now,
		}
		if _, err := tx.NewInsert().Model(debt).Exec(ctx); err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return hold, nil
}

func (s *CreditsService) ReleaseHold(ctx context.Context, holdID uuid.UUID) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("credits service not initialized")
	}
	tx, err := s.db.GetDB().(*bun.DB).BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	hold := new(models.CreditTransaction)
	if err := tx.NewSelect().Model(hold).Where("id = ?", holdID).For("UPDATE").Scan(ctx); err != nil {
		return err
	}
	if hold.TransactionType != "hold" {
		return fmt.Errorf("transaction is not a hold")
	}
	if hold.Status != "active" {
		return fmt.Errorf("hold is not active")
	}
	if hold.Authorized == nil || *hold.Authorized <= 0 {
		return fmt.Errorf("hold missing authorized amount")
	}
	authorized := *hold.Authorized

	now := s.now()
	bal, err := s.lockBalance(ctx, tx, hold.UserID, hold.CreditTypeID)
	if err != nil {
		return err
	}
	if _, err := tx.NewUpdate().Model((*models.UserCreditBalance)(nil)).
		Set("held_balance = ?", bal.HeldBalance-authorized).
		Set("updated_at = ?", now).
		Where("user_id = ? AND credit_type_id = ?", hold.UserID, hold.CreditTypeID).
		Exec(ctx); err != nil {
		return err
	}

	hold.Status = "released"
	hold.UpdatedAt = now
	if _, err := tx.NewUpdate().Model(hold).
		Column("status", "updated_at").
		WherePK().
		Exec(ctx); err != nil {
		return err
	}

	return tx.Commit()
}

func (s *CreditsService) withdrawBalanceAndBlocks(ctx context.Context, tx bun.Tx, userID string, creditTypeID uuid.UUID, amount int64) (int64, error) {
	newBal, _, err := s.withdrawBalanceAndBlocksWithPolicy(ctx, tx, userID, creditTypeID, amount, false, 0)
	return newBal, err
}

func (s *CreditsService) withdrawBalanceAndBlocksWithPolicy(ctx context.Context, tx bun.Tx, userID string, creditTypeID uuid.UUID, amount int64, allowNegative bool, maxNegative int64) (newBal int64, debt int64, err error) {
	now := s.now()
	bal, err := s.lockBalance(ctx, tx, userID, creditTypeID)
	if err != nil {
		return 0, 0, err
	}
	available := bal.Balance - bal.HeldBalance
	if available < amount {
		if !allowNegative {
			return 0, 0, ErrInsufficientCredits
		}
		if maxNegative <= 0 {
			return 0, 0, ErrInvalidNegativePolicy
		}
		// Guardrail: resulting balance must not go below -maxNegative.
		if bal.Balance-amount < -maxNegative {
			return 0, 0, ErrNegativeBalanceLimitExceeded
		}
	}

	remaining := amount
	var blocks []models.CreditBlock
	if err := tx.NewSelect().Model(&blocks).
		Where("user_id = ? AND credit_type_id = ? AND remaining_amount > 0 AND (expires_at IS NULL OR expires_at > ?)", userID, creditTypeID, now).
		OrderExpr("expires_at ASC NULLS LAST, created_at ASC").
		For("UPDATE").
		Scan(ctx); err != nil {
		return 0, 0, err
	}
	for i := range blocks {
		if remaining == 0 {
			break
		}
		use := blocks[i].RemainingAmount
		if use > remaining {
			use = remaining
		}
		if use <= 0 {
			continue
		}
		blocks[i].RemainingAmount -= use
		remaining -= use
		if _, err := tx.NewUpdate().Model(&blocks[i]).
			Column("remaining_amount").
			WherePK().
			Exec(ctx); err != nil {
			return 0, 0, err
		}
	}

	newBal = bal.Balance - amount
	if _, err := tx.NewUpdate().Model((*models.UserCreditBalance)(nil)).
		Set("balance = ?", newBal).
		Set("updated_at = ?", now).
		Where("user_id = ? AND credit_type_id = ?", userID, creditTypeID).
		Exec(ctx); err != nil {
		return 0, 0, err
	}
	if remaining > 0 {
		// We only allow this when allowNegative=true (overspend "debt").
		if !allowNegative {
			return 0, 0, fmt.Errorf("credit blocks out of sync")
		}
		debt = remaining
	}
	return newBal, debt, nil
}

func (s *CreditsService) lockBalance(ctx context.Context, tx bun.Tx, userID string, creditTypeID uuid.UUID) (*models.UserCreditBalance, error) {
	bal := new(models.UserCreditBalance)
	err := tx.NewSelect().Model(bal).
		Where("user_id = ? AND credit_type_id = ?", userID, creditTypeID).
		For("UPDATE").
		Scan(ctx)
	if err == nil {
		return bal, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	now := s.now()
	bal = &models.UserCreditBalance{
		ID:           uuid.New(),
		UserID:       userID,
		CreditTypeID: creditTypeID,
		Balance:      0,
		HeldBalance:  0,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if _, err := tx.NewInsert().Model(bal).Exec(ctx); err != nil {
		return nil, err
	}
	return bal, nil
}

func (s *CreditsService) withdrawTx(ctx context.Context, tx bun.Tx, creditTypeID uuid.UUID, params CreditWithdrawParams) (*models.CreditTransaction, error) {
	now := s.now()
	sourceIDText := (*string)(nil)
	if params.SourceID != nil {
		v := params.SourceID.String()
		sourceIDText = &v
	}
	trxID := uuid.New()
	newBal, debt, err := s.withdrawBalanceAndBlocksWithPolicy(ctx, tx, params.UserID, creditTypeID, params.Amount, params.AllowNegative, params.MaxNegative)
	if err != nil {
		return nil, err
	}

	trx := &models.CreditTransaction{
		ID:              trxID,
		UserID:          params.UserID,
		CreditTypeID:    creditTypeID,
		Amount:          -params.Amount,
		BalanceAfter:    &newBal,
		TransactionType: "withdrawal",
		Status:          "posted",
		Source:          params.Source,
		SourceID:        sourceIDText,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if _, err := tx.NewInsert().Model(trx).Exec(ctx); err != nil {
		return nil, err
	}
	if debt > 0 {
		debtBlock := &models.CreditBlock{
			ID:                  uuid.New(),
			UserID:              params.UserID,
			CreditTypeID:        creditTypeID,
			OriginalAmount:      -debt,
			RemainingAmount:     -debt,
			ExpiresAt:           nil,
			SourceTransactionID: &trx.ID,
			CreatedAt:           now,
		}
		if _, err := tx.NewInsert().Model(debtBlock).Exec(ctx); err != nil {
			return nil, err
		}
	}
	return trx, nil
}
