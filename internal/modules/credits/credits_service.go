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
	"github.com/open-rails/openrails/internal/shared/timeutil"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/tenant"
	"github.com/uptrace/bun"
)

var (
	ErrInsufficientCredits = errors.New("insufficient_credits")
)

type CreditsService struct {
	db    *db.DB
	clock clockwork.Clock
}

func NewCreditsService(database *db.DB, clocks ...clockwork.Clock) *CreditsService {
	return &CreditsService{db: database, clock: timeutil.FirstClock(clocks...)}
}

func (s *CreditsService) SetClock(c clockwork.Clock) {
	s.clock = timeutil.FirstClock(c)
}

func (s *CreditsService) Clock() clockwork.Clock {
	return s.clock
}

func (s *CreditsService) now() time.Time {
	if s.clock != nil {
		return s.clock.Now().UTC()
	}
	return time.Now().UTC()
}

// ErrOwnerRequired is returned when a credit operation cannot resolve a real
// owner org id. HARDCUT (#221): owner_id is supplied by the caller and is NOT
// synthesized — there is no deterministic stand-in derivation. For the
// self-hosted / single-tenant personal case the tenant subject IS the authenticated
// user's own account/personal tenant-subject UUID, so a non-UUID invoker with no explicit
// payer is a programming error rather than something to paper over.
var ErrTenantSubjectRequired = errors.New("tenant_subject_id required")

// resolveTenantSubject resolves the tenant subject for a credit operation (issue #221, the
// payer/billing payer). When the caller supplies an explicit tenant subject it is
// used verbatim. Otherwise, for the self-hosted / single-tenant personal case,
// the payer is the invoker's own account/personal tenant-subject id parsed from its UUID
// subject (identity.TenantSubjectIDFromString). It is NEVER a synthesized stand-in.
//
// The invokerInvokerID is the existing free-form invoker_id (who caused usage); it is
// retained for attribution and is NOT the financial payer. resolveTenantSubject returns
// ErrTenantSubjectRequired when no payer can be resolved.
func resolveTenantSubject(payer *identity.TenantSubjectID, invokerInvokerID string) (identity.TenantSubjectID, error) {
	if payer != nil && !payer.IsZero() {
		return *payer, nil
	}
	resolved := identity.TenantSubjectIDFromString(invokerInvokerID)
	if resolved.IsZero() {
		return identity.TenantSubjectID{}, ErrTenantSubjectRequired
	}
	return resolved, nil
}

func (s *CreditsService) GetCreditTypeByName(ctx context.Context, name string) (*models.CreditType, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("credits service not initialized")
	}
	// Self-contained RLS scoping (#227): pin a tenant connection so the lookup sets
	// app.tenant_id and is tenant-scoped under the openrails_app role, whether
	// called standalone or before opening a separate balance tx. RunInTenantConn is
	// a no-op (reuses the tx) when s.db is already a tx-scoped service, so it is
	// safe in both call shapes.
	ct := new(models.CreditType)
	err := s.db.RunInTenantConn(ctx, func(ctx context.Context) error {
		return s.db.Q(ctx).NewSelect().Model(ct).Where("name = ?", name).Limit(1).Scan(ctx)
	})
	if err != nil {
		return nil, err
	}
	return ct, nil
}

func (s *CreditsService) GetBalance(ctx context.Context, invokerID string, creditType string) (*models.UserCreditBalance, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("credits service not initialized")
	}
	ct, err := s.GetCreditTypeByName(ctx, creditType)
	if err != nil {
		return nil, err
	}
	tenantID := tenant.FromContextOrDefault(ctx).UUID()
	// Reads are scoped by tenant + OWNER (issue #221). For the self-hosted /
	// single-tenant personal case the payer IS the user's own account/personal tenant-subject
	// UUID — the same row the tenant-subject-owned writers stamp — so single-user reads return
	// the right balance. tenant_subject_id is never synthesized.
	payer, err := resolveTenantSubject(nil, invokerID)
	if err != nil {
		return nil, err
	}
	ownerID := payer.UUID()
	bal := new(models.UserCreditBalance)
	err = s.db.Q(ctx).NewSelect().
		Model(bal).
		Where("tenant_id = ?", tenantID).
		Where("tenant_subject_id = ? AND credit_type_id = ?", ownerID, ct.ID).
		Limit(1).
		Scan(ctx)
	if err == nil {
		return bal, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return &models.UserCreditBalance{
			TenantID:        tenantID,
			TenantSubjectID: ownerID,
			InvokerID:       invokerID,
			CreditTypeID:    ct.ID,
			Balance:         0,
			HeldBalance:     0,
		}, nil
	}
	return nil, err
}

// GetBalanceForTenantSubject reads a balance scoped explicitly by tenant subject (issue
// #221), for callers that own balances at a team org rather than a personal
// org. The invoker invoker id is not required for an payer-scoped read.
func (s *CreditsService) GetBalanceForTenantSubject(ctx context.Context, payer identity.TenantSubjectID, creditType string) (*models.UserCreditBalance, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("credits service not initialized")
	}
	ct, err := s.GetCreditTypeByName(ctx, creditType)
	if err != nil {
		return nil, err
	}
	tenantID := tenant.FromContextOrDefault(ctx).UUID()
	ownerID := payer.UUID()
	bal := new(models.UserCreditBalance)
	err = s.db.Q(ctx).NewSelect().
		Model(bal).
		Where("tenant_id = ?", tenantID).
		Where("tenant_subject_id = ? AND credit_type_id = ?", ownerID, ct.ID).
		Limit(1).
		Scan(ctx)
	if err == nil {
		return bal, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return &models.UserCreditBalance{
			TenantID:        tenantID,
			TenantSubjectID: ownerID,
			CreditTypeID:    ct.ID,
			Balance:         0,
			HeldBalance:     0,
		}, nil
	}
	return nil, err
}

func (s *CreditsService) GetTransactions(ctx context.Context, invokerID string, creditType string, limit, offset int) ([]models.CreditTransaction, int, error) {
	if s == nil || s.db == nil {
		return nil, 0, fmt.Errorf("credits service not initialized")
	}
	ct, err := s.GetCreditTypeByName(ctx, creditType)
	if err != nil {
		return nil, 0, err
	}
	payer, err := resolveTenantSubject(nil, invokerID)
	if err != nil {
		return nil, 0, err
	}
	ownerID := payer.UUID()
	var items []models.CreditTransaction
	q := s.db.Q(ctx).NewSelect().Model(&items).
		Where("tenant_id = ?", tenant.FromContextOrDefault(ctx).UUID()).
		Where("tenant_subject_id = ? AND credit_type_id = ?", ownerID, ct.ID)
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

// GetTransactionsByTenantSubject lists credit transactions for an EXPLICIT tenant subject
// (the payer), newest first, paginated. Unlike GetTransactions it does not derive
// the payer from a invoker id — it filters tenant_subject_id directly, which is what the
// org-level billing-account usage view (issue #242) needs. RLS-scoped to the
// request tenant via Q(ctx).
func (s *CreditsService) GetTransactionsByTenantSubject(ctx context.Context, payer identity.TenantSubjectID, creditType string, limit, offset int) ([]models.CreditTransaction, int, error) {
	if s == nil || s.db == nil {
		return nil, 0, fmt.Errorf("credits service not initialized")
	}
	if payer.IsZero() {
		return nil, 0, fmt.Errorf("payer required")
	}
	ct, err := s.GetCreditTypeByName(ctx, creditType)
	if err != nil {
		return nil, 0, err
	}
	var items []models.CreditTransaction
	q := s.db.Q(ctx).NewSelect().Model(&items).
		Where("tenant_id = ?", tenant.FromContextOrDefault(ctx).UUID()).
		Where("tenant_subject_id = ? AND credit_type_id = ?", payer.UUID(), ct.ID)
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
	if err := q.OrderExpr("created_at DESC").Limit(limit).Offset(offset).Scan(ctx); err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

// GetAccountSettingsForTenantSubject returns the stored credit-account settings for an
// payer+credit_type (billing mode, spend caps, auto-top-up, expiry default),
// RLS-scoped to the request tenant (issue #242). Never nil — missing rows return
// the defaults.
func (s *CreditsService) GetAccountSettingsForTenantSubject(ctx context.Context, payer identity.TenantSubjectID, creditType string) (*models.CreditAccountSettings, error) {
	var out *models.CreditAccountSettings
	err := s.db.RunInTenantConn(ctx, func(ctx context.Context) error {
		st, e := s.GetAccountSettings(ctx, payer, creditType)
		if e != nil {
			return e
		}
		out = st
		return nil
	})
	return out, err
}

// GetTransactionBySource looks up a single credit transaction by its idempotency key.
// For API-credit usage tracking, this is commonly used with transactionType="hold" and
// (source, sourceID) = (metering_source, request_id).
func (s *CreditsService) GetTransactionBySource(ctx context.Context, invokerID string, creditType string, transactionType string, source string, sourceID string) (*models.CreditTransaction, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("credits service not initialized")
	}
	invokerID = strings.TrimSpace(invokerID)
	creditType = strings.TrimSpace(creditType)
	transactionType = strings.TrimSpace(transactionType)
	source = strings.TrimSpace(source)
	sourceID = strings.TrimSpace(sourceID)
	if invokerID == "" {
		return nil, fmt.Errorf("invoker_id required")
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

	payer, err := resolveTenantSubject(nil, invokerID)
	if err != nil {
		return nil, err
	}
	ownerID := payer.UUID()
	trx := new(models.CreditTransaction)
	if err := s.db.Q(ctx).NewSelect().
		Model(trx).
		Where("tenant_id = ?", tenant.FromContextOrDefault(ctx).UUID()).
		Where("tenant_subject_id = ? AND credit_type_id = ?", ownerID, ct.ID).
		Where("transaction_type = ?", transactionType).
		Where("source = ? AND source_id = ?", source, sourceID).
		Limit(1).
		Scan(ctx); err != nil {
		return nil, err
	}
	return trx, nil
}

type CreditDepositParams struct {
	// TenantSubjectID is the tenant subject that owns the deposited balance (issue #221). When
	// nil, the payer is the invoker (InvokerID)'s own account/personal tenant-subject UUID for the
	// self-hosted / single-tenant personal case; it is never a synthesized
	// stand-in, and a non-UUID InvokerID with no explicit payer is rejected.
	TenantSubjectID *identity.TenantSubjectID
	InvokerID       string
	CreditType      string
	Amount          int64
	Source          string
	SourceID        *uuid.UUID
	ExpiresAt       *time.Time
	// ApplyAccountExpiryDefault, when true and ExpiresAt is nil, sets the deposit's
	// expiry to now + the payer's credit_account_settings.default_credit_expiry_days
	// (issue #240). Purchase/top-up paths set this so bought credits expire (default
	// 365d); permanent grants (subscriptions, admin) leave it false. No-op when no
	// settings default is configured.
	ApplyAccountExpiryDefault bool
	Description               *string
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

	// Apply the per-account default expiry (issue #240) when requested and the
	// caller did not set an explicit expiry.
	if params.ExpiresAt == nil && params.ApplyAccountExpiryDefault {
		if payer, oerr := resolveTenantSubject(params.TenantSubjectID, params.InvokerID); oerr == nil {
			if settings, serr := s.GetAccountSettings(ctx, payer, params.CreditType); serr == nil &&
				settings.DefaultCreditExpiryDays != nil && *settings.DefaultCreditExpiryDays > 0 {
				exp := s.now().AddDate(0, 0, *settings.DefaultCreditExpiryDays)
				params.ExpiresAt = &exp
			}
		}
	}

	tx, err := s.db.BeginTenantTx(ctx)
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

// ensureTenantSubject upserts the billing.tenant_subjects row for a self-service
// subject so the credit-write FKs added in migration 076 are satisfied on a
// subject's FIRST credit operation (deposit/hold/usage). ON CONFLICT DO NOTHING.
func ensureTenantSubject(ctx context.Context, db bun.IDB, tenantID, tsid uuid.UUID) error {
	if tsid == uuid.Nil {
		return nil
	}
	if tenantID == uuid.Nil {
		tenantID = tenant.FromContextOrDefault(ctx).UUID()
	}
	_, err := db.NewRaw(
		`INSERT INTO billing.tenant_subjects (id, tenant_id, issuer, subject)
		 VALUES (?, ?, ?, ?) ON CONFLICT DO NOTHING`,
		tsid, tenantID, "openrails:self", tsid.String(),
	).Exec(ctx)
	return err
}

func (s *CreditsService) depositTx(ctx context.Context, tx bun.Tx, ct *models.CreditType, params CreditDepositParams) (*models.CreditTransaction, error) {
	now := s.now()
	tenantID := tenant.FromContextOrDefault(ctx).UUID()
	payer, err := resolveTenantSubject(params.TenantSubjectID, params.InvokerID)
	if err != nil {
		return nil, err
	}
	ownerID := payer.UUID()
	if err := ensureTenantSubject(ctx, tx, tenantID, ownerID); err != nil {
		return nil, err
	}
	bal, err := s.lockBalance(ctx, tx, payer, params.InvokerID, ct.ID)
	if err != nil {
		return nil, err
	}

	// Idempotency: if caller provides SourceID, treat (tenant, payer, credit_type,
	// source, source_id) as an idempotency key for deposits. This is safe because
	// we lock the balance row for UPDATE, serializing deposits per payer+credit_type.
	sourceIDText := (*string)(nil)
	if params.SourceID != nil {
		v := params.SourceID.String()
		sourceIDText = &v
		existing := new(models.CreditTransaction)
		err := tx.NewSelect().
			Model(existing).
			Where("tenant_id = ? AND tenant_subject_id = ?", tenantID, ownerID).
			Where("credit_type_id = ?", ct.ID).
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
		Where("tenant_id = ? AND tenant_subject_id = ? AND credit_type_id = ?", tenantID, ownerID, ct.ID).
		Exec(ctx); err != nil {
		return nil, err
	}

	trx := &models.CreditTransaction{
		ID:              uuidutil.NewV7(),
		TenantID:        tenantID,
		TenantSubjectID: ownerID,
		InvokerID:       params.InvokerID,
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
		ID:                  uuidutil.NewV7(),
		TenantID:            tenantID,
		TenantSubjectID:     ownerID,
		InvokerID:           params.InvokerID,
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
	// TenantSubjectID is the tenant subject to withdraw from (issue #221). When nil, the payer
	// is the invoker (InvokerID)'s own account/personal tenant-subject UUID for the self-hosted /
	// single-tenant personal case; it is never a synthesized stand-in.
	TenantSubjectID *identity.TenantSubjectID
	InvokerID       string
	CreditType      string
	Amount          int64
	Source          string
	SourceID        *uuid.UUID
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

	tx, err := s.db.BeginTenantTx(ctx)
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

// Hold reserves credits for prepay usage (issue #221: Reserve/Hold(payer) ->
// CaptureHold(actual) -> ReleaseHold(failure/zero)). payer is the tenant subject the
// reservation is charged against; when nil the payer is the invoker (invokerID)'s own
// account/personal tenant-subject UUID for the self-hosted / single-tenant personal case
// (never a synthesized stand-in).
func (s *CreditsService) Hold(ctx context.Context, payer *identity.TenantSubjectID, invokerID string, creditType string, amount int64, source, sourceID string, expiresAt time.Time) (*models.CreditTransaction, error) {
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

	tx, err := s.db.BeginTenantTx(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	now := s.now()
	tenantID := tenant.FromContextOrDefault(ctx).UUID()
	ownerOrg, err := resolveTenantSubject(payer, invokerID)
	if err != nil {
		return nil, err
	}
	ownerID := ownerOrg.UUID()
	if err := ensureTenantSubject(ctx, tx, tenantID, ownerID); err != nil {
		return nil, err
	}

	// Idempotency: treat (tenant, payer, credit_type, source, source_id) as a
	// unique key for holds. This prevents duplicate held_balance reservations on
	// caller retries.
	existing := new(models.CreditTransaction)
	err = tx.NewSelect().
		Model(existing).
		Where("transaction_type = 'hold'").
		Where("tenant_id = ? AND tenant_subject_id = ?", tenantID, ownerID).
		Where("credit_type_id = ?", ct.ID).
		Where("source = ? AND source_id = ?", source, sourceID).
		Limit(1).
		Scan(ctx)
	if err == nil {
		return existing, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	bal, err := s.lockBalance(ctx, tx, ownerOrg, invokerID, ct.ID)
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
		Where("tenant_id = ? AND tenant_subject_id = ? AND credit_type_id = ?", tenantID, ownerID, ct.ID).
		Exec(ctx); err != nil {
		return nil, err
	}

	exp := expiresAt.UTC()
	auth := amount
	hold := &models.CreditTransaction{
		ID:              uuidutil.NewV7(),
		TenantID:        tenantID,
		TenantSubjectID: ownerID,
		InvokerID:       invokerID,
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
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("credits service not initialized")
	}
	tx, err := s.db.BeginTenantTx(ctx)
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
	now := s.now()
	if hold.ExpiresAt != nil && !hold.ExpiresAt.After(now) {
		return nil, fmt.Errorf("hold is expired")
	}
	if hold.Authorized == nil || *hold.Authorized <= 0 {
		return nil, fmt.Errorf("hold missing authorized amount")
	}
	authorized := *hold.Authorized
	if actualAmount <= 0 || actualAmount > authorized {
		return nil, fmt.Errorf("invalid capture amount")
	}

	tenantID := tenant.FromContextOrDefault(ctx).UUID()
	holdPayer := identity.TenantSubjectID(hold.TenantSubjectID)
	bal, err := s.lockBalance(ctx, tx, holdPayer, hold.InvokerID, hold.CreditTypeID)
	if err != nil {
		return nil, err
	}
	if _, err := tx.NewUpdate().Model((*models.UserCreditBalance)(nil)).
		Set("held_balance = ?", bal.HeldBalance-authorized).
		Set("updated_at = ?", now).
		Where("tenant_id = ? AND tenant_subject_id = ? AND credit_type_id = ?", tenantID, hold.TenantSubjectID, hold.CreditTypeID).
		Exec(ctx); err != nil {
		return nil, err
	}

	// Settle the actual balance-first-then-owed (#302): an arrears hold spills the
	// uncovered remainder to outstanding_owed instead of failing. availableAfter
	// reflects this hold's reservation already released above.
	availableAfter := bal.Balance - (bal.HeldBalance - authorized)
	newBal, err := s.captureSettleTx(ctx, tx, holdPayer, hold.InvokerID, hold.CreditTypeID, actualAmount, availableAfter)
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

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return hold, nil
}

func (s *CreditsService) ReleaseHold(ctx context.Context, holdID uuid.UUID) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("credits service not initialized")
	}
	tx, err := s.db.BeginTenantTx(ctx)
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
	tenantID := tenant.FromContextOrDefault(ctx).UUID()
	holdPayer := identity.TenantSubjectID(hold.TenantSubjectID)
	bal, err := s.lockBalance(ctx, tx, holdPayer, hold.InvokerID, hold.CreditTypeID)
	if err != nil {
		return err
	}
	if _, err := tx.NewUpdate().Model((*models.UserCreditBalance)(nil)).
		Set("held_balance = ?", bal.HeldBalance-authorized).
		Set("updated_at = ?", now).
		Where("tenant_id = ? AND tenant_subject_id = ? AND credit_type_id = ?", tenantID, hold.TenantSubjectID, hold.CreditTypeID).
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

func (s *CreditsService) withdrawBalanceAndBlocks(ctx context.Context, tx bun.Tx, payer identity.TenantSubjectID, invokerID string, creditTypeID uuid.UUID, amount int64) (int64, error) {
	now := s.now()
	tenantID := tenant.FromContextOrDefault(ctx).UUID()
	ownerID := payer.UUID()
	bal, err := s.lockBalance(ctx, tx, payer, invokerID, creditTypeID)
	if err != nil {
		return 0, err
	}
	available := bal.Balance - bal.HeldBalance
	if available < amount {
		return 0, ErrInsufficientCredits
	}

	remaining := amount
	var blocks []models.CreditBlock
	if err := tx.NewSelect().Model(&blocks).
		Where("tenant_id = ? AND tenant_subject_id = ? AND credit_type_id = ? AND remaining_amount > 0 AND (expires_at IS NULL OR expires_at > ?)", tenantID, ownerID, creditTypeID, now).
		OrderExpr("expires_at ASC NULLS LAST, created_at ASC").
		For("UPDATE").
		Scan(ctx); err != nil {
		return 0, err
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
			return 0, err
		}
	}

	newBal := bal.Balance - amount
	if _, err := tx.NewUpdate().Model((*models.UserCreditBalance)(nil)).
		Set("balance = ?", newBal).
		Set("updated_at = ?", now).
		Where("tenant_id = ? AND tenant_subject_id = ? AND credit_type_id = ?", tenantID, ownerID, creditTypeID).
		Exec(ctx); err != nil {
		return 0, err
	}
	return newBal, nil
}

// lockBalance selects-for-update the balance row for an tenant subject (issue #221)
// within the resolved tenant (issue #223), creating it if absent. The balance
// is keyed by (tenant, payer, credit_type); invoker_id is stamped for attribution.
//
// The balance row is unique per (tenant_id, tenant_subject_id, credit_type_id) — the
// HARDCUT payer+tenant-scoped uniqueness (migration 040). ON CONFLICT DO NOTHING
// targets that constraint to avoid a duplicate-key error on concurrent
// first-touch, after which the row is re-selected FOR UPDATE.
func (s *CreditsService) lockBalance(ctx context.Context, tx bun.Tx, payer identity.TenantSubjectID, invokerID string, creditTypeID uuid.UUID) (*models.UserCreditBalance, error) {
	tenantID := tenant.FromContextOrDefault(ctx).UUID()
	ownerID := payer.UUID()
	bal := new(models.UserCreditBalance)
	err := tx.NewSelect().Model(bal).
		Where("tenant_id = ?", tenantID).
		Where("tenant_subject_id = ? AND credit_type_id = ?", ownerID, creditTypeID).
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
		ID:              uuidutil.NewV7(),
		TenantID:        tenantID,
		TenantSubjectID: ownerID,
		InvokerID:       invokerID,
		CreditTypeID:    creditTypeID,
		Balance:         0,
		HeldBalance:     0,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if _, err := tx.NewInsert().Model(bal).On("CONFLICT (tenant_id, tenant_subject_id, credit_type_id) DO NOTHING").Exec(ctx); err != nil {
		return nil, err
	}
	bal = new(models.UserCreditBalance)
	if err := tx.NewSelect().Model(bal).
		Where("tenant_id = ?", tenantID).
		Where("tenant_subject_id = ? AND credit_type_id = ?", ownerID, creditTypeID).
		For("UPDATE").
		Scan(ctx); err != nil {
		return nil, err
	}
	return bal, nil
}

func (s *CreditsService) withdrawTx(ctx context.Context, tx bun.Tx, creditTypeID uuid.UUID, params CreditWithdrawParams) (*models.CreditTransaction, error) {
	now := s.now()
	tenantID := tenant.FromContextOrDefault(ctx).UUID()
	payer, err := resolveTenantSubject(params.TenantSubjectID, params.InvokerID)
	if err != nil {
		return nil, err
	}
	ownerID := payer.UUID()
	sourceIDText := (*string)(nil)
	if params.SourceID != nil {
		if _, err := s.lockBalance(ctx, tx, payer, params.InvokerID, creditTypeID); err != nil {
			return nil, err
		}
		v := params.SourceID.String()
		sourceIDText = &v
		existing := new(models.CreditTransaction)
		err := tx.NewSelect().
			Model(existing).
			Where("tenant_id = ? AND tenant_subject_id = ?", tenantID, ownerID).
			Where("credit_type_id = ?", creditTypeID).
			Where("transaction_type = 'withdrawal'").
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
	newBal, err := s.withdrawBalanceAndBlocks(ctx, tx, payer, params.InvokerID, creditTypeID, params.Amount)
	if err != nil {
		return nil, err
	}

	trx := &models.CreditTransaction{
		ID:              uuidutil.NewV7(),
		TenantID:        tenantID,
		TenantSubjectID: ownerID,
		InvokerID:       params.InvokerID,
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
	return trx, nil
}
