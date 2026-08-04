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
	"github.com/open-rails/openrails/internal/modules/grants"
	"github.com/open-rails/openrails/internal/modules/money/ledger"
	"github.com/open-rails/openrails/internal/shared/timeutil"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
)

// grantLedger binds a #514 grant ledger to the query handle + merchant, on the
// service clock so derived event/expiry timestamps stay consistent with the
// rest of the money service.
func (s *MoneyService) grantLedger(q *gen.Queries, tenantID uuid.UUID) *grants.Ledger {
	gl := grants.New(q, tenantID)
	gl.SetClock(s.now)
	return gl
}

// moneyLedger binds a #512 double-entry money ledger to the query handle +
// merchant (for direct transfers like owed accrual/settlement).
func (s *MoneyService) moneyLedger(q *gen.Queries, tenantID uuid.UUID) *ledger.Ledger {
	return ledger.New(q, tenantID)
}

// depositSourceType maps a free-form deposit source label onto the grant
// ledger's source_type vocabulary (purchase | subscription | admin | grace).
func depositSourceType(source string) string {
	s := strings.ToLower(source)
	switch {
	case strings.Contains(s, "subscription"):
		return string(grants.Subscription)
	case strings.Contains(s, "purchase"):
		return string(grants.Purchase)
	case strings.Contains(s, "grace"):
		return string(grants.Grace)
	default:
		return string(grants.Admin)
	}
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func derefInt(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}

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
// real payer merchant-subject id. HARDCUT (#221): customer_id is supplied by
// the caller and is NOT synthesized — there is no deterministic stand-in
// derivation. For the
// self-hosted / single-merchant personal case the merchant subject IS the authenticated
// user's own account/personal merchant-subject UUID, so a non-UUID invoker with no explicit
// payer is a programming error rather than something to paper over.
var ErrCustomerRequired = errors.New("customer_id required")

// resolveCustomer resolves the merchant subject for a money operation (issue #221, the
// payer/billing payer). When the caller supplies an explicit merchant subject it is
// used verbatim. Otherwise, for the self-hosted / single-merchant personal case,
// the payer is the invoker's own account/personal merchant-subject id parsed from its UUID
// subject (identity.CustomerIDFromString). It is NEVER a synthesized stand-in.
//
// The invokerID is the free-form invoker (who caused usage); it is retained for
// attribution and is NOT the financial payer. resolveCustomer returns
// ErrCustomerRequired when no payer can be resolved.
func resolveCustomer(payer *identity.CustomerID, invokerID string) (identity.CustomerID, error) {
	if payer != nil && !payer.IsZero() {
		return *payer, nil
	}
	resolved := identity.CustomerIDFromString(invokerID)
	if resolved.IsZero() {
		return identity.CustomerID{}, ErrCustomerRequired
	}
	return resolved, nil
}

func (s *MoneyService) GetBalance(ctx context.Context, invokerID, currency string) (*models.MoneyBalance, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	// Reads are scoped by merchant + OWNER (issue #221). For the self-hosted /
	// single-merchant personal case the payer IS the user's own account/personal merchant-subject
	// UUID — the same row the merchant-subject-owned writers stamp — so single-user reads return
	// the right balance. customer_id is never synthesized.
	payer, err := resolveCustomer(nil, invokerID)
	if err != nil {
		return nil, err
	}
	return s.GetBalanceForCustomer(ctx, payer, currency)
}

// GetBalanceForCustomer reads a balance scoped explicitly by merchant subject (issue
// #221), for callers that own balances at a team customer rather than a personal
// customer group. The invoker string is not required for a payer-scoped read.
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

func (s *MoneyService) ListBalancesForCustomer(ctx context.Context, payer identity.CustomerID) ([]models.MoneyBalance, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	if payer.IsZero() {
		return nil, fmt.Errorf("payer required")
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	tenantID := tid.UUID()
	payerID := payer.UUID()
	rows, err := s.db.Qx(ctx).Query(ctx, `
SELECT currency
FROM (
    SELECT currency
    FROM openrails.ledger_accounts
    WHERE merchant_id = $1
      AND customer_id = $2
      AND account_type = 'customer_balance'
    UNION
    SELECT currency
    FROM openrails.money_settings
    WHERE merchant_id = $1
      AND customer_id = $2
    UNION
    SELECT currency
    FROM openrails.invoice_items
    WHERE merchant_id = $1
      AND customer_id = $2
      AND invoice_id IS NULL
      AND status = 'pending'
    UNION
    SELECT currency
    FROM openrails.invoices
    WHERE merchant_id = $1
      AND customer_id = $2
      AND status IN ('open', 'past_due')
      AND amount_due > 0
) currencies
ORDER BY currency`, tenantID, payerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	currencies := []string{}
	for rows.Next() {
		var cur string
		if err := rows.Scan(&cur); err != nil {
			return nil, err
		}
		currencies = append(currencies, cur)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()

	q := s.db.Gen(ctx)
	out := []models.MoneyBalance{}
	for _, cur := range currencies {
		bal, err := s.deriveBalance(ctx, q, tenantID, payerID, normalizeUnit(cur))
		if err != nil {
			return nil, err
		}
		out = append(out, *bal)
	}
	return out, nil
}

// AdmissionCapacity is the O(1) affordability snapshot consumed by the Redis
// service-admit gate.
type AdmissionCapacity struct {
	Balance     int64
	Held        int64
	BillingMode string
	CreditLimit int64
}

// GetAdmissionCapacity reads the admit hot-path capacity in one point lookup:
// customer_balance counters plus optional money_settings. It deliberately does
// not subtract outstanding owed; under the Phase-H model used credit is already
// represented by a negative customer_balance.
func (s *MoneyService) GetAdmissionCapacity(ctx context.Context, payer identity.CustomerID, currency string) (AdmissionCapacity, error) {
	if s == nil || s.db == nil {
		return AdmissionCapacity{}, fmt.Errorf("money service not initialized")
	}
	cur := normalizeUnit(currency)
	if err := s.validateUnit(ctx, cur); err != nil {
		return AdmissionCapacity{}, err
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return AdmissionCapacity{}, err
	}
	tenantID := tid.UUID()
	payerID := payer.UUID()
	q := s.db.Gen(ctx)
	if err := ensureCustomer(ctx, q, tenantID, payerID); err != nil {
		return AdmissionCapacity{}, err
	}
	if _, err := ledger.New(q, tenantID).EnsureCustomerBalance(ctx, payerID, cur); err != nil {
		return AdmissionCapacity{}, err
	}
	row, err := q.GetAdmissionCapacity(ctx, gen.GetAdmissionCapacityParams{
		MerchantID: tenantID,
		CustomerID: payerID,
		Currency:   cur,
	})
	if err != nil {
		return AdmissionCapacity{}, err
	}
	return AdmissionCapacity{
		Balance:     row.Balance,
		Held:        row.Held,
		BillingMode: row.BillingMode,
		CreditLimit: row.CreditLimitAmount,
	}, nil
}

func (s *MoneyService) GetTransactions(ctx context.Context, invokerID, currency string, limit, offset int) ([]models.MoneyTransaction, int, error) {
	if s == nil || s.db == nil {
		return nil, 0, fmt.Errorf("money service not initialized")
	}
	payer, err := resolveCustomer(nil, invokerID)
	if err != nil {
		return nil, 0, err
	}
	return s.GetTransactionsByCustomer(ctx, payer, currency, limit, offset)
}

// GetTransactionsByCustomer lists money transactions for an EXPLICIT merchant subject
// (the payer), newest first, paginated. Unlike GetTransactions it does not derive
// the payer from a invoker id — it filters customer_id directly, which is what the
// customer-level billing-account usage view (issue #242) needs. RLS-scoped to the
// request merchant via Qx(ctx).
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
	total, err := q.CountLedgerTransfersByCustomer(ctx, gen.CountLedgerTransfersByCustomerParams{
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
	rows, err := q.ListLedgerTransfersByCustomer(ctx, gen.ListLedgerTransfersByCustomerParams{
		MerchantID: tenantID, CustomerID: payerID, Currency: cur,
		Lim: int32(limit), Off: int32(offset),
	})
	if err != nil {
		return nil, 0, err
	}
	items := make([]models.MoneyTransaction, 0, len(rows))
	for _, r := range rows {
		items = append(items, *moneyTransactionFromTransfer(r))
	}
	return items, int(total), nil
}

// GetAccountSettingsForCustomer returns the stored money-account settings for an
// payer (billing mode, spend caps, auto-top-up, expiry default), RLS-scoped to
// the request merchant (issue #242). Never nil — missing rows return the defaults.
func (s *MoneyService) GetAccountSettingsForCustomer(ctx context.Context, payer identity.CustomerID, currency string) (*models.MoneyAccount, error) {
	var out *models.MoneyAccount
	err := s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		st, e := s.GetAccountSettings(ctx, payer, currency)
		if e != nil {
			return e
		}
		out = st
		return nil
	})
	return out, err
}

// or#894 task 4: GetTransactionBySource is DELETED, not fixed. It read the
// ledger by (transfer_type, source, source_id) — the same coordinate the
// capture/waste collision lived at — and had no callers. A dead read that
// resolves an ambiguous coordinate is a trap waiting for its first caller, so
// it goes. Reads by coordinate now go through GetLedgerTransferByCoords with an
// operation.

type DepositParams struct {
	// CustomerID is the merchant subject that owns the deposited balance (issue #221). When
	// nil, the payer is the invoker (Invoker)'s own account/personal merchant-subject UUID for the
	// self-hosted / single-merchant personal case; it is never a synthesized
	// stand-in, and a non-UUID Invoker with no explicit payer is rejected.
	CustomerID *identity.CustomerID
	Invoker    string
	Currency   string
	Amount     int64
	Source     string
	SourceID   *string // #491: natural-key string (uuidv7 pk + UNIQUE natural key), not a derived uuid
	ExpiresAt  *time.Time
	// ApplyAccountExpiryDefault, when true and ExpiresAt is nil, sets the deposit's
	// expiry to now + the payer's money_accounts.default_credit_expiry_hours
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
		if payer, oerr := resolveCustomer(params.CustomerID, params.Invoker); oerr == nil {
			if settings, serr := s.GetAccountSettings(ctx, payer, params.Currency); serr == nil &&
				settings.DefaultCreditExpiryHours != nil && *settings.DefaultCreditExpiryHours > 0 {
				exp := s.now().Add(time.Duration(*settings.DefaultCreditExpiryHours) * time.Hour)
				params.ExpiresAt = &exp
			}
		}
	}

	var trx *models.MoneyTransaction
	err := s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
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
	return db.EnsureCustomerRowQ(ctx, q, tenantID, tsid)
}

func stringPtr(s string) *string { return &s }

// depositTx records a money-in as a #514 credit grant (kind=credit), then
// materializes it (derive-2) into a #512 ledger deposit (DR processor_clearing /
// CR customer_balance). The grant IS the FIFO credit lot — there is no separate
// money_blocks row. Idempotent on the deposit's natural key (merchant, payer,
// source_id) via the credit-grant lookup. Runs under the customers-row lock so
// deposits serialize per customer.
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
	payer, err := resolveCustomer(params.CustomerID, params.Invoker)
	if err != nil {
		return nil, err
	}
	payerID := payer.UUID()
	bal, err := s.lockBalance(ctx, q, payer, params.Invoker, cur)
	if err != nil {
		return nil, err
	}
	// Guard against int64 overflow: a deposit that would wrap the derived balance
	// past MaxInt64 is rejected rather than corrupting the ledger.
	if bal.Balance > math.MaxInt64-params.Amount {
		return nil, fmt.Errorf("deposit would overflow balance")
	}

	// Idempotency: (merchant, payer, source_id) is the deposit key. A credit grant
	// already carrying this source_id means the deposit happened — return it.
	if params.SourceID != nil && strings.TrimSpace(*params.SourceID) != "" {
		existing, gerr := q.GetCreditGrantBySourceID(ctx, gen.GetCreditGrantBySourceIDParams{
			MerchantID: tenantID, CustomerID: payerID, SourceID: *params.SourceID,
		})
		if gerr == nil {
			// or#892: the deposit's structural key is the credit grant's
			// (merchant, customer, source_id). A hit means this deposit already
			// credited; say so instead of letting the caller assume it moved money.
			replayed := s.creditGrantTxn(existing, params)
			replayed.Replayed = true
			return replayed, nil
		}
		if !errors.Is(gerr, pgx.ErrNoRows) {
			return nil, gerr
		}
	}

	gl := s.grantLedger(q, tenantID)
	g, err := gl.Grant(ctx, grants.GrantInput{
		Customer: payerID, Kind: grants.Credit,
		Source: grants.SourceType(depositSourceType(params.Source)), SourceID: derefStr(params.SourceID),
		Amount: &params.Amount, Currency: &cur, StartsAt: now, EndsAt: params.ExpiresAt,
	})
	if err != nil {
		return nil, err
	}
	if err := gl.MaterializeGrant(ctx, g); err != nil {
		return nil, err
	}

	// AUTO-graduation (#476): cumulative credits granted in this currency just
	// changed, so recompute + persist the payer's same-currency trust level from
	// the stored tier_schedule, in-band with the deposit.
	cumPaid, perr := q.SumCreditGrants(ctx, gen.SumCreditGrantsParams{
		MerchantID: tenantID, CustomerID: payerID, Currency: cur,
	})
	if perr != nil {
		return nil, perr
	}
	if err := s.autoGraduateTrustLevelTx(ctx, q, tenantID, payer, cur, cumPaid, now); err != nil {
		return nil, err
	}

	return s.creditGrantTxn(g, params), nil
}

// creditGrantTxn synthesizes the public MoneyTransaction DTO for a deposit from
// its backing credit grant (the lot). The single-entry money_transactions row is
// gone (#512 hard cut); this DTO is derived, not stored.
func (s *MoneyService) creditGrantTxn(g gen.OpenrailsGrant, params DepositParams) *models.MoneyTransaction {
	sid := g.SourceID
	return &models.MoneyTransaction{
		ID:              g.ID,
		MerchantID:      g.MerchantID,
		CustomerID:      g.CustomerID,
		Currency:        normalizeUnit(derefStrOr(g.Currency, params.Currency)),
		Invoker:         params.Invoker,
		Amount:          derefInt(g.Amount),
		TransactionType: "deposit",
		Status:          "posted",
		Source:          params.Source,
		SourceID:        &sid,
		ExpiresAt:       g.EndsAt,
		Description:     params.Description,
		CreatedAt:       g.CreatedAt,
		UpdatedAt:       g.CreatedAt,
	}
}

func derefStrOr(s *string, fallback string) string {
	if s == nil || *s == "" {
		return fallback
	}
	return *s
}

type WithdrawParams struct {
	// CustomerID is the merchant subject to withdraw from (issue #221). When nil, the payer
	// is the invoker (Invoker)'s own account/personal merchant-subject UUID for the self-hosted /
	// single-merchant personal case; it is never a synthesized stand-in.
	CustomerID *identity.CustomerID
	Invoker    string
	Currency   string
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
	err := s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var terr error
		trx, terr = s.withdrawTx(ctx, gen.New(tx), params)
		return terr
	})
	if err != nil {
		return nil, err
	}
	return trx, nil
}

// withdrawBalanceAndBlocks debits `amount` from the customer's prepaid balance by
// spending #514 credit lots FIFO (soonest-expiring first) via the grant ledger,
// which emits one #512 credit_spend transfer per lot drawn — tagged with the
// caller's operation coordinate (source, sourceID) for idempotency/history and
// grant_id for lot attribution. The available/credit-line gate is the CALLER's
// job (#491); this fails only if the lots physically cannot cover `amount` (a
// gated caller never hits that). Returns the derived balance AFTER the debit.
func (s *MoneyService) withdrawBalanceAndBlocks(ctx context.Context, q *gen.Queries, payer identity.CustomerID, invokerID, currency string, key IdempotencyKey, resource string, amount int64) (newBalance int64, applied bool, err error) {
	cur := normalizeUnit(currency)
	tid, err := merchant.Require(ctx)
	if err != nil {
		return 0, false, err
	}
	tenantID := tid.UUID()
	payerID := payer.UUID()
	// Lock + serialize, then derive the balance under the lock.
	bal, err := s.lockBalance(ctx, q, payer, invokerID, cur)
	if err != nil {
		return 0, false, err
	}
	if bal.Balance < amount {
		return 0, false, ErrInsufficientCredits
	}
	gl := s.grantLedger(q, tenantID)
	applied, err = gl.CreditSpend(ctx, payerID, cur, amount, invokerID, resource, key.Coord())
	if err != nil {
		if errors.Is(err, grants.ErrInsufficientCredits) {
			return 0, false, ErrInsufficientCredits
		}
		return 0, false, err
	}
	if !applied {
		// The coordinate was already committed: the balance never moved in this
		// call, so the pre-debit snapshot IS the current balance.
		return bal.Balance, false, nil
	}
	return bal.Balance - amount, true, nil
}

// lockBalance is the per-customer spend mutex (#491): it FOR UPDATE-locks the
// customers row (the serialization point — money_balances is gone), ensuring the
// row exists first, then returns the DERIVED balance snapshot
// (Balance = SUM spendable blocks, HeldBalance = active holds + open windows)
// computed UNDER the lock. Every spend/hold/capture/deposit/expiry path calls
// this before reading/mutating the customer's blocks so no two mutations on the
// same customer interleave (no overdraft, atomic hold placement).
func (s *MoneyService) lockBalance(ctx context.Context, q *gen.Queries, payer identity.CustomerID, invokerID, currency string) (*models.MoneyBalance, error) {
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
// = the customer's #512 ledger balance (credits − debits over posted +
// post_pending transfers), held = open-window unsettled reservations (#335). The
// caller holds the customers-row lock (#491).
func (s *MoneyService) deriveBalance(ctx context.Context, q *gen.Queries, tenantID, payerID uuid.UUID, cur string) (*models.MoneyBalance, error) {
	l := ledger.New(q, tenantID)
	// A balance READ must never create the account (#534): no account = zero
	// balance. EnsureCustomerBalance (create) is reserved for the write flows.
	acc, found, err := l.CustomerBalanceAccountID(ctx, payerID, cur)
	if err != nil {
		return nil, err
	}
	var bal int64
	if found {
		if bal, err = l.Balance(ctx, acc); err != nil {
			return nil, err
		}
	}
	// HeldBalance is always 0 in the durable ledger: admission holds live in Redis
	// (#513) and the prepaid money_windows reservation was removed (dormant,
	// superseded by the spendgate). Two-phase ledger holds are not on the live path.
	return &models.MoneyBalance{
		MerchantID:  tenantID,
		CustomerID:  payerID,
		Currency:    cur,
		Balance:     bal,
		HeldBalance: 0,
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
	payer, err := resolveCustomer(params.CustomerID, params.Invoker)
	if err != nil {
		return nil, err
	}
	payerID := payer.UUID()
	// Lock + derive once: gate on held-aware available (a withdraw cannot eat into
	// reserved/held funds), then idempotency, then debit the FIFO blocks (#491).
	bal, err := s.lockBalance(ctx, q, payer, params.Invoker, cur)
	if err != nil {
		return nil, err
	}
	if bal.Balance-bal.HeldBalance < params.Amount {
		return nil, ErrInsufficientCredits
	}
	// or#891 item 1 (same shape as SpendCredits): the dedupe read used to sit
	// behind `if params.SourceID != nil`, so a keyless withdraw posted
	// unconditionally and every retry debited again.
	if params.SourceID == nil {
		return nil, fmt.Errorf("withdraw: source_id required for idempotency")
	}
	sid := params.SourceID.String()
	sourceIDText := &sid
	key, kerr := NewIdempotencyKey(OpWithdraw, params.Source, sid)
	if kerr != nil {
		return nil, kerr
	}
	// or#891 item 3: compare the TOTAL already withdrawn at these coordinates (a
	// withdraw fans out one credit_spend transfer per FIFO lot) and refuse a
	// replay whose amount differs, rather than answering it with the first row.
	committed, cerr := key.requireSameAmount(ctx, q, tenantID, payerID, cur, params.Amount)
	if cerr != nil {
		return nil, cerr
	}
	if committed {
		existing, gerr := q.GetLedgerTransferByCoords(ctx, gen.GetLedgerTransferByCoordsParams{
			MerchantID: tenantID, CustomerID: payerID, Currency: cur,
			TransferType: "credit_spend", Operation: string(key.Operation()),
			Source: key.Source(), SourceID: key.SourceID(),
		})
		if gerr == nil {
			replayed := moneyTransactionFromTransfer(existing)
			replayed.Replayed = true
			return replayed, nil
		}
		if !errors.Is(gerr, pgx.ErrNoRows) {
			return nil, gerr
		}
	}
	newBal, applied, err := s.withdrawBalanceAndBlocks(ctx, q, payer, params.Invoker, cur, key, "", params.Amount)
	if err != nil {
		return nil, err
	}

	trx := &models.MoneyTransaction{
		ID:              uuidutil.NewV7(),
		MerchantID:      tenantID,
		CustomerID:      payerID,
		Currency:        cur,
		Invoker:         params.Invoker,
		Amount:          -params.Amount,
		BalanceAfter:    &newBal,
		TransactionType: "withdrawal",
		Status:          "posted",
		Source:          params.Source,
		SourceID:        sourceIDText,
		CreatedAt:       now,
		UpdatedAt:       now,
		// Authoritative: the unique index, not the pre-check above, decides.
		Replayed: !applied,
	}
	return trx, nil
}
