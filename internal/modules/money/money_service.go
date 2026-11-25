package money

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/destructive"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/internal/shared/moneyutil"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/limits"
	"github.com/open-rails/openrails/internal/modules/money/credits"
	"github.com/open-rails/openrails/internal/modules/money/ledger"
	"github.com/open-rails/openrails/internal/modules/money/owed"
	"github.com/open-rails/openrails/internal/pagination"
	"github.com/open-rails/openrails/internal/shared/timeutil"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

// creditLots binds the merchant's credit lots to the query handle on the
// service clock.
func (s *MoneyService) creditLots(q *gen.Queries, tenantID uuid.UUID) *credits.Lots {
	lots := credits.New(q, tenantID, s.now)
	lots.DrawLiveAt(s.lotsLiveAt())
	return lots
}

// moneyLedger binds the double-entry money ledger to the query handle +
// merchant (for direct transfers like owed accrual/settlement).
func (s *MoneyService) moneyLedger(q *gen.Queries, tenantID uuid.UUID) *ledger.Ledger {
	return ledger.New(q, tenantID).SetClock(s.now)
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
	destructiveAdmissions *destructive.Admissions
	hyperSwitchDeployment string
	usageRecoveryResolver railresolve.NMIClientResolver
	db                    *db.DB
	clock                 clockwork.Clock
	// posting is set on the service a running charge's posting runs on.
	posting *runningChargePosting
	// usagePosted marks running-charge postings made after the customer's
	// usage before their instant posted.
	usagePosted bool
}

func NewMoneyService(database *db.DB, clocks ...clockwork.Clock) *MoneyService {
	return &MoneyService{db: database, clock: timeutil.FirstClock(clocks...)}
}

// WithDatabase derives a transaction-scoped service without losing host configuration.
func (s *MoneyService) WithDatabase(database *db.DB) *MoneyService {
	copy := *s
	copy.db = database
	return &copy
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
// payer: customer_id comes from the caller and is never synthesized.
var ErrCustomerRequired = errors.New("customer_id required")

// resolveCustomer returns the explicit payer, else the invoker parsed as a
// customer UUID (the self-hosted personal case), else ErrCustomerRequired. The
// invoker is kept for attribution and is not the financial payer.
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
	// Reads are scoped by merchant + payer; without an explicit payer the
	// invoker's own UUID is the payer.
	payer, err := resolveCustomer(nil, invokerID)
	if err != nil {
		return nil, err
	}
	return s.GetBalanceForCustomer(ctx, payer, currency)
}

// GetBalanceForCustomer reads a balance scoped explicitly by payer, e.g. a team
// customer rather than the invoker's own. No invoker is needed.
func (s *MoneyService) GetBalanceForCustomer(ctx context.Context, payer identity.CustomerID, currency string) (*models.MoneyBalance, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	cur := normalizeCurrency(currency)
	if err := moneyutil.ValidateBalanceCurrency(cur); err != nil {
		return nil, err
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	tenantID := tid.UUID()
	payerID := payer.UUID()
	// Derived read without a lock: a stale read never overdrafts, since writers
	// re-derive under the customers-row lock.
	return s.deriveBalance(ctx, s.db.Gen(ctx), tenantID, payerID, cur)
}

// AdmissionCapacity is the affordability snapshot consumed by the SQL
// service-admit gate.
type AdmissionCapacity struct {
	HardCreditLimit bool
	Balance         int64
	Held            int64
	BillingMode     string
	CreditLimit     int64
	// OutstandingOwed is the payer's unpaid arrears (positive), read O(1) from
	// their own arrears account in the same lookup. The credit line caps debt,
	// so the line still available is CreditLimit - this.
	OutstandingOwed int64
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
		deductions, err := usageDeductions(ctx, s.db.Gen(ctx), tid.UUID(), payer.UUID(), rows)
		if err != nil {
			return page, err
		}
		for _, r := range rows {
			item := balanceTransactionFromTransfer(r)
			item.Usage = deductions[r.ID]
			page.Items = append(page.Items, item)
		}
		return page, nil
	}
	cur := normalizeCurrency(string(params.Currency))
	if err := moneyutil.ValidateBalanceCurrency(cur); err != nil {
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
	deductions, err := usageDeductions(ctx, s.db.Gen(ctx), tid.UUID(), payer.UUID(), rows)
	if err != nil {
		return page, err
	}
	items := make([]billing.BalanceTransaction, 0, len(rows))
	for _, r := range rows {
		item := balanceTransactionFromTransfer(r)
		item.Usage = deductions[r.ID]
		items = append(items, item)
	}
	return pagination.Cut(items, limit, func(t billing.BalanceTransaction) any {
		return pagination.TimeID{At: t.CreatedAt, ID: t.ID.UUID()}
	}), nil
}

// GetAccountSettingsForCustomer returns the payer's stored money-account
// settings (billing mode and expiry default) in the request merchant. Never
// nil: missing rows return the defaults.
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

type DepositParams struct {
	// CustomerID owns the deposited balance; nil means the invoker's own UUID.
	// A non-UUID Invoker without CustomerID is refused.
	CustomerID  *identity.CustomerID
	Invoker     string
	Currency    string
	Amount      int64
	Source      string
	SourceID    *string    // natural-key string (uuidv7 pk + UNIQUE natural key), not a derived uuid
	ExpiresAt   *time.Time // nil is permanent; expiry is an immutable operation term
	Description *string
	// Funding is where the units came from; zero is a grant, and a replay
	// that leaves it zero matches whatever the lot recorded. A purchase here
	// is change from a charge, paid at face.
	Funding credits.FundingSource
	// RepayOwed applies the new credit to outstanding owed first.
	RepayOwed bool
	// Internal fulfillment terms: duration is anchored to the first grant, including on replay.
	expiryHours int
}

func (p DepositParams) funding() credits.FundingSource {
	if p.Funding == "" {
		return credits.Grant
	}
	return p.Funding
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
		if err := countDepositUnits(ctx, gen.New(tx), params); err != nil {
			return err
		}
		var terr error
		trx, terr = s.depositTx(ctx, s.db.NewWithPgxTx(tx), params)
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
		if err := countDepositUnits(ctx, gen.New(tx), items...); err != nil {
			return err
		}
		d := s.db.NewWithPgxTx(tx)
		for _, i := range order {
			trx, err := s.depositTx(ctx, d, items[i])
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

// countDepositUnits counts the merchant's units the deposits are in before
// any customer is locked: a first use takes the units lock first, so two
// batches never wait on each other's locks in opposite orders.
func countDepositUnits(ctx context.Context, q *gen.Queries, items ...DepositParams) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	codes := make([]string, 0, len(items))
	for _, item := range items {
		codes = append(codes, normalizeCurrency(item.Currency))
	}
	return ledger.New(q, mid.UUID()).CountUnits(ctx, codes...)
}

// ensureCustomer upserts the billing.customers row for a payable customer id
// so the money-write FKs hold on a customer's first money operation
// (deposit/hold/usage). ON CONFLICT DO NOTHING.
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

// depositTx records a money-in as a credit lot and posts its deposit (DR
// units_sold / CR customer_units). Idempotent on (merchant, payer,
// source_id) via the lot lookup. Runs under the customers-row lock so deposits
// serialize per customer.
func (s *MoneyService) depositTx(ctx context.Context, d *db.DB, params DepositParams) (*models.MoneyTransaction, error) {
	q := d.Gen(ctx)
	now := s.now()
	expiresAt, err := params.expiryAt(now)
	if err != nil {
		return nil, err
	}
	cur := normalizeCurrency(params.Currency)
	if err := moneyutil.ValidateBalanceCurrency(cur); err != nil {
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
			if existing.Units != params.Amount {
				return nil, &IdempotencyConflict{
					Operation: string(OpDeposit), Source: params.Source, SourceID: *params.SourceID,
					Field: "amount", Committed: existing.Units, Retried: params.Amount,
				}
			}
			if existing.Unit != cur {
				return nil, &IdempotencyConflict{Operation: string(OpDeposit), Source: params.Source, SourceID: *params.SourceID,
					Field: "currency", Committed: existing.Unit, Retried: cur}
			}
			retryExpiry, err := params.expiryAt(existing.StartsAt)
			if err != nil {
				return nil, err
			}
			if !sameDepositExpiry(existing.ExpiresAt, retryExpiry) {
				return nil, &IdempotencyConflict{Operation: string(OpDeposit), Source: params.Source, SourceID: *params.SourceID,
					Field: "expires_at", Committed: existing.ExpiresAt, Retried: retryExpiry}
			}
			if params.Funding != "" && existing.FundingSource != string(params.Funding) {
				return nil, &IdempotencyConflict{Operation: string(OpDeposit), Source: params.Source, SourceID: *params.SourceID,
					Field: "funding_source", Committed: existing.FundingSource, Retried: string(params.Funding)}
			}
			replayed := creditGrantTxn(existing)
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

	funding := params.funding()
	if !funding.Valid() || funding == credits.Recurring {
		return nil, fmt.Errorf("a deposit is a grant, a promotion, an import or change from a charge")
	}
	invoker := params.Invoker
	if invoker == "" {
		invoker = payerID.String() // the customer funded its own balance
	}
	issue := credits.Issue{
		Customer: payerID, Unit: cur, Units: params.Amount, Funding: funding,
		Source: params.Source, SourceID: derefStr(params.SourceID), Invoker: invoker,
		Description: params.Description, StartsAt: now, ExpiresAt: expiresAt,
	}
	if funding.Paid() {
		issue.PaidAmount, issue.PaidCurrency = &params.Amount, &cur
	}
	lot, err := s.creditLots(q, tenantID).Issue(ctx, issue)
	if err != nil {
		return nil, err
	}
	if params.RepayOwed {
		if _, err := owed.Repay(ctx, d, tenantID, lot, params.Amount, now); err != nil {
			return nil, err
		}
	}
	return creditGrantTxn(lot), nil
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

// creditGrantTxn is the public MoneyTransaction of a deposit: its lot.
func creditGrantTxn(lot gen.BillingCreditGrant) *models.MoneyTransaction {
	return &models.MoneyTransaction{
		ID:              lot.ID,
		MerchantID:      lot.MerchantID,
		CustomerID:      lot.CustomerID,
		Currency:        lot.Unit,
		Invoker:         lot.Invoker,
		Amount:          lot.Units,
		TransactionType: "deposit",
		Status:          "posted",
		Source:          lot.Source,
		SourceID:        lot.SourceID,
		ExpiresAt:       lot.ExpiresAt,
		Description:     lot.Description,
		CreatedAt:       lot.CreatedAt,
		UpdatedAt:       lot.CreatedAt,
	}
}

// withdrawBalanceAndBlocks debits amount from the customer's prepaid balance by
// spending credit lots FIFO (soonest-expiring first), one credit_spend transfer
// per lot at the caller's coordinate. The available/credit-line gate is the
// caller's job; this fails only if the lots cannot cover amount. Returns the
// derived balance after the debit.
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
	applied, err = s.creditLots(q, tenantID).Spend(ctx, payerID, cur, amount, invokerID, resource, key.Coord())
	if err != nil {
		if errors.Is(err, credits.ErrInsufficientCredits) {
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

// lockBalance is the per-customer spend mutex: it ensures the customers row
// exists, FOR UPDATE-locks it, and returns the balance derived under the lock
// (Balance = ledger counters, HeldBalance = open authorizations + live
// reservations + running-charge accrual). Every spend/hold/capture/deposit/expiry path calls it first,
// so mutations on one customer never interleave (no overdraft).
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

// deriveBalance computes the balance snapshot: balance = the customer's ledger
// account counters, held = heldAmount. Writers call it under the customers-row
// lock.
func (s *MoneyService) deriveBalance(ctx context.Context, q *gen.Queries, tenantID, payerID uuid.UUID, cur string) (*models.MoneyBalance, error) {
	l := ledger.New(q, tenantID)
	// A balance read never creates the account: no account = zero balance.
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
	now := instant(s.now())
	var held, accrued int64
	if found {
		if held, accrued, err = s.heldAmount(ctx, q, tenantID, payerID, cur, now); err != nil {
			return nil, err
		}
	}
	// One held total covers live request reservations, provider authorizations
	// and what running charges have accrued. Every ordinary spend path
	// therefore respects all of them without double counting.
	return &models.MoneyBalance{
		MerchantID:  tenantID,
		CustomerID:  payerID,
		Currency:    cur,
		Balance:     bal,
		HeldBalance: held,
		Accrued:     accrued,
		AsOf:        now,
	}, nil
}

// heldAmount is the one held total against a customer's balance in a
// currency as of now (live request reservations, provider authorizations,
// running-charge accrual and unposted usage), and the accrual apart. A
// posting after the usage before its instant posted holds no usage: as of
// that instant there is none, so usage after it never keeps a lot lapsing
// then from what came before.
func (s *MoneyService) heldAmount(ctx context.Context, q *gen.Queries, tenantID, payerID uuid.UUID, cur string, now time.Time) (held, accrued int64, err error) {
	row, err := q.GetFinancialHeldAmount(ctx, gen.GetFinancialHeldAmountParams{
		MerchantID: tenantID, CustomerID: payerID, Currency: cur, AsOf: now,
		LeaseLiveSince: limits.LeaseLiveSince(now), AccruedAsOf: s.accruedAsOf(),
	})
	if err != nil {
		return 0, 0, err
	}
	if s.posting != nil && s.posting.usagePosted {
		return row.Held, row.Accrued, nil
	}
	unposted, err := s.unpostedUsage(ctx, q, tenantID, payerID, cur)
	return addSat(row.Held, unposted), row.Accrued, err
}

// SetDestructiveAdmissions wires the shared staff-command admission boundary.
func (s *MoneyService) SetDestructiveAdmissions(a *destructive.Admissions) {
	s.destructiveAdmissions = a
}
