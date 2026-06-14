package money

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/shared/timeutil"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/tenant"
)

var (
	ErrInsufficientCredits = errors.New("insufficient_credits")
)

type MoneyService struct {
	db    *db.DB
	clock clockwork.Clock
}

func NewMoneyService(database *db.DB, clocks ...clockwork.Clock) *MoneyService {
	return &MoneyService{db: database, clock: timeutil.FirstClock(clocks...)}
}

func (s *MoneyService) SetClock(c clockwork.Clock) {
	s.clock = timeutil.FirstClock(c)
}

func (s *MoneyService) Clock() clockwork.Clock {
	return s.clock
}

func (s *MoneyService) now() time.Time {
	if s.clock != nil {
		return s.clock.Now().UTC()
	}
	return time.Now().UTC()
}

// ErrTenantSubjectRequired is returned when a money operation cannot resolve a
// real payer tenant-subject id. HARDCUT (#221): tenant_subject_id is supplied by
// the caller and is NOT synthesized — there is no deterministic stand-in
// derivation. For the
// self-hosted / single-tenant personal case the tenant subject IS the authenticated
// user's own account/personal tenant-subject UUID, so a non-UUID actor with no explicit
// payer is a programming error rather than something to paper over.
var ErrTenantSubjectRequired = errors.New("tenant_subject_id required")

// resolveTenantSubject resolves the tenant subject for a money operation (issue #221, the
// payer/billing payer). When the caller supplies an explicit tenant subject it is
// used verbatim. Otherwise, for the self-hosted / single-tenant personal case,
// the payer is the actor's own account/personal tenant-subject id parsed from its UUID
// subject (identity.TenantSubjectIDFromString). It is NEVER a synthesized stand-in.
//
// The actorActorID is the existing free-form actor (who caused usage); it is
// retained for attribution and is NOT the financial payer. resolveTenantSubject returns
// ErrTenantSubjectRequired when no payer can be resolved.
func resolveTenantSubject(payer *identity.TenantSubjectID, actorActorID string) (identity.TenantSubjectID, error) {
	if payer != nil && !payer.IsZero() {
		return *payer, nil
	}
	resolved := identity.TenantSubjectIDFromString(actorActorID)
	if resolved.IsZero() {
		return identity.TenantSubjectID{}, ErrTenantSubjectRequired
	}
	return resolved, nil
}

func (s *MoneyService) GetBalance(ctx context.Context, actorID string) (*models.MoneyBalance, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	// Reads are scoped by tenant + OWNER (issue #221). For the self-hosted /
	// single-tenant personal case the payer IS the user's own account/personal tenant-subject
	// UUID — the same row the tenant-subject-owned writers stamp — so single-user reads return
	// the right balance. tenant_subject_id is never synthesized.
	payer, err := resolveTenantSubject(nil, actorID)
	if err != nil {
		return nil, err
	}
	return s.GetBalanceForTenantSubject(ctx, payer, DefaultCurrency)
}

// GetBalanceForTenantSubject reads a balance scoped explicitly by tenant subject (issue
// #221), for callers that own balances at a team org rather than a personal
// org. The actor string is not required for an payer-scoped read.
func (s *MoneyService) GetBalanceForTenantSubject(ctx context.Context, payer identity.TenantSubjectID, currency string) (*models.MoneyBalance, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	cur := normalizeCurrency(currency)
	if err := ValidateCurrency(cur); err != nil {
		return nil, err
	}
	tid, err := tenant.Require(ctx)
	if err != nil {
		return nil, err
	}
	tenantID := tid.UUID()
	payerID := payer.UUID()
	row, err := s.db.Gen(ctx).GetMoneyBalance(ctx, gen.GetMoneyBalanceParams{
		TenantID: tenantID, TenantSubjectID: payerID, Currency: cur,
	})
	if err == nil {
		return moneyBalanceFromGen(row), nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return &models.MoneyBalance{
			TenantID:        tenantID,
			TenantSubjectID: payerID,
			Currency:        cur,
			Balance:         0,
			HeldBalance:     0,
		}, nil
	}
	return nil, err
}

func (s *MoneyService) GetTransactions(ctx context.Context, actorID string, limit, offset int) ([]models.MoneyTransaction, int, error) {
	if s == nil || s.db == nil {
		return nil, 0, fmt.Errorf("money service not initialized")
	}
	payer, err := resolveTenantSubject(nil, actorID)
	if err != nil {
		return nil, 0, err
	}
	return s.GetTransactionsByTenantSubject(ctx, payer, DefaultCurrency, limit, offset)
}

// GetTransactionsByTenantSubject lists money transactions for an EXPLICIT tenant subject
// (the payer), newest first, paginated. Unlike GetTransactions it does not derive
// the payer from a actor id — it filters tenant_subject_id directly, which is what the
// org-level billing-account usage view (issue #242) needs. RLS-scoped to the
// request tenant via Qx(ctx).
func (s *MoneyService) GetTransactionsByTenantSubject(ctx context.Context, payer identity.TenantSubjectID, currency string, limit, offset int) ([]models.MoneyTransaction, int, error) {
	if s == nil || s.db == nil {
		return nil, 0, fmt.Errorf("money service not initialized")
	}
	if payer.IsZero() {
		return nil, 0, fmt.Errorf("payer required")
	}
	cur := normalizeCurrency(currency)
	if err := ValidateCurrency(cur); err != nil {
		return nil, 0, err
	}
	tid, err := tenant.Require(ctx)
	if err != nil {
		return nil, 0, err
	}
	tenantID := tid.UUID()
	payerID := payer.UUID()
	q := s.db.Gen(ctx)
	total, err := q.CountMoneyTransactionsByPayer(ctx, gen.CountMoneyTransactionsByPayerParams{
		TenantID: tenantID, TenantSubjectID: payerID, Currency: cur,
	})
	if err != nil {
		return nil, 0, err
	}
	if limit <= 0 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := q.ListMoneyTransactionsByPayer(ctx, gen.ListMoneyTransactionsByPayerParams{
		TenantID: tenantID, TenantSubjectID: payerID, Currency: cur,
		Column3: int32(limit), Column4: int32(offset),
	})
	if err != nil {
		return nil, 0, err
	}
	items := make([]models.MoneyTransaction, 0, len(rows))
	for _, r := range rows {
		m, merr := moneyTransactionFromGen(r)
		if merr != nil {
			return nil, 0, merr
		}
		items = append(items, *m)
	}
	return items, int(total), nil
}

// GetAccountSettingsForTenantSubject returns the stored money-account settings for an
// payer (billing mode, spend caps, auto-top-up, expiry default), RLS-scoped to
// the request tenant (issue #242). Never nil — missing rows return the defaults.
func (s *MoneyService) GetAccountSettingsForTenantSubject(ctx context.Context, payer identity.TenantSubjectID) (*models.MoneyAccount, error) {
	var out *models.MoneyAccount
	err := s.db.RunInTenantConn(ctx, func(ctx context.Context) error {
		st, e := s.GetAccountSettings(ctx, payer)
		if e != nil {
			return e
		}
		out = st
		return nil
	})
	return out, err
}

// GetTransactionBySource looks up a single money transaction by its idempotency key.
// For usage tracking, this is commonly used with transactionType="hold" and
// (source, sourceID) = (metering_source, request_id).
func (s *MoneyService) GetTransactionBySource(ctx context.Context, actorID string, currency string, transactionType string, source string, sourceID string) (*models.MoneyTransaction, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	actorID = strings.TrimSpace(actorID)
	transactionType = strings.TrimSpace(transactionType)
	source = strings.TrimSpace(source)
	sourceID = strings.TrimSpace(sourceID)
	if actorID == "" {
		return nil, fmt.Errorf("actor required")
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

	payer, err := resolveTenantSubject(nil, actorID)
	if err != nil {
		return nil, err
	}
	tid, err := tenant.Require(ctx)
	if err != nil {
		return nil, err
	}
	cur := normalizeCurrency(currency)
	if err := ValidateCurrency(cur); err != nil {
		return nil, err
	}
	row, err := s.db.Gen(ctx).GetMoneyTransactionByCoords(ctx, gen.GetMoneyTransactionByCoordsParams{
		TenantID:        tid.UUID(),
		TenantSubjectID: payer.UUID(),
		Currency:        cur,
		TransactionType: transactionType,
		Source:          source,
		SourceID:        &sourceID,
	})
	if err != nil {
		return nil, err
	}
	return moneyTransactionFromGen(row)
}

type DepositParams struct {
	// TenantSubjectID is the tenant subject that owns the deposited balance (issue #221). When
	// nil, the payer is the actor (Actor)'s own account/personal tenant-subject UUID for the
	// self-hosted / single-tenant personal case; it is never a synthesized
	// stand-in, and a non-UUID Actor with no explicit payer is rejected.
	TenantSubjectID *identity.TenantSubjectID
	Actor           string
	Currency        string // "" => DefaultCurrency (#472)
	Amount          int64
	Source          string
	SourceID        *uuid.UUID
	ExpiresAt       *time.Time
	// ApplyAccountExpiryDefault, when true and ExpiresAt is nil, sets the deposit's
	// expiry to now + the payer's money_accounts.default_credit_expiry_days
	// (issue #240). Purchase/top-up paths set this so bought funds expire (default
	// 365d); permanent grants (subscriptions, admin) leave it false. No-op when no
	// settings default is configured.
	ApplyAccountExpiryDefault bool
	Description               *string
}

func (s *MoneyService) Deposit(ctx context.Context, params DepositParams) (*models.MoneyTransaction, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	if params.Amount <= 0 {
		return nil, fmt.Errorf("amount must be positive")
	}

	// Apply the per-account default expiry (issue #240) when requested and the
	// caller did not set an explicit expiry.
	if params.ExpiresAt == nil && params.ApplyAccountExpiryDefault {
		if payer, oerr := resolveTenantSubject(params.TenantSubjectID, params.Actor); oerr == nil {
			if settings, serr := s.GetAccountSettings(ctx, payer); serr == nil &&
				settings.DefaultCreditExpiryDays != nil && *settings.DefaultCreditExpiryDays > 0 {
				exp := s.now().AddDate(0, 0, *settings.DefaultCreditExpiryDays)
				params.ExpiresAt = &exp
			}
		}
	}

	var trx *models.MoneyTransaction
	err := s.db.TenantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var terr error
		trx, terr = s.depositTx(ctx, gen.New(tx), params)
		return terr
	})
	if err != nil {
		return nil, err
	}
	return trx, nil
}

// ensureTenantSubject upserts the openrails.tenant_subjects row for a self-service
// subject so the money-write FKs added in migration 076 are satisfied on a
// subject's FIRST money operation (deposit/hold/usage). ON CONFLICT DO NOTHING.
func ensureTenantSubject(ctx context.Context, q *gen.Queries, tenantID, tsid uuid.UUID) error {
	if tsid == uuid.Nil {
		return nil
	}
	if tenantID == uuid.Nil {
		tid, err := tenant.Require(ctx)
		if err != nil {
			return err
		}
		tenantID = tid.UUID()
	}
	return q.EnsureTenantSubjectRow(ctx, gen.EnsureTenantSubjectRowParams{
		ID: tsid, TenantID: tenantID, Issuer: "openrails:self", Subject: tsid.String(),
	})
}

func (s *MoneyService) depositTx(ctx context.Context, q *gen.Queries, params DepositParams) (*models.MoneyTransaction, error) {
	now := s.now()
	cur := normalizeCurrency(params.Currency)
	if err := ValidateCurrency(cur); err != nil {
		return nil, err
	}
	tid, err := tenant.Require(ctx)
	if err != nil {
		return nil, err
	}
	tenantID := tid.UUID()
	payer, err := resolveTenantSubject(params.TenantSubjectID, params.Actor)
	if err != nil {
		return nil, err
	}
	payerID := payer.UUID()
	if err := ensureTenantSubject(ctx, q, tenantID, payerID); err != nil {
		return nil, err
	}
	bal, err := s.lockBalance(ctx, q, payer, params.Actor, cur)
	if err != nil {
		return nil, err
	}

	// Idempotency: if caller provides SourceID, treat (tenant, payer, source,
	// source_id) as an idempotency key for deposits. This is safe because
	// we lock the balance row for UPDATE, serializing deposits per payer.
	sourceIDText := (*string)(nil)
	if params.SourceID != nil {
		v := params.SourceID.String()
		sourceIDText = &v
		existing, gerr := q.GetMoneyTransactionByCoords(ctx, gen.GetMoneyTransactionByCoordsParams{
			TenantID: tenantID, TenantSubjectID: payerID, Currency: cur,
			TransactionType: "deposit", Source: params.Source, SourceID: &v,
		})
		if gerr == nil {
			return moneyTransactionFromGen(existing)
		}
		if !errors.Is(gerr, pgx.ErrNoRows) {
			return nil, gerr
		}
	}

	// Guard against int64 overflow: a sufficiently large deposit on a user with
	// a positive balance would wrap the balance negative, permanently bricking
	// the account. Reject the deposit rather than silently corrupting it.
	if bal.Balance > math.MaxInt64-params.Amount {
		return nil, fmt.Errorf("deposit would overflow balance")
	}
	newBal := bal.Balance + params.Amount

	if err := q.SetMoneyBalance(ctx, gen.SetMoneyBalanceParams{
		TenantID: tenantID, TenantSubjectID: payerID, Currency: cur,
		Balance: newBal, UpdatedAt: now,
	}); err != nil {
		return nil, err
	}

	trx := &models.MoneyTransaction{
		ID:              uuidutil.NewV7(),
		TenantID:        tenantID,
		TenantSubjectID: payerID,
		Currency:        cur,
		Actor:           params.Actor,
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
	if err := q.InsertMoneyTransaction(ctx, insertParamsFromTransaction(trx)); err != nil {
		return nil, err
	}
	if err := q.InsertMoneyBlock(ctx, gen.InsertMoneyBlockParams{
		ID:                  uuidutil.NewV7(),
		TenantID:            tenantID,
		TenantSubjectID:     payerID,
		Currency:            cur,
		OriginalAmount:      params.Amount,
		RemainingAmount:     params.Amount,
		ExpiresAt:           params.ExpiresAt,
		SourceTransactionID: &trx.ID,
		CreatedAt:           now,
	}); err != nil {
		return nil, err
	}

	// AUTO-graduation (#476): cumulative_paid just changed, so recompute + persist
	// the payer's tier from the stored tier_schedule, in-band with the deposit.
	// Only the trust-signal currency (DefaultCurrency) graduates; a no-op when no
	// schedule exists or the tier is an admin override. Best-effort within the
	// tx: a schedule lookup error fails the deposit (consistency), but no schedule
	// is the common path and costs one indexed read.
	if cur == DefaultCurrency {
		cumPaid, perr := q.SumMoneyDeposits(ctx, gen.SumMoneyDepositsParams{
			TenantID: tenantID, TenantSubjectID: payerID, Currency: DefaultCurrency,
		})
		if perr != nil {
			return nil, perr
		}
		if err := s.autoGraduateTierTx(ctx, q, tenantID, payer, cumPaid, now); err != nil {
			return nil, err
		}
	}

	return trx, nil
}

// insertParamsFromTransaction maps a fully-populated transaction model onto the
// generated insert params (one definition shared by every ledger writer).
// Metadata is dropped: no current ledger writer sets it (windows/settles stamp
// resource only), and an encode error has nowhere to go from this mapper.
func insertParamsFromTransaction(t *models.MoneyTransaction) gen.InsertMoneyTransactionParams {
	return gen.InsertMoneyTransactionParams{
		ID:               t.ID,
		TenantID:         t.TenantID,
		TenantSubjectID:  t.TenantSubjectID,
		Currency:         normalizeCurrency(t.Currency),
		Actor:            t.Actor,
		Resource:         t.Resource,
		Amount:           t.Amount,
		BalanceAfter:     t.BalanceAfter,
		TransactionType:  t.TransactionType,
		Status:           t.Status,
		AuthorizedAmount: t.Authorized,
		CapturedAmount:   t.Captured,
		Source:           t.Source,
		SourceID:         t.SourceID,
		ExpiresAt:        t.ExpiresAt,
		Description:      t.Description,
		CreatedAt:        t.CreatedAt,
		UpdatedAt:        t.UpdatedAt,
	}
}

type WithdrawParams struct {
	// TenantSubjectID is the tenant subject to withdraw from (issue #221). When nil, the payer
	// is the actor (Actor)'s own account/personal tenant-subject UUID for the self-hosted /
	// single-tenant personal case; it is never a synthesized stand-in.
	TenantSubjectID *identity.TenantSubjectID
	Actor           string
	Currency        string // "" => DefaultCurrency (#472)
	Amount          int64
	Source          string
	SourceID        *uuid.UUID
}

func (s *MoneyService) Withdraw(ctx context.Context, params WithdrawParams) (*models.MoneyTransaction, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	if params.Amount <= 0 {
		return nil, fmt.Errorf("amount must be positive")
	}

	var trx *models.MoneyTransaction
	err := s.db.TenantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var terr error
		trx, terr = s.withdrawTx(ctx, gen.New(tx), params)
		return terr
	})
	if err != nil {
		return nil, err
	}
	return trx, nil
}

// Hold reserves money for prepay usage (issue #221: Reserve/Hold(payer) ->
// CaptureHold(actual) -> ReleaseHold(failure/zero)). payer is the tenant subject the
// reservation is charged against; when nil the payer is the actor (actorID)'s own
// account/personal tenant-subject UUID for the self-hosted / single-tenant personal case
// (never a synthesized stand-in).
func (s *MoneyService) Hold(ctx context.Context, payer *identity.TenantSubjectID, actorID, currency string, amount int64, source, sourceID string, expiresAt time.Time) (*models.MoneyTransaction, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	if amount <= 0 {
		return nil, fmt.Errorf("amount must be positive")
	}
	cur := normalizeCurrency(currency)
	if err := ValidateCurrency(cur); err != nil {
		return nil, err
	}

	var hold *models.MoneyTransaction
	err := s.db.TenantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		now := s.now()
		tid, terr := tenant.Require(ctx)
		if terr != nil {
			return terr
		}
		tenantID := tid.UUID()
		payerSubject, rerr := resolveTenantSubject(payer, actorID)
		if rerr != nil {
			return rerr
		}
		payerID := payerSubject.UUID()
		if err := ensureTenantSubject(ctx, q, tenantID, payerID); err != nil {
			return err
		}

		// Idempotency: treat (tenant, payer, source, source_id) as a unique key for
		// holds. This prevents duplicate held_balance reservations on caller retries.
		existing, gerr := q.GetMoneyTransactionByCoords(ctx, gen.GetMoneyTransactionByCoordsParams{
			TenantID: tenantID, TenantSubjectID: payerID, Currency: cur,
			TransactionType: "hold", Source: source, SourceID: &sourceID,
		})
		if gerr == nil {
			hold, gerr = moneyTransactionFromGen(existing)
			return gerr
		}
		if !errors.Is(gerr, pgx.ErrNoRows) {
			return gerr
		}

		bal, lerr := s.lockBalance(ctx, q, payerSubject, actorID, cur)
		if lerr != nil {
			return lerr
		}
		available := bal.Balance - bal.HeldBalance
		if available < amount {
			return ErrInsufficientCredits
		}

		if err := q.SetMoneyHeldBalance(ctx, gen.SetMoneyHeldBalanceParams{
			TenantID: tenantID, TenantSubjectID: payerID, Currency: cur,
			HeldBalance: bal.HeldBalance + amount, UpdatedAt: now,
		}); err != nil {
			return err
		}

		exp := expiresAt.UTC()
		auth := amount
		srcID := sourceID
		hold = &models.MoneyTransaction{
			ID:              uuidutil.NewV7(),
			TenantID:        tenantID,
			TenantSubjectID: payerID,
			Currency:        cur,
			Actor:           actorID,
			Amount:          0,
			Source:          source,
			SourceID:        &srcID,
			Status:          "active",
			Authorized:      &auth,
			ExpiresAt:       &exp,
			TransactionType: "hold",
			CreatedAt:       now,
			UpdatedAt:       now,
		}
		return q.InsertMoneyTransaction(ctx, insertParamsFromTransaction(hold))
	})
	if err != nil {
		return nil, err
	}
	return hold, nil
}

func (s *MoneyService) CaptureHold(ctx context.Context, holdID uuid.UUID, actualAmount int64) (*models.MoneyTransaction, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	var hold *models.MoneyTransaction
	err := s.db.TenantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		row, gerr := q.GetMoneyTransactionForUpdate(ctx, holdID)
		if gerr != nil {
			return gerr
		}
		h, gerr := moneyTransactionFromGen(row)
		if gerr != nil {
			return gerr
		}
		if h.TransactionType != "hold" {
			return fmt.Errorf("transaction is not a hold")
		}
		if h.Status != "active" {
			return fmt.Errorf("hold is not active")
		}
		now := s.now()
		if h.ExpiresAt != nil && !h.ExpiresAt.After(now) {
			return fmt.Errorf("hold is expired")
		}
		if h.Authorized == nil || *h.Authorized <= 0 {
			return fmt.Errorf("hold missing authorized amount")
		}
		authorized := *h.Authorized
		if actualAmount <= 0 || actualAmount > authorized {
			return fmt.Errorf("invalid capture amount")
		}

		tid, terr := tenant.Require(ctx)
		if terr != nil {
			return terr
		}
		tenantID := tid.UUID()
		holdPayer := identity.TenantSubjectID(h.TenantSubjectID)
		cur := normalizeCurrency(h.Currency)
		bal, lerr := s.lockBalance(ctx, q, holdPayer, h.Actor, cur)
		if lerr != nil {
			return lerr
		}
		if err := q.SetMoneyHeldBalance(ctx, gen.SetMoneyHeldBalanceParams{
			TenantID: tenantID, TenantSubjectID: h.TenantSubjectID, Currency: cur,
			HeldBalance: bal.HeldBalance - authorized, UpdatedAt: now,
		}); err != nil {
			return err
		}

		// Settle the actual balance-first-then-owed (#302): an arrears hold spills the
		// uncovered remainder to outstanding_owed instead of failing. availableAfter
		// reflects this hold's reservation already released above.
		availableAfter := bal.Balance - (bal.HeldBalance - authorized)
		newBal, serr := s.captureSettleTx(ctx, q, holdPayer, h.Actor, cur, actualAmount, availableAfter)
		if serr != nil {
			return serr
		}
		h.Status = "captured"
		h.Captured = &actualAmount
		h.Amount = -actualAmount
		h.BalanceAfter = &newBal
		h.UpdatedAt = now
		if err := q.CaptureMoneyHold(ctx, gen.CaptureMoneyHoldParams{
			ID: h.ID, CapturedAmount: &actualAmount, Amount: -actualAmount,
			BalanceAfter: &newBal, UpdatedAt: now,
		}); err != nil {
			return err
		}
		hold = h
		return nil
	})
	if err != nil {
		return nil, err
	}
	return hold, nil
}

// ReleaseHold frees an active hold and returns the released transaction (the
// caller uses its (payer, source, source_id) coords to settle any composed
// budget reservations — #473).
func (s *MoneyService) ReleaseHold(ctx context.Context, holdID uuid.UUID) (*models.MoneyTransaction, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	var released *models.MoneyTransaction
	err := s.db.TenantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		row, gerr := q.GetMoneyTransactionForUpdate(ctx, holdID)
		if gerr != nil {
			return gerr
		}
		if row.TransactionType != "hold" {
			return fmt.Errorf("transaction is not a hold")
		}
		if row.Status != "active" {
			return fmt.Errorf("hold is not active")
		}
		if row.AuthorizedAmount == nil || *row.AuthorizedAmount <= 0 {
			return fmt.Errorf("hold missing authorized amount")
		}
		authorized := *row.AuthorizedAmount

		now := s.now()
		tid, terr := tenant.Require(ctx)
		if terr != nil {
			return terr
		}
		tenantID := tid.UUID()
		holdPayer := identity.TenantSubjectID(row.TenantSubjectID)
		cur := normalizeCurrency(row.Currency)
		bal, lerr := s.lockBalance(ctx, q, holdPayer, row.Actor, cur)
		if lerr != nil {
			return lerr
		}
		if err := q.SetMoneyHeldBalance(ctx, gen.SetMoneyHeldBalanceParams{
			TenantID: tenantID, TenantSubjectID: row.TenantSubjectID, Currency: cur,
			HeldBalance: bal.HeldBalance - authorized, UpdatedAt: now,
		}); err != nil {
			return err
		}
		if err := q.ReleaseMoneyHold(ctx, gen.ReleaseMoneyHoldParams{ID: row.ID, UpdatedAt: now}); err != nil {
			return err
		}
		h, merr := moneyTransactionFromGen(row)
		if merr != nil {
			return merr
		}
		h.Status = "released"
		h.UpdatedAt = now
		released = h
		return nil
	})
	if err != nil {
		return nil, err
	}
	return released, nil
}

func (s *MoneyService) withdrawBalanceAndBlocks(ctx context.Context, q *gen.Queries, payer identity.TenantSubjectID, actorID, currency string, amount int64) (int64, error) {
	now := s.now()
	cur := normalizeCurrency(currency)
	tid, err := tenant.Require(ctx)
	if err != nil {
		return 0, err
	}
	tenantID := tid.UUID()
	payerID := payer.UUID()
	bal, err := s.lockBalance(ctx, q, payer, actorID, cur)
	if err != nil {
		return 0, err
	}
	available := bal.Balance - bal.HeldBalance
	if available < amount {
		return 0, ErrInsufficientCredits
	}

	remaining := amount
	blocks, err := q.ListSpendableMoneyBlocksForUpdate(ctx, gen.ListSpendableMoneyBlocksForUpdateParams{
		TenantID: tenantID, TenantSubjectID: payerID, Currency: cur, Now: now,
	})
	if err != nil {
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
		remaining -= use
		if err := q.SetMoneyBlockRemaining(ctx, gen.SetMoneyBlockRemainingParams{
			ID: blocks[i].ID, RemainingAmount: blocks[i].RemainingAmount - use,
		}); err != nil {
			return 0, err
		}
	}

	newBal := bal.Balance - amount
	if err := q.SetMoneyBalance(ctx, gen.SetMoneyBalanceParams{
		TenantID: tenantID, TenantSubjectID: payerID, Currency: cur,
		Balance: newBal, UpdatedAt: now,
	}); err != nil {
		return 0, err
	}
	return newBal, nil
}

// lockBalance selects-for-update the balance row for an tenant subject (issue #221)
// within the resolved tenant (issue #223), creating it if absent. The balance
// is keyed by (tenant, payer); actor is stamped for attribution.
//
// The balance row is unique per (tenant_id, tenant_subject_id) — the HARDCUT
// payer+tenant-scoped uniqueness (001_schema.up.sql). ON CONFLICT DO NOTHING
// targets that constraint to avoid a duplicate-key error on concurrent
// first-touch, after which the row is re-selected FOR UPDATE.
func (s *MoneyService) lockBalance(ctx context.Context, q *gen.Queries, payer identity.TenantSubjectID, actorID, currency string) (*models.MoneyBalance, error) {
	cur := normalizeCurrency(currency)
	tid, err := tenant.Require(ctx)
	if err != nil {
		return nil, err
	}
	tenantID := tid.UUID()
	payerID := payer.UUID()
	key := gen.LockMoneyBalanceParams{
		TenantID: tenantID, TenantSubjectID: payerID, Currency: cur,
	}
	row, err := q.LockMoneyBalance(ctx, key)
	if err == nil {
		return moneyBalanceFromGen(row), nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}

	if err := q.InsertMoneyBalanceIfAbsent(ctx, gen.InsertMoneyBalanceIfAbsentParams{
		ID: uuidutil.NewV7(), TenantID: tenantID, TenantSubjectID: payerID, Currency: cur,
		Now: s.now(),
	}); err != nil {
		return nil, err
	}
	row, err = q.LockMoneyBalance(ctx, key)
	if err != nil {
		return nil, err
	}
	return moneyBalanceFromGen(row), nil
}

func (s *MoneyService) withdrawTx(ctx context.Context, q *gen.Queries, params WithdrawParams) (*models.MoneyTransaction, error) {
	now := s.now()
	cur := normalizeCurrency(params.Currency)
	if err := ValidateCurrency(cur); err != nil {
		return nil, err
	}
	tid, err := tenant.Require(ctx)
	if err != nil {
		return nil, err
	}
	tenantID := tid.UUID()
	payer, err := resolveTenantSubject(params.TenantSubjectID, params.Actor)
	if err != nil {
		return nil, err
	}
	payerID := payer.UUID()
	sourceIDText := (*string)(nil)
	if params.SourceID != nil {
		if _, err := s.lockBalance(ctx, q, payer, params.Actor, cur); err != nil {
			return nil, err
		}
		v := params.SourceID.String()
		sourceIDText = &v
		existing, gerr := q.GetMoneyTransactionByCoords(ctx, gen.GetMoneyTransactionByCoordsParams{
			TenantID: tenantID, TenantSubjectID: payerID, Currency: cur,
			TransactionType: "withdrawal", Source: params.Source, SourceID: &v,
		})
		if gerr == nil {
			return moneyTransactionFromGen(existing)
		}
		if !errors.Is(gerr, pgx.ErrNoRows) {
			return nil, gerr
		}
	}
	newBal, err := s.withdrawBalanceAndBlocks(ctx, q, payer, params.Actor, cur, params.Amount)
	if err != nil {
		return nil, err
	}

	trx := &models.MoneyTransaction{
		ID:              uuidutil.NewV7(),
		TenantID:        tenantID,
		TenantSubjectID: payerID,
		Currency:        cur,
		Actor:           params.Actor,
		Amount:          -params.Amount,
		BalanceAfter:    &newBal,
		TransactionType: "withdrawal",
		Status:          "posted",
		Source:          params.Source,
		SourceID:        sourceIDText,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if err := q.InsertMoneyTransaction(ctx, insertParamsFromTransaction(trx)); err != nil {
		return nil, err
	}
	return trx, nil
}
