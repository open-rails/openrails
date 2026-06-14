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
	"github.com/open-rails/openrails/pkg/merchant"
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

// ErrCustomerRequired is returned when a money operation cannot resolve a
// real payer tenant-subject id. HARDCUT (#221): customer_id is supplied by
// the caller and is NOT synthesized — there is no deterministic stand-in
// derivation. For the
// self-hosted / single-tenant personal case the tenant subject IS the authenticated
// user's own account/personal tenant-subject UUID, so a non-UUID actor with no explicit
// payer is a programming error rather than something to paper over.
var ErrCustomerRequired = errors.New("customer_id required")

// resolveCustomer resolves the tenant subject for a money operation (issue #221, the
// payer/billing payer). When the caller supplies an explicit tenant subject it is
// used verbatim. Otherwise, for the self-hosted / single-tenant personal case,
// the payer is the actor's own account/personal tenant-subject id parsed from its UUID
// subject (identity.CustomerIDFromString). It is NEVER a synthesized stand-in.
//
// The actorActorID is the existing free-form actor (who caused usage); it is
// retained for attribution and is NOT the financial payer. resolveCustomer returns
// ErrCustomerRequired when no payer can be resolved.
func resolveCustomer(payer *identity.CustomerID, actorActorID string) (identity.CustomerID, error) {
	if payer != nil && !payer.IsZero() {
		return *payer, nil
	}
	resolved := identity.CustomerIDFromString(actorActorID)
	if resolved.IsZero() {
		return identity.CustomerID{}, ErrCustomerRequired
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
	// the right balance. customer_id is never synthesized.
	payer, err := resolveCustomer(nil, actorID)
	if err != nil {
		return nil, err
	}
	return s.GetBalanceForCustomer(ctx, payer, DefaultCurrency)
}

// GetBalanceForCustomer reads a balance scoped explicitly by tenant subject (issue
// #221), for callers that own balances at a team org rather than a personal
// org. The actor string is not required for an payer-scoped read.
func (s *MoneyService) GetBalanceForCustomer(ctx context.Context, payer identity.CustomerID, currency string) (*models.MoneyBalance, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	cur := normalizeUnit(currency)
	if err := s.validateUnit(ctx, cur); err != nil {
		return nil, err
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	tenantID := tid.UUID()
	payerID := payer.UUID()
	// Derived read (#491): balance = SUM spendable blocks, held = active holds +
	// open windows. No lock needed — a stale read can never overdraft (writers
	// re-derive under the customers-row lock).
	return s.deriveBalance(ctx, s.db.Gen(ctx), tenantID, payerID, cur)
}

func (s *MoneyService) GetTransactions(ctx context.Context, actorID string, limit, offset int) ([]models.MoneyTransaction, int, error) {
	if s == nil || s.db == nil {
		return nil, 0, fmt.Errorf("money service not initialized")
	}
	payer, err := resolveCustomer(nil, actorID)
	if err != nil {
		return nil, 0, err
	}
	return s.GetTransactionsByCustomer(ctx, payer, DefaultCurrency, limit, offset)
}

// GetTransactionsByCustomer lists money transactions for an EXPLICIT tenant subject
// (the payer), newest first, paginated. Unlike GetTransactions it does not derive
// the payer from a actor id — it filters customer_id directly, which is what the
// org-level billing-account usage view (issue #242) needs. RLS-scoped to the
// request tenant via Qx(ctx).
func (s *MoneyService) GetTransactionsByCustomer(ctx context.Context, payer identity.CustomerID, currency string, limit, offset int) ([]models.MoneyTransaction, int, error) {
	if s == nil || s.db == nil {
		return nil, 0, fmt.Errorf("money service not initialized")
	}
	if payer.IsZero() {
		return nil, 0, fmt.Errorf("payer required")
	}
	cur := normalizeUnit(currency)
	if err := s.validateUnit(ctx, cur); err != nil {
		return nil, 0, err
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, 0, err
	}
	tenantID := tid.UUID()
	payerID := payer.UUID()
	q := s.db.Gen(ctx)
	total, err := q.CountMoneyTransactionsByPayer(ctx, gen.CountMoneyTransactionsByPayerParams{
		MerchantID: tenantID, CustomerID: payerID, Currency: cur,
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
		MerchantID: tenantID, CustomerID: payerID, Currency: cur,
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

// GetAccountSettingsForCustomer returns the stored money-account settings for an
// payer (billing mode, spend caps, auto-top-up, expiry default), RLS-scoped to
// the request tenant (issue #242). Never nil — missing rows return the defaults.
func (s *MoneyService) GetAccountSettingsForCustomer(ctx context.Context, payer identity.CustomerID) (*models.MoneyAccount, error) {
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

	payer, err := resolveCustomer(nil, actorID)
	if err != nil {
		return nil, err
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	cur := normalizeUnit(currency)
	if err := s.validateUnit(ctx, cur); err != nil {
		return nil, err
	}
	row, err := s.db.Gen(ctx).GetMoneyTransactionByCoords(ctx, gen.GetMoneyTransactionByCoordsParams{
		MerchantID:      tid.UUID(),
		CustomerID:      payer.UUID(),
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
	// CustomerID is the tenant subject that owns the deposited balance (issue #221). When
	// nil, the payer is the actor (Actor)'s own account/personal tenant-subject UUID for the
	// self-hosted / single-tenant personal case; it is never a synthesized
	// stand-in, and a non-UUID Actor with no explicit payer is rejected.
	CustomerID *identity.CustomerID
	Actor      string
	Currency   string // "" => DefaultCurrency (#472)
	Amount     int64
	Source     string
	SourceID   *uuid.UUID
	ExpiresAt  *time.Time
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
		if payer, oerr := resolveCustomer(params.CustomerID, params.Actor); oerr == nil {
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

// ensureCustomer upserts the openrails.customers row for a payable customer id
// so the money-write FKs are satisfied on a customer's FIRST money operation
// (deposit/hold/usage). customers is UUID-only (#491). ON CONFLICT DO NOTHING.
func ensureCustomer(ctx context.Context, q *gen.Queries, tenantID, tsid uuid.UUID) error {
	if tsid == uuid.Nil {
		return nil
	}
	if tenantID == uuid.Nil {
		tid, err := merchant.Require(ctx)
		if err != nil {
			return err
		}
		tenantID = tid.UUID()
	}
	return q.EnsureCustomerRow(ctx, gen.EnsureCustomerRowParams{
		ID: tsid, MerchantID: tenantID,
	})
}

func (s *MoneyService) depositTx(ctx context.Context, q *gen.Queries, params DepositParams) (*models.MoneyTransaction, error) {
	now := s.now()
	cur := normalizeUnit(params.Currency)
	if err := s.validateUnit(ctx, cur); err != nil {
		return nil, err
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	tenantID := tid.UUID()
	payer, err := resolveCustomer(params.CustomerID, params.Actor)
	if err != nil {
		return nil, err
	}
	payerID := payer.UUID()
	bal, err := s.lockBalance(ctx, q, payer, params.Actor, cur)
	if err != nil {
		return nil, err
	}

	// Idempotency: if caller provides SourceID, treat (tenant, payer, source,
	// source_id) as an idempotency key for deposits. This is safe because we hold
	// the customers-row lock, serializing deposits per customer (#491).
	sourceIDText := (*string)(nil)
	if params.SourceID != nil {
		v := params.SourceID.String()
		sourceIDText = &v
		existing, gerr := q.GetMoneyTransactionByCoords(ctx, gen.GetMoneyTransactionByCoordsParams{
			MerchantID: tenantID, CustomerID: payerID, Currency: cur,
			TransactionType: "deposit", Source: params.Source, SourceID: &v,
		})
		if gerr == nil {
			return moneyTransactionFromGen(existing)
		}
		if !errors.Is(gerr, pgx.ErrNoRows) {
			return nil, gerr
		}
	}

	// Guard against int64 overflow: a sufficiently large deposit on a customer
	// with a positive balance would wrap the derived balance negative. Reject the
	// deposit rather than corrupt the spendable total.
	if bal.Balance > math.MaxInt64-params.Amount {
		return nil, fmt.Errorf("deposit would overflow balance")
	}
	newBal := bal.Balance + params.Amount

	trx := &models.MoneyTransaction{
		ID:              uuidutil.NewV7(),
		MerchantID:      tenantID,
		CustomerID:      payerID,
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
		MerchantID:          tenantID,
		CustomerID:          payerID,
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
			MerchantID: tenantID, CustomerID: payerID, Currency: DefaultCurrency,
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
		MerchantID:       t.MerchantID,
		CustomerID:       t.CustomerID,
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
	// CustomerID is the tenant subject to withdraw from (issue #221). When nil, the payer
	// is the actor (Actor)'s own account/personal tenant-subject UUID for the self-hosted /
	// single-tenant personal case; it is never a synthesized stand-in.
	CustomerID *identity.CustomerID
	Actor      string
	Currency   string // "" => DefaultCurrency (#472)
	Amount     int64
	Source     string
	SourceID   *uuid.UUID
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
func (s *MoneyService) Hold(ctx context.Context, payer *identity.CustomerID, actorID, currency string, amount int64, source, sourceID string, expiresAt time.Time) (*models.MoneyTransaction, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	if amount <= 0 {
		return nil, fmt.Errorf("amount must be positive")
	}
	cur := normalizeUnit(currency)
	if err := s.validateUnit(ctx, cur); err != nil {
		return nil, err
	}

	var hold *models.MoneyTransaction
	err := s.db.TenantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		now := s.now()
		tid, terr := merchant.Require(ctx)
		if terr != nil {
			return terr
		}
		tenantID := tid.UUID()
		customerSubject, rerr := resolveCustomer(payer, actorID)
		if rerr != nil {
			return rerr
		}
		payerID := customerSubject.UUID()

		// Idempotency: treat (tenant, payer, source, source_id) as a unique key for
		// holds. This prevents duplicate reservations on caller retries.
		existing, gerr := q.GetMoneyTransactionByCoords(ctx, gen.GetMoneyTransactionByCoordsParams{
			MerchantID: tenantID, CustomerID: payerID, Currency: cur,
			TransactionType: "hold", Source: source, SourceID: &sourceID,
		})
		if gerr == nil {
			hold, gerr = moneyTransactionFromGen(existing)
			return gerr
		}
		if !errors.Is(gerr, pgx.ErrNoRows) {
			return gerr
		}

		// Lock the customers row, then check derived available. Inserting the active
		// hold row IS the held reservation (#491): SumActiveMoneyHeld picks it up.
		bal, lerr := s.lockBalance(ctx, q, customerSubject, actorID, cur)
		if lerr != nil {
			return lerr
		}
		available := bal.Balance - bal.HeldBalance
		if available < amount {
			return ErrInsufficientCredits
		}

		exp := expiresAt.UTC()
		auth := amount
		srcID := sourceID
		hold = &models.MoneyTransaction{
			ID:              uuidutil.NewV7(),
			MerchantID:      tenantID,
			CustomerID:      payerID,
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

		holdPayer := identity.CustomerID(h.CustomerID)
		cur := normalizeCurrency(h.Currency)
		bal, lerr := s.lockBalance(ctx, q, holdPayer, h.Actor, cur)
		if lerr != nil {
			return lerr
		}

		// Settle the actual balance-first-then-owed (#302): an arrears hold spills the
		// uncovered remainder to outstanding_owed instead of failing. The hold is
		// still 'active' here (CaptureMoneyHold runs below) so bal.HeldBalance still
		// counts it; availableAfter is the available once THIS hold is released
		// (#491: releasing held = the status flip to 'captured', no cache write).
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

		now := s.now()
		holdPayer := identity.CustomerID(row.CustomerID)
		cur := normalizeCurrency(row.Currency)
		// Lock the customers row to serialize against concurrent spend; the status
		// flip to 'released' IS the held release (#491), no cache write.
		if _, lerr := s.lockBalance(ctx, q, holdPayer, row.Actor, cur); lerr != nil {
			return lerr
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

func (s *MoneyService) withdrawBalanceAndBlocks(ctx context.Context, q *gen.Queries, payer identity.CustomerID, actorID, currency string, amount int64) (int64, error) {
	now := s.now()
	cur := normalizeUnit(currency)
	tid, err := merchant.Require(ctx)
	if err != nil {
		return 0, err
	}
	tenantID := tid.UUID()
	payerID := payer.UUID()
	// Lock + serialize. This is a low-level FIFO block-debit primitive: the
	// available/credit-line gate is the CALLER's job (#491). It draws the spendable
	// lots and errors only if the blocks physically cannot cover `amount` (a gated
	// caller never hits that). Returns the derived balance AFTER the debit.
	bal, err := s.lockBalance(ctx, q, payer, actorID, cur)
	if err != nil {
		return 0, err
	}
	if bal.Balance < amount {
		return 0, ErrInsufficientCredits
	}

	remaining := amount
	blocks, err := q.ListSpendableMoneyBlocksForUpdate(ctx, gen.ListSpendableMoneyBlocksForUpdateParams{
		MerchantID: tenantID, CustomerID: payerID, Currency: cur, Now: now,
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
	if remaining > 0 {
		// Blocks couldn't cover it despite the balance check — should be impossible
		// under the customers-row lock; fail loudly rather than under-debit.
		return 0, ErrInsufficientCredits
	}
	return bal.Balance - amount, nil
}

// lockBalance is the per-customer spend mutex (#491): it FOR UPDATE-locks the
// customers row (the serialization point — money_balances is gone), ensuring the
// row exists first, then returns the DERIVED balance snapshot
// (Balance = SUM spendable blocks, HeldBalance = active holds + open windows)
// computed UNDER the lock. Every spend/hold/capture/deposit/expiry path calls
// this before reading/mutating the customer's blocks so no two mutations on the
// same customer interleave (no overdraft, atomic hold placement).
func (s *MoneyService) lockBalance(ctx context.Context, q *gen.Queries, payer identity.CustomerID, actorID, currency string) (*models.MoneyBalance, error) {
	cur := normalizeUnit(currency)
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	tenantID := tid.UUID()
	payerID := payer.UUID()
	if err := ensureCustomer(ctx, q, tenantID, payerID); err != nil {
		return nil, err
	}
	if _, err := q.LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{
		ID: payerID, MerchantID: tenantID,
	}); err != nil {
		return nil, err
	}
	return s.deriveBalance(ctx, q, tenantID, payerID, cur)
}

// deriveBalance computes the balance snapshot from the source of truth: balance
// = SUM(unexpired, unspent money_blocks), held = active holds + open windows. The
// caller holds the customers-row lock (#491).
func (s *MoneyService) deriveBalance(ctx context.Context, q *gen.Queries, tenantID, payerID uuid.UUID, cur string) (*models.MoneyBalance, error) {
	now := s.now()
	bal, err := q.SumSpendableMoneyBlocks(ctx, gen.SumSpendableMoneyBlocksParams{
		MerchantID: tenantID, CustomerID: payerID, Currency: cur, Now: now,
	})
	if err != nil {
		return nil, err
	}
	held, err := q.SumActiveMoneyHeld(ctx, gen.SumActiveMoneyHeldParams{
		MerchantID: tenantID, CustomerID: payerID, Currency: cur,
	})
	if err != nil {
		return nil, err
	}
	return &models.MoneyBalance{
		MerchantID:  tenantID,
		CustomerID:  payerID,
		Currency:    cur,
		Balance:     bal,
		HeldBalance: held,
	}, nil
}

func (s *MoneyService) withdrawTx(ctx context.Context, q *gen.Queries, params WithdrawParams) (*models.MoneyTransaction, error) {
	now := s.now()
	cur := normalizeUnit(params.Currency)
	if err := s.validateUnit(ctx, cur); err != nil {
		return nil, err
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	tenantID := tid.UUID()
	payer, err := resolveCustomer(params.CustomerID, params.Actor)
	if err != nil {
		return nil, err
	}
	payerID := payer.UUID()
	// Lock + derive once: gate on held-aware available (a withdraw cannot eat into
	// reserved/held funds), then idempotency, then debit the FIFO blocks (#491).
	bal, err := s.lockBalance(ctx, q, payer, params.Actor, cur)
	if err != nil {
		return nil, err
	}
	if bal.Balance-bal.HeldBalance < params.Amount {
		return nil, ErrInsufficientCredits
	}
	sourceIDText := (*string)(nil)
	if params.SourceID != nil {
		v := params.SourceID.String()
		sourceIDText = &v
		existing, gerr := q.GetMoneyTransactionByCoords(ctx, gen.GetMoneyTransactionByCoordsParams{
			MerchantID: tenantID, CustomerID: payerID, Currency: cur,
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
		MerchantID:      tenantID,
		CustomerID:      payerID,
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
