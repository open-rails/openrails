package money

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/shared/moneyutil"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/grants"
	"github.com/open-rails/openrails/internal/modules/money/ledger"
	"github.com/open-rails/openrails/internal/modules/money/owed"
	"github.com/open-rails/openrails/internal/pagination"
	"github.com/open-rails/openrails/internal/retention"
	"github.com/open-rails/openrails/internal/shared/timeutil"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
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

// ErrInsufficientCredits is the public sentinel, so embedded host transactions
// and Client calls classify a capacity refusal identically.
var ErrInsufficientCredits = billing.ErrInsufficientCredits

type MoneyService struct {
	// EngineAdmissionHold is fixed during runtime wiring; accepted work still reconciles.
	EngineAdmissionHold     bool
	hyperSwitchDeployment   string
	invoiceRecoveryResolver NMIClientResolver
	db                      *db.DB
	clock                   clockwork.Clock
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
	cur := normalizeCurrency(currency)
	if err := moneyutil.ValidateCurrency(cur); err != nil {
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
	q := s.db.Gen(ctx)
	currencies, err := q.ListCustomerBalanceCurrencies(ctx, gen.ListCustomerBalanceCurrenciesParams{MerchantID: tenantID, CustomerID: payerID})
	if err != nil {
		return nil, err
	}
	out := []models.MoneyBalance{}
	for _, cur := range currencies {
		bal, err := s.deriveBalance(ctx, q, tenantID, payerID, normalizeCurrency(cur))
		if err != nil {
			return nil, err
		}
		out = append(out, *bal)
	}
	return out, nil
}

// AdmissionCapacity is the affordability snapshot consumed by the SQL
// service-admit gate.
type AdmissionCapacity struct {
	Balance     int64
	Held        int64
	BillingMode string
	CreditLimit int64
	// OutstandingOwed is the payer's unpaid arrears (positive), read O(1) from
	// their own arrears account in the same lookup (or#897). The credit line is
	// a ceiling on DEBT, so the line still available is CreditLimit - this.
	OutstandingOwed int64
}

// WithLockedAdmissionCapacity holds the merchant policy read lock, then the payer
// money lock. The callback receives the same transaction for admission writes.
func (s *MoneyService) WithLockedAdmissionCapacity(ctx context.Context, payer identity.CustomerID, currency string, fn func(context.Context, *db.DB, AdmissionCapacity) error) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("money service not initialized")
	}
	if payer.IsZero() {
		return fmt.Errorf("payer required")
	}
	if fn == nil {
		return fmt.Errorf("admission capacity callback required")
	}
	cur := normalizeCurrency(currency)
	if err := moneyutil.ValidateCurrency(cur); err != nil {
		return err
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	tenantID := tid.UUID()
	payerID := payer.UUID()
	s.db.EnsurePartitions(ctx, s.now())
	return s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		if _, err := q.ReadMerchantSettingsLock(ctx, tenantID); err != nil {
			return err
		}
		if err := ensureCustomer(ctx, q, tenantID, payerID); err != nil {
			return err
		}
		txSvc := &MoneyService{db: s.db.NewWithPgxTx(tx), clock: s.clock}
		if _, err := txSvc.lockBalance(ctx, q, payer, payerID.String(), cur); err != nil {
			return err
		}
		if _, err := ledger.New(q, tenantID).EnsureCustomerBalance(ctx, payerID, cur); err != nil {
			return err
		}
		now := s.now()
		row, err := q.GetAdmissionCapacity(ctx, gen.GetAdmissionCapacityParams{
			MerchantID: tenantID,
			CustomerID: payerID,
			Currency:   cur,
			AsOf:       now,
			HeldSince:  now.Add(-retention.AdmissionMaxHold),
		})
		if err != nil {
			return err
		}
		return fn(ctx, s.db.NewWithPgxTx(tx), admissionCapacityFromRow(row))
	})
}

// GetAdmissionCapacity reads the admit hot-path capacity in one point lookup:
// customer_balance counters, optional money_settings, and the payer's arrears
// account.
//
// or#878 ruling / or#897: it now reports OutstandingOwed. Arrears debt does NOT
// show up as a negative customer_balance — AccrueOwed debits the arrears
// account, leaving the balance untouched — so a credit line that ignored
// outstanding owed never bit at all: a payer could accrue past the line
// indefinitely. Still one query, still O(1).
func (s *MoneyService) GetAdmissionCapacity(ctx context.Context, payer identity.CustomerID, currency string) (AdmissionCapacity, error) {
	if s == nil || s.db == nil {
		return AdmissionCapacity{}, fmt.Errorf("money service not initialized")
	}
	cur := normalizeCurrency(currency)
	if err := moneyutil.ValidateCurrency(cur); err != nil {
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
	now := s.now()
	row, err := q.GetAdmissionCapacity(ctx, gen.GetAdmissionCapacityParams{
		MerchantID: tenantID,
		CustomerID: payerID,
		Currency:   cur,
		AsOf:       now,
		HeldSince:  now.Add(-retention.AdmissionMaxHold),
	})
	if err != nil {
		return AdmissionCapacity{}, err
	}
	return admissionCapacityFromRow(row), nil
}

func admissionCapacityFromRow(row gen.GetAdmissionCapacityRow) AdmissionCapacity {
	return AdmissionCapacity{
		Balance:         row.Balance,
		Held:            row.Held,
		BillingMode:     row.BillingMode,
		CreditLimit:     row.CreditLimitAmount,
		OutstandingOwed: row.OutstandingOwed,
	}
}

// ListBalanceTransactions lists a customer's ledger movements in one currency,
// newest first.
func (s *MoneyService) ListBalanceTransactions(ctx context.Context, payer identity.CustomerID, params billing.BalanceTransactionListParams) (billing.ListPage[billing.BalanceTransaction], error) {
	var page billing.ListPage[billing.BalanceTransaction]
	if s == nil || s.db == nil {
		return page, fmt.Errorf("money service not initialized")
	}
	if params.IDs != nil {
		tid, err := merchant.Require(ctx)
		if err != nil {
			return page, err
		}
		rows, err := s.db.Gen(ctx).ListLedgerTransfersByIDs(ctx, gen.ListLedgerTransfersByIDsParams{MerchantID: tid.UUID(), CustomerID: payer.UUID(), Ids: uuidutil.Of(params.IDs)})
		if err != nil {
			return page, err
		}
		for _, r := range rows {
			page.Items = append(page.Items, balanceTransactionFromTransfer(r))
		}
		return page, nil
	}
	cur := normalizeCurrency(params.Currency)
	if err := moneyutil.ValidateCurrency(cur); err != nil {
		return page, err
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return page, err
	}
	limit, err := pagination.Limit(params.PageRequest)
	if err != nil {
		return page, err
	}
	afterAt, afterID, err := pagination.After(params.Cursor)
	if err != nil {
		return page, err
	}
	rows, err := s.db.Gen(ctx).ListLedgerTransfersByCustomer(ctx, gen.ListLedgerTransfersByCustomerParams{
		MerchantID: tid.UUID(), CustomerID: payer.UUID(), Currency: cur,
		AfterAt: afterAt, AfterID: afterID, RowLimit: pagination.Fetch(limit),
	})
	if err != nil {
		return page, err
	}
	items := make([]billing.BalanceTransaction, 0, len(rows))
	for _, r := range rows {
		items = append(items, balanceTransactionFromTransfer(r))
	}
	return pagination.Cut(items, limit, func(t billing.BalanceTransaction) any {
		return pagination.TimeID{At: t.CreatedAt, ID: t.ID.UUID()}
	}), nil
}

// GetAccountSettingsForCustomer returns the stored money-account settings for an
// payer (billing mode and expiry default), scoped to
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

// GetDepositBySourceID answers "what did this deposit key do" (or#906): the
// credit grant committed at the caller's key, or (nil, nil) when the key never
// committed. KEY-QUALIFIED on the deposit's full structural coordinate —
// (merchant, payer, source_id) with operation=deposit fixed by the method
// itself — exactly the coordinate depositTx dedupes on. NOT a keyless
// coordinate read; or#894 deleted that shape as a trap (see above).
func (s *MoneyService) GetDepositBySourceID(ctx context.Context, payer identity.CustomerID, sourceID string) (*models.MoneyTransaction, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	sourceID = strings.TrimSpace(sourceID)
	if sourceID == "" {
		return nil, fmt.Errorf("source_id required")
	}
	var out *models.MoneyTransaction
	err := s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		tid, err := merchant.Require(ctx)
		if err != nil {
			return err
		}
		g, err := s.db.Gen(ctx).GetCreditGrantBySourceID(ctx, gen.GetCreditGrantBySourceIDParams{
			MerchantID: tid.UUID(), CustomerID: payer.UUID(), SourceID: sourceID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		out, err = creditGrantTxn(g)
		if err != nil {
			return err
		}
		out.Replayed = true
		return nil
	})
	return out, err
}

type DepositParams struct {
	// CustomerID is the merchant subject that owns the deposited balance (issue #221). When
	// nil, the payer is the invoker (Invoker)'s own account/personal merchant-subject UUID for the
	// self-hosted / single-merchant personal case; it is never a synthesized
	// stand-in, and a non-UUID Invoker with no explicit payer is rejected.
	CustomerID  *identity.CustomerID
	Invoker     string
	Currency    string
	Amount      int64
	Source      string
	SourceID    *string    // #491: natural-key string (uuidv7 pk + UNIQUE natural key), not a derived uuid
	ExpiresAt   *time.Time // nil is permanent; expiry is an immutable operation term
	Description *string
	// RepayOwed applies the new credit to outstanding owed first.
	RepayOwed bool
	// Internal fulfillment terms: duration is anchored to the first grant, including on replay.
	expiryHours int
}

func (s *MoneyService) Deposit(ctx context.Context, params DepositParams) (*models.MoneyTransaction, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	if params.Amount <= 0 {
		return nil, fmt.Errorf("amount must be positive")
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

// DepositItemError is a batch deposit refused at Index; the batch wrote
// nothing.
type DepositItemError struct {
	Index int
	Err   error
}

func (e *DepositItemError) Error() string { return fmt.Sprintf("item %d: %v", e.Index, e.Err) }
func (e *DepositItemError) Unwrap() error { return e.Err }

// DepositBatch records deposits all or none in one transaction, answering
// them in order. Balances lock in (customer, currency) order, so concurrent
// batches never deadlock.
func (s *MoneyService) DepositBatch(ctx context.Context, items []DepositParams) ([]*models.MoneyTransaction, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	order := make([]int, len(items))
	for i, item := range items {
		if item.Amount <= 0 {
			return nil, &DepositItemError{Index: i, Err: fmt.Errorf("amount must be positive")}
		}
		order[i] = i
	}
	lockKey := func(i int) string {
		customer := ""
		if items[i].CustomerID != nil {
			customer = items[i].CustomerID.String()
		}
		return customer + "\x00" + normalizeCurrency(items[i].Currency)
	}
	sort.SliceStable(order, func(a, b int) bool { return lockKey(order[a]) < lockKey(order[b]) })
	out := make([]*models.MoneyTransaction, len(items))
	err := s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		for _, i := range order {
			trx, err := s.depositTx(ctx, q, items[i])
			if err != nil {
				return &DepositItemError{Index: i, Err: err}
			}
			out[i] = trx
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ensureCustomer upserts the billing.customers row for a payable customer id
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

// depositTx records a money-in as a #514 credit grant (kind=credit), then
// materializes it (derive-2) into a #512 ledger deposit (DR processor_clearing /
// CR customer_balance). The grant IS the FIFO credit lot — there is no separate
// money_blocks row. Idempotent on the deposit's natural key (merchant, payer,
// source_id) via the credit-grant lookup. Runs under the customers-row lock so
// deposits serialize per customer.
func (s *MoneyService) depositTx(ctx context.Context, q *gen.Queries, params DepositParams) (*models.MoneyTransaction, error) {
	now := s.now()
	expiresAt, err := params.expiryAt(now)
	if err != nil {
		return nil, err
	}
	cur := normalizeCurrency(params.Currency)
	if _, err := CurrencyDecimals(cur); err != nil {
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
	// Idempotency: (merchant, payer, source_id) is the deposit key. A credit grant
	// already carrying this source_id means the deposit happened — return it.
	if params.SourceID != nil && strings.TrimSpace(*params.SourceID) != "" {
		existing, gerr := q.GetCreditGrantBySourceID(ctx, gen.GetCreditGrantBySourceIDParams{
			MerchantID: tenantID, CustomerID: payerID, SourceID: *params.SourceID,
		})
		if gerr == nil {
			if committed := derefInt(existing.Amount); committed != params.Amount {
				return nil, &IdempotencyConflict{
					Operation: string(OpDeposit), Source: params.Source, SourceID: *params.SourceID,
					Field: "amount", Committed: committed, Retried: params.Amount,
				}
			}
			if committed := derefStr(existing.Currency); committed != cur {
				return nil, &IdempotencyConflict{Operation: string(OpDeposit), Source: params.Source, SourceID: *params.SourceID,
					Field: "currency", Committed: committed, Retried: cur}
			}
			retryExpiry, err := params.expiryAt(existing.StartsAt)
			if err != nil {
				return nil, err
			}
			if !sameDepositExpiry(existing.EndsAt, retryExpiry) {
				return nil, &IdempotencyConflict{Operation: string(OpDeposit), Source: params.Source, SourceID: *params.SourceID,
					Field: "expires_at", Committed: existing.EndsAt, Retried: retryExpiry}
			}
			replayed, err := creditGrantTxn(existing)
			if err != nil {
				return nil, err
			}
			replayed.Replayed = true
			return replayed, nil
		}
		if !errors.Is(gerr, pgx.ErrNoRows) {
			return nil, gerr
		}
	}
	// A replay does not add balance, so only a new deposit needs the overflow guard.
	if bal.Balance > math.MaxInt64-params.Amount {
		return nil, fmt.Errorf("deposit would overflow balance")
	}

	gl := s.grantLedger(q, tenantID)
	g, err := gl.Grant(ctx, grants.GrantInput{
		Customer: payerID, Kind: grants.Credit,
		Source: grants.SourceType(depositSourceType(params.Source)), SourceID: derefStr(params.SourceID),
		Amount: &params.Amount, Currency: &cur, StartsAt: now, EndsAt: expiresAt,
		Reason: params.Description,
		Spec:   &grants.Spec{Deposit: &grants.DepositProvenance{Source: params.Source, Invoker: params.Invoker}},
	})
	if err != nil {
		return nil, err
	}
	if err := gl.MaterializeGrant(ctx, g); err != nil {
		return nil, err
	}
	if params.RepayOwed {
		if _, err := owed.Repay(ctx, q, tenantID, g, params.Amount, now); err != nil {
			return nil, err
		}
	}
	return creditGrantTxn(g)
}

func (p DepositParams) expiryAt(start time.Time) (*time.Time, error) {
	expiry := p.ExpiresAt
	if p.expiryHours > 0 {
		if expiry != nil || int64(p.expiryHours) > math.MaxInt64/int64(time.Hour) {
			return nil, fmt.Errorf("invalid relative deposit expiry")
		}
		t := start.Add(time.Duration(p.expiryHours) * time.Hour)
		expiry = &t
	}
	if expiry == nil {
		return nil, nil
	}
	t := expiry.UTC().Truncate(time.Microsecond)
	return &t, nil
}

func sameDepositExpiry(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

// creditGrantTxn synthesizes the public MoneyTransaction DTO for a deposit from
// its backing credit grant (the lot). The single-entry money_transactions row is
// gone (#512 hard cut); this DTO is derived, not stored.
func creditGrantTxn(g gen.BillingGrant) (*models.MoneyTransaction, error) {
	var spec grants.Spec
	if err := json.Unmarshal(g.SpecSnapshot, &spec); err != nil {
		return nil, fmt.Errorf("decode deposit provenance: %w", err)
	}
	if spec.Deposit == nil {
		return nil, fmt.Errorf("deposit grant %s has no recorded provenance", g.ID)
	}
	return &models.MoneyTransaction{
		ID:              g.ID,
		MerchantID:      g.MerchantID,
		CustomerID:      g.CustomerID,
		Currency:        derefStr(g.Currency),
		Invoker:         spec.Deposit.Invoker,
		Amount:          derefInt(g.Amount),
		TransactionType: "deposit",
		Status:          "posted",
		Source:          spec.Deposit.Source,
		SourceID:        g.SourceID,
		ExpiresAt:       g.EndsAt,
		Description:     g.Reason,
		CreatedAt:       g.CreatedAt,
		UpdatedAt:       g.CreatedAt,
	}, nil
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
	cur := normalizeCurrency(currency)
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
// (Balance = ledger customer-balance counters, HeldBalance = durable open
// operation authorizations)
// computed UNDER the lock. HeldBalance includes durable open operation
// authorizations and live request reservations.
// Every spend/hold/capture/deposit/expiry path calls this before
// reading/mutating the customer's blocks so no two mutations on the same
// customer interleave (no overdraft, atomic hold placement).
func (s *MoneyService) lockBalance(ctx context.Context, q *gen.Queries, payer identity.CustomerID, invokerID, currency string) (*models.MoneyBalance, error) {
	cur := normalizeCurrency(currency)
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
// = the customer's #512 ledger account counters and held = the sum of durable
// open operation authorizations linked to that account. The caller holds the
// customers-row lock (#491).
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
	var held int64
	if found {
		if held, err = s.heldAmount(ctx, q, tenantID, payerID, cur); err != nil {
			return nil, err
		}
	}
	// One held total covers live request reservations and provider authorizations.
	// Every ordinary spend path therefore respects both without double counting.
	return &models.MoneyBalance{
		MerchantID:  tenantID,
		CustomerID:  payerID,
		Currency:    cur,
		Balance:     bal,
		HeldBalance: held,
	}, nil
}

// heldAmount is the one held total against a customer's balance in a
// currency: live request reservations and provider authorizations.
func (s *MoneyService) heldAmount(ctx context.Context, q *gen.Queries, tenantID, payerID uuid.UUID, cur string) (int64, error) {
	now := s.now()
	return q.GetFinancialHeldAmount(ctx, gen.GetFinancialHeldAmountParams{
		MerchantID: tenantID, CustomerID: payerID, Currency: cur, AsOf: now,
		HeldSince: now.Add(-retention.AdmissionMaxHold),
	})
}

func (s *MoneyService) withdrawTx(ctx context.Context, q *gen.Queries, params WithdrawParams) (*models.MoneyTransaction, error) {
	now := s.now()
	cur := normalizeCurrency(params.Currency)
	if err := moneyutil.ValidateCurrency(cur); err != nil {
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
