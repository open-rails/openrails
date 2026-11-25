// Package leases syncs gates' leases: in one transaction per sync it settles
// their usage in order, as far as its time allows, and keeps their in-flight
// reports; once it committed it answers views of the customers they serve.
//
// A sync takes no customer's lock. Its transaction locks its own lease row
// and shares the merchant's catalog fence; its intake is append-only (events
// into the event store, then the lease's numbering and receipts, an unposted
// mark held by key share), and its in-flight report writes only its own
// rows. Only a window that must open or a failed item's charge takes
// that customer's lock (money.PrepareUsageTx). Views are read after the
// commit, in batches, in one snapshot (views.go).
package leases

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/api"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/eventstore"
	"github.com/open-rails/openrails/internal/failpoint"
	"github.com/open-rails/openrails/internal/logx"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/admission"
	"github.com/open-rails/openrails/internal/modules/limits"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/retention"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// Bookkeeping constants. None is a setting: only speed depends on them.
const (
	// maxReceipts and maxReceiptBytes bound the receipts a lease keeps: a
	// gate resends only what its lost replies left unacknowledged.
	maxReceipts     = 64
	maxReceiptBytes = 128 << 10
	// leaseIDSkew is how far a lease id's time may run ahead of the server
	// clock: an id with no lease older than the replay horizon less it names
	// a lease that may already be forgotten.
	leaseIDSkew = retention.Day
	// commitMargin is how far inside limits.SyncTimeout a sync must reach its
	// commit; one that ran longer is refused and retried.
	commitMargin = time.Second
	// workBudget is how far into its transaction a sync still starts a view:
	// one it has no time for is answered null and asked again.
	workBudget = 2500 * time.Millisecond
	// lockBudget is how long a sync's statement may wait for a lock: a
	// posting holding an unposted mark, or a customer whose window opens.
	lockBudget = workBudget - 500*time.Millisecond
	// usageChunkItems is the most items one chunk of usage takes in.
	usageChunkItems = 250
)

// A sync settles its new usage in chunks of at most UsageChunkCustomers
// customers, each one intake, and starts no chunk after the first once
// UsageBudget of it has passed: the rest is left for the next sync. A chunk
// takes well under commitMargin.
const (
	UsageBudget         = 2 * time.Second
	UsageChunkCustomers = 100
)

// FailpointKind names a lease sync at failpoint.AfterChunk.
const FailpointKind = "lease-sync"

// Syncer syncs leases of the context's merchant.
type Syncer struct {
	DB       *db.DB
	Money    *money.MoneyService
	Admitter *admission.Admitter
	// FailedWindows are the windows a failed report counts toward; read
	// before the sync's transaction.
	FailedWindows func(ctx context.Context, customer identity.CustomerID, invokerType billing.InvokerType, currency string) ([]money.FailedUsageWindow, error)
	// Allows is the credential's customer scope.
	Allows func(billing.CustomerID) bool
	Now    func() time.Time
}

// receipt is what one settlement of a lease answered. A refused item keeps
// only its number and code.
type receipt struct {
	FromN    int64          `json:"from_n"`
	ThroughN int64          `json:"through_n"`
	Digest   string         `json:"digest"`
	Refused  []refusedEntry `json:"refused"`
}

type refusedEntry struct {
	N    int64             `json:"n"`
	Code billing.ErrorCode `json:"code"`
}

// answer is a refused item's answer rebuilt from its receipt.
func (e refusedEntry) answer() billing.LeaseUsageRefusal {
	status, typ, meaning := http.StatusUnprocessableEntity, billing.ErrorType(""), "the usage item was refused when it settled"
	if info, ok := billing.LookupErrorCode(e.Code); ok {
		status, typ = info.Status, info.Type
		if info.Meaning != "" {
			meaning = info.Meaning
		}
	}
	if typ == "" {
		typ = api.TypeForCode(status, e.Code)
	}
	return billing.LeaseUsageRefusal{N: e.N, Status: status, Error: &billing.ErrorDetails{Type: typ, Code: e.Code,
		Message: fmt.Sprintf("usage item %d was refused when it settled: %s", e.N, meaning)}}
}

func invalid(param, format string, args ...any) error {
	return apperr.Invalidf(format, args...).WithParam(param)
}

var (
	limitKeyPattern = regexp.MustCompile(`^([a-z][a-z0-9_-]{0,63}|spend:[0-9a-f]{64})$`)
	unitPattern     = regexp.MustCompile(`^([a-z][a-z0-9_]{0,63}|[A-Z0-9]{3,12})$`)
)

// validate refuses a sync whose shape is wrong before anything is read. A
// view request or usage item is refused alone, never the sync.
func validate(p billing.SyncLeasesParams) error {
	switch {
	case p.LeaseID.IsZero():
		return invalid("lease_id", "lease_id is required")
	case p.LeaseID.UUID().Version() != 7:
		return invalid("lease_id", "lease_id must be a UUIDv7")
	case len(p.Usage) > billing.MaxSyncUsageItems:
		return invalid("usage", "a sync carries at most %d usage items", billing.MaxSyncUsageItems)
	case len(p.InFlight) > billing.MaxSyncInFlight:
		return invalid("in_flight", "a sync carries at most %d in-flight lines", billing.MaxSyncInFlight)
	case len(p.Views) > billing.MaxSyncViews:
		return invalid("views", "a sync asks for at most %d views", billing.MaxSyncViews)
	}
	for i, f := range p.InFlight {
		switch {
		case f.CustomerID.IsZero():
			return invalid(fmt.Sprintf("in_flight[%d].customer_id", i), "customer_id is required")
		case !limitKeyPattern.MatchString(f.Limit):
			return invalid(fmt.Sprintf("in_flight[%d].limit", i), "limit must be balance, a limit key or a spend window")
		case !unitPattern.MatchString(f.Unit):
			return invalid(fmt.Sprintf("in_flight[%d].unit", i), "unit must be a unit or currency code")
		case f.Amount < 0:
			return invalid(fmt.Sprintf("in_flight[%d].amount", i), "amount must be nonnegative")
		}
	}
	for i, u := range p.Usage {
		if u.N <= 0 {
			return invalid(fmt.Sprintf("usage[%d].n", i), "n must be positive")
		}
		if i > 0 && u.N <= p.Usage[i-1].N {
			return invalid(fmt.Sprintf("usage[%d].n", i), "usage items are sent in order of n")
		}
	}
	return nil
}

// Sync is one sync of a lease.
func (s Syncer) Sync(ctx context.Context, p billing.SyncLeasesParams) (*billing.SyncLeasesResult, error) {
	if err := validate(p); err != nil {
		return nil, err
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	// A failed report's windows come from the merchant's policies.
	type windowKey struct {
		customer    billing.CustomerID
		invokerType billing.InvokerType
		currency    string
	}
	failed := map[windowKey][]money.FailedUsageWindow{}
	for _, u := range p.Usage {
		if u.Outcome != billing.UsageFailed || u.CustomerID.IsZero() || moneyutil.ValidateCurrency(money.NormalizeCurrency(string(u.Currency))) != nil {
			continue
		}
		k := windowKey{u.CustomerID, cmp.Or(u.InvokerType, billing.InvokerTypeCustomer), money.NormalizeCurrency(string(u.Currency))}
		if _, ok := failed[k]; ok || s.FailedWindows == nil {
			continue
		}
		if failed[k], err = s.FailedWindows(ctx, identity.CustomerID(u.CustomerID), k.invokerType, k.currency); err != nil {
			return nil, err
		}
	}
	var out *billing.SyncLeasesResult
	var run *syncRun
	began := time.Now()
	err = s.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		if _, err := q.SetConfig(ctx, gen.SetConfigParams{Setting: "transaction_timeout", Value: strconv.Itoa(int(limits.SyncTimeout / time.Millisecond)), IsLocal: true}); err != nil {
			return err
		}
		if err := setLockTimeout(ctx, q, lockBudget); err != nil {
			return err
		}
		now := s.Now().UTC().Truncate(time.Microsecond)
		d := s.DB.NewWithPgxTx(tx)
		run = &syncRun{Syncer: s, ctx: ctx, q: q, merchant: mid.UUID(), now: now, p: p, began: began,
			money: s.Money.WithDatabase(d), admitter: s.Admitter.Bind(d, func() time.Time { return now }), failed: func(u billing.LeaseUsage) []money.FailedUsageWindow {
				return failed[windowKey{u.CustomerID, cmp.Or(u.InvokerType, billing.InvokerTypeCustomer), money.NormalizeCurrency(string(u.Currency))}]
			}}
		var err error
		if out, err = run.sync(); err != nil {
			return err
		}
		return run.withinTimeout()
	})
	if lockTimedOut(err) {
		return nil, errLockBudget
	}
	if err != nil {
		return nil, err
	}
	// The balances its usage moved are watched, outside the transaction: a
	// watch it misses (a crash between) looks at its own next instant.
	if err := s.Money.WatchUsage(ctx, s.DB, run.changed); err != nil && s.DB.Log != nil {
		s.DB.Log.WarnContext(ctx, "lease sync: watching the balances its usage moved failed; each looks at its next instant", logx.Err(err))
	}
	// A failure here leaves the sync committed: its answer is lost, and the
	// gate's resend answers the receipts and fresh views.
	if err := run.views(ctx); err != nil {
		return nil, err
	}
	return out, nil
}

// errLockBudget answers a sync that could not take its locks within
// lockBudget: it committed nothing and is sent again.
var errLockBudget = apperr.New(http.StatusServiceUnavailable, billing.CodeServiceUnavailable, "the sync could not take its locks in time; retry it")

// lockTimedOut reports a statement that waited out lock_timeout, or was
// chosen to break a deadlock: chunks of two syncs may take the same
// customers' rows in different orders.
func lockTimedOut(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && (pg.Code == "55P03" || pg.Code == "40P01")
}

func setLockTimeout(ctx context.Context, q *gen.Queries, d time.Duration) error {
	_, err := q.SetConfig(ctx, gen.SetConfigParams{Setting: "lock_timeout", Value: strconv.FormatInt(max(d.Milliseconds(), 1), 10), IsLocal: true})
	return err
}

// withinTimeout is a sync's last statement: one that ran past its timeout
// less the margin rolls back.
func (r *syncRun) withinTimeout() error {
	elapsed, err := r.q.SyncElapsed(r.ctx)
	if err != nil {
		return err
	}
	if time.Duration(elapsed)*time.Microsecond > limits.SyncTimeout-commitMargin {
		return apperr.New(http.StatusServiceUnavailable, billing.CodeServiceUnavailable, "the sync ran too long to commit; retry it")
	}
	return nil
}

// syncRun is one sync's transaction.
type syncRun struct {
	Syncer
	ctx      context.Context
	q        *gen.Queries
	merchant uuid.UUID
	now      time.Time
	p        billing.SyncLeasesParams
	money    *money.MoneyService
	admitter *admission.Admitter
	failed   func(billing.LeaseUsage) []money.FailedUsageWindow
	// began is the sync's start by the real clock.
	began time.Time

	lease  gen.BillingLease
	result *billing.SyncLeasesResult
	prefix prefix
	// spend caches the billing policy of each item's attributes.
	spend map[string]*admission.SpendPolicy
	// changed are the balances its usage moved.
	changed []money.UsageAccount
	// viewable are the currencies and units the views may be asked in.
	viewable map[string]bool
}

// prefix is the run of new usage items a sync may settle: from first, the
// items whose numbers follow on (end exclusive). gap is an item at end that
// skips a number.
type prefix struct {
	first, end int
	gap        bool
}

// usagePrefix finds the sync's prefix of new usage: items at or below the
// lease's settled number are resends.
func (r *syncRun) usagePrefix() prefix {
	through := r.lease.SettledThroughN
	var p prefix
	for p.first < len(r.p.Usage) && r.p.Usage[p.first].N <= through {
		p.first++
	}
	last := through
	p.end = p.first
	for i := p.first; i < len(r.p.Usage); i++ {
		u := r.p.Usage[i]
		if u.N != last+1 {
			p.gap = true
			break
		}
		last = u.N
		p.end = i + 1
	}
	return p
}

func (r *syncRun) sync() (*billing.SyncLeasesResult, error) {
	if err := r.lockLease(); err != nil {
		return nil, err
	}
	if _, err := r.q.ReadMerchantSettingsLock(r.ctx, r.merchant); err != nil {
		return nil, err
	}
	r.prefix = r.usagePrefix()
	r.spend = map[string]*admission.SpendPolicy{}
	r.result = &billing.SyncLeasesResult{Views: []billing.View{}, LimitSets: make([]*billing.LimitSet, len(r.p.Views)), RefusedViews: []billing.ViewRefusal{}, Pricing: []billing.ViewPricing{},
		Usage: billing.LeaseUsageResult{Refused: []billing.LeaseUsageRefusal{}}}
	// Usage settles first, always.
	through, receipts, err := r.settle()
	if err != nil {
		return nil, err
	}
	closedAt := r.lease.ClosedAt
	if r.p.Close || closedAt != nil {
		if err := r.q.ClearLeaseInFlight(r.ctx, gen.ClearLeaseInFlightParams{MerchantID: r.merchant, LeaseID: r.lease.ID}); err != nil {
			return nil, err
		}
	} else if err := r.report(); err != nil {
		return nil, err
	}
	if r.p.Close && closedAt == nil {
		if err := r.q.CloseLease(r.ctx, gen.CloseLeaseParams{ClosedAt: r.now, MerchantID: r.merchant, ID: r.lease.ID}); err != nil {
			return nil, err
		}
		closedAt = &r.now
		if r.now.Before(r.lease.CreatedAt) {
			closedAt = &r.lease.CreatedAt
		}
	}
	encoded, err := encodeReceipts(receipts)
	if err != nil {
		return nil, err
	}
	if err := r.q.UpdateLeaseSettlement(r.ctx, gen.UpdateLeaseSettlementParams{SettledThroughN: through, Receipts: encoded, MerchantID: r.merchant, ID: r.lease.ID}); err != nil {
		return nil, err
	}
	lastSync := r.lease.LastSyncAt
	if closedAt == nil && r.now.After(lastSync) {
		lastSync = r.now
	}
	expires := lastSync.Add(limits.LeaseLifetime)
	if closedAt != nil && closedAt.Before(expires) {
		expires = *closedAt
	}
	live, err := r.q.CountLiveLeases(r.ctx, gen.CountLiveLeasesParams{MerchantID: r.merchant, LiveSince: limits.LeaseLiveSince(r.now)})
	if err != nil {
		return nil, err
	}
	r.result.Lease = billing.Lease{ID: r.p.LeaseID, ExpiresAt: expires}
	r.result.Usage.ThroughN = through
	r.result.Gates = int(max(live, 1))
	return r.result, nil
}

// lockLease locks the lease, creating it on its first sync, and renews it
// unless it was closed. An id older than the replay horizon with no lease
// names one that may already be forgotten: its usage must not post again.
func (r *syncRun) lockLease() error {
	id := r.p.LeaseID.UUID()
	row, err := r.q.LockLease(r.ctx, gen.LockLeaseParams{MerchantID: r.merchant, ID: id})
	switch {
	case err == nil:
	case errors.Is(err, pgx.ErrNoRows):
		sec, nsec := id.Time().UnixTime()
		if time.Unix(sec, nsec).Before(r.now.Add(-(retention.Leases - leaseIDSkew))) {
			return apperr.New(http.StatusNotFound, billing.CodeLeaseNotFound, "the lease is older than the replay horizon and no longer kept").WithParam("lease_id")
		}
		// A batch job numbers its items from its own cursor: a new lease
		// starts just before its first item.
		base := int64(0)
		if len(r.p.Usage) > 0 {
			base = r.p.Usage[0].N - 1
		}
		row, err = r.q.InsertLease(r.ctx, gen.InsertLeaseParams{MerchantID: r.merchant, ID: id, Now: r.now, SettledThroughN: base})
		if errors.Is(err, pgx.ErrNoRows) {
			row, err = r.q.LockLease(r.ctx, gen.LockLeaseParams{MerchantID: r.merchant, ID: id})
		}
		if err != nil {
			return err
		}
	default:
		return err
	}
	r.lease = row
	if r.p.Close || row.ClosedAt != nil {
		return nil
	}
	return r.q.RenewLease(r.ctx, gen.RenewLeaseParams{Now: r.now, MerchantID: r.merchant, ID: id})
}

// report replaces the lease's in-flight report, writing only the lines that
// changed: its own rows, never the customer's.
func (r *syncRun) report() error {
	type line struct {
		customer    uuid.UUID
		limit, unit string
	}
	want := map[line]int64{}
	for _, f := range r.p.InFlight {
		if f.Amount == 0 || (r.Allows != nil && !r.Allows(f.CustomerID)) {
			continue
		}
		k := line{f.CustomerID.UUID(), f.Limit, f.Unit}
		want[k] = satAdd(want[k], f.Amount)
	}
	rows, err := r.q.ListLeaseInFlight(r.ctx, gen.ListLeaseInFlightParams{MerchantID: r.merchant, LeaseID: r.lease.ID})
	if err != nil {
		return err
	}
	have := map[line]int64{}
	for _, row := range rows {
		k := line{row.CustomerID, row.LimitKey, row.Unit}
		have[k] = row.Amount
		if _, kept := want[k]; !kept {
			if err := r.q.DeleteLeaseInFlight(r.ctx, gen.DeleteLeaseInFlightParams{MerchantID: r.merchant, LeaseID: r.lease.ID,
				CustomerID: k.customer, LimitKey: k.limit, Unit: k.unit}); err != nil {
				return err
			}
		}
	}
	keys := slices.Collect(func(yield func(line) bool) {
		for k := range want {
			if !yield(k) {
				return
			}
		}
	})
	slices.SortFunc(keys, func(a, b line) int {
		return cmp.Or(compareUUID(a.customer, b.customer), strings.Compare(a.limit, b.limit), strings.Compare(a.unit, b.unit))
	})
	for _, k := range keys {
		if have[k] == want[k] {
			continue
		}
		if err := r.q.UpsertLeaseInFlight(r.ctx, gen.UpsertLeaseInFlightParams{MerchantID: r.merchant, LeaseID: r.lease.ID, CustomerID: k.customer,
			LimitKey: k.limit, Unit: k.unit, Amount: want[k], Now: r.now}); err != nil {
			return err
		}
	}
	return nil
}

// settle settles the sync's usage prefix in order and answers what it
// resent: through_n covers what settled, and items past it are neither
// settled nor refused.
func (r *syncRun) settle() (int64, []receipt, error) {
	var receipts []receipt
	if err := json.Unmarshal(r.lease.Receipts, &receipts); err != nil {
		return 0, nil, fmt.Errorf("decode lease receipts: %w", err)
	}
	through := r.lease.SettledThroughN
	refuse := func(n int64, err error) {
		status, details := Details(err)
		r.result.Usage.Refused = append(r.result.Usage.Refused, billing.LeaseUsageRefusal{N: n, Status: status, Error: &details})
	}
	// Resent items answer their settlements' receipts.
	digests := make([]string, len(r.p.Usage))
	for i, u := range r.p.Usage {
		digests[i] = itemDigest(u)
	}
	first := r.prefix.first
	for _, rc := range receipts {
		var covered []int
		for i := 0; i < first; i++ {
			if n := r.p.Usage[i].N; n >= rc.FromN && n <= rc.ThroughN {
				covered = append(covered, i)
			}
		}
		if len(covered) == 0 {
			continue
		}
		whole := int64(len(covered)) == rc.ThroughN-rc.FromN+1 && r.p.Usage[covered[0]].N == rc.FromN
		if whole && settlementDigest(digests[covered[0]:covered[len(covered)-1]+1]) != rc.Digest {
			for _, i := range covered {
				refuse(r.p.Usage[i].N, apperr.New(http.StatusUnprocessableEntity, billing.CodeIdempotencyKeyReused,
					fmt.Sprintf("usage item %d was settled with other terms", r.p.Usage[i].N)).WithParam(fmt.Sprintf("usage[%d]", i)))
			}
			continue
		}
		for _, ref := range rc.Refused {
			for _, i := range covered {
				if r.p.Usage[i].N == ref.N {
					r.result.Usage.Refused = append(r.result.Usage.Refused, ref.answer())
				}
			}
		}
	}
	// New items settle in order, chunk by chunk while the sync has time; a
	// gap stops them once every item before it settled.
	var refusals []billing.LeaseUsageRefusal
	refuseNew := func(i int, err error) {
		status, details := Details(err)
		refusals = append(refusals, billing.LeaseUsageRefusal{N: r.p.Usage[i].N, Status: status, Error: &details})
	}
	end := first
	for end < r.prefix.end {
		if end > first {
			if err := failpoint.Hit(r.ctx, failpoint.Site{Point: failpoint.AfterChunk, Kind: FailpointKind, Operation: r.lease.ID}); err != nil {
				return 0, nil, err
			}
			if time.Since(r.began) > UsageBudget {
				break
			}
		}
		stop := r.chunkEnd(end)
		if err := r.resolveSpend(r.p.Usage[end:stop]); err != nil {
			return 0, nil, err
		}
		var items []money.UsageItem
		var index []int
		for i := end; i < stop; i++ {
			u := r.p.Usage[i]
			item, err := r.usageItem(u)
			if err != nil {
				if _, coded := asCoded(err); !coded {
					return 0, nil, err
				}
				refuseNew(i, prefixParam(err, fmt.Sprintf("usage[%d]", i)))
				continue
			}
			r.identify(&item, u)
			items = append(items, item)
			index = append(index, i)
		}
		if len(items) > 0 {
			results, err := r.intake(items)
			if err != nil {
				return 0, nil, err
			}
			for j, res := range results {
				i := index[j]
				if res.Err != nil {
					if _, coded := asCoded(res.Err); !coded {
						return 0, nil, res.Err
					}
					refuseNew(i, prefixParam(res.Err, fmt.Sprintf("usage[%d]", i)))
				}
			}
		}
		end = stop
	}
	if r.prefix.gap && end == r.prefix.end {
		next := through + 1
		if end > first {
			next = r.p.Usage[end-1].N + 1
		}
		for j := end; j < len(r.p.Usage); j++ {
			refuse(r.p.Usage[j].N, apperr.New(http.StatusConflict, billing.CodeUsageOutOfOrder,
				fmt.Sprintf("usage item %d skips %d, which the lease has not settled", r.p.Usage[j].N, next)).WithParam(fmt.Sprintf("usage[%d].n", j)))
		}
	}
	if end == first {
		slices.SortFunc(r.result.Usage.Refused, func(a, b billing.LeaseUsageRefusal) int { return cmp.Compare(a.N, b.N) })
		return through, receipts, nil
	}
	last := r.p.Usage[end-1].N
	slices.SortFunc(refusals, func(a, b billing.LeaseUsageRefusal) int { return cmp.Compare(a.N, b.N) })
	r.result.Usage.Refused = append(r.result.Usage.Refused, refusals...)
	slices.SortFunc(r.result.Usage.Refused, func(a, b billing.LeaseUsageRefusal) int { return cmp.Compare(a.N, b.N) })
	rc := receipt{FromN: through + 1, ThroughN: last, Refused: make([]refusedEntry, 0, len(refusals))}
	for _, ref := range refusals {
		code := billing.CodeInternalError
		if ref.Error != nil {
			code = ref.Error.Code
		}
		rc.Refused = append(rc.Refused, refusedEntry{N: ref.N, Code: code})
	}
	rc.Digest = settlementDigest(digests[first:end])
	receipts = append(receipts, rc)
	return last, receipts, nil
}

// encodeReceipts keeps the newest receipts within maxReceipts and
// maxReceiptBytes; the newest one always stays.
func encodeReceipts(receipts []receipt) ([]byte, error) {
	if len(receipts) > maxReceipts {
		receipts = receipts[len(receipts)-maxReceipts:]
	}
	for {
		if len(receipts) == 0 {
			return []byte("[]"), nil
		}
		encoded, err := json.Marshal(receipts)
		if err != nil || len(encoded) <= maxReceiptBytes || len(receipts) == 1 {
			return encoded, err
		}
		receipts = receipts[1:]
	}
}

// checkItem refuses an item no settlement takes.
func (r *syncRun) checkItem(u billing.LeaseUsage) error {
	switch {
	case u.CustomerID.IsZero():
		return invalid("customer_id", "customer_id required")
	case r.Allows != nil && !r.Allows(u.CustomerID):
		return apperr.New(http.StatusForbidden, billing.CodeServiceCredentialCustomerScopeDenied, "the credential's customer scope does not include the customer").WithParam("customer_id")
	case len(u.SourceID) > 255 || len(u.Source) > 255:
		return invalid("source_id", "source and source_id must be at most 255 bytes")
	case u.Outcome != "" && u.Outcome != billing.UsageSucceeded && u.Outcome != billing.UsageFailed:
		return invalid("outcome", `outcome must be "succeeded" or "failed"`)
	case u.InvokerType != "" && u.InvokerType != billing.InvokerTypeCustomer && u.InvokerType != billing.InvokerTypeDelegated:
		return invalid("invoker_type", `invoker_type must be "customer" or "delegated"`)
	}
	return nil
}

// request is the admission request a usage item's invoker attributes name.
func request(u billing.LeaseUsage, currency string) admission.AdmitRequest {
	return admission.AdmitRequest{CustomerID: identity.CustomerID(u.CustomerID), Invoker: strings.TrimSpace(u.Invoker),
		InvokerType: string(cmp.Or(u.InvokerType, billing.InvokerTypeCustomer)), TrustLevel: strings.TrimSpace(deref(u.TrustLevel)),
		Roles: u.Roles, Resource: strings.TrimSpace(deref(u.Resource)), Currency: currency}
}

// chunkEnd is where the chunk of the prefix from start ends: at most
// usageChunkItems items naming at most UsageChunkCustomers customers.
func (r *syncRun) chunkEnd(start int) int {
	seen := map[billing.CustomerID]bool{}
	i := start
	for ; i < r.prefix.end && i-start < usageChunkItems; i++ {
		if c := r.p.Usage[i].CustomerID; !seen[c] {
			if len(seen) == UsageChunkCustomers {
				break
			}
			seen[c] = true
		}
	}
	return i
}

// spendRequest is the admission request whose billing policy counts an
// item's spend, and its key among the sync's policies; none for an item not
// in a registry currency.
func spendRequest(u billing.LeaseUsage) (string, admission.AdmitRequest, bool) {
	currency := money.NormalizeCurrency(string(u.Currency))
	if moneyutil.ValidateCurrency(currency) != nil {
		return "", admission.AdmitRequest{}, false
	}
	req := request(u, currency)
	roles := make([]string, len(req.Roles))
	for i, role := range req.Roles {
		roles[i] = role.String()
	}
	slices.Sort(roles)
	return strings.Join([]string{u.CustomerID.String(), req.Invoker, req.InvokerType, req.TrustLevel, strings.Join(roles, ","), req.Resource, currency}, "\x00"), req, true
}

// resolveSpend resolves in one batch the billing policies of the items'
// attributes the sync has not resolved yet. A policy that cannot be resolved
// now (an FX outage, a policy the deployment cannot enforce) counts its
// customer's usage in no spend window, as before usage had any: it still
// settles.
func (r *syncRun) resolveSpend(usage []billing.LeaseUsage) error {
	var keys []string
	var reqs []admission.AdmitRequest
	asked := map[string]bool{}
	for _, u := range usage {
		if r.checkItem(u) != nil {
			continue
		}
		key, req, ok := spendRequest(u)
		if _, done := r.spend[key]; !ok || done || asked[key] {
			continue
		}
		asked[key] = true
		keys, reqs = append(keys, key), append(reqs, req)
	}
	if len(reqs) == 0 {
		return nil
	}
	policies, errs, err := r.admitter.SpendPolicies(r.ctx, reqs)
	if err != nil {
		return err
	}
	for i, key := range keys {
		if errs[i] != nil {
			if !isolated(errs[i]) {
				return errs[i]
			}
			policies[i] = nil
		}
		r.spend[key] = policies[i]
	}
	return nil
}

// usageItem is a lease's usage report as intake takes it, priced by the
// cards in force and counted in the spend windows its attributes resolve to.
func (r *syncRun) usageItem(u billing.LeaseUsage) (money.UsageItem, error) {
	if err := r.checkItem(u); err != nil {
		return money.UsageItem{}, err
	}
	item := money.UsageItem{Customer: identity.CustomerID(u.CustomerID), Invoker: u.Invoker, Delegated: u.InvokerType == billing.InvokerTypeDelegated,
		Currency: string(u.Currency), EventType: u.EventType, Dimensions: u.Dimensions, Metadata: u.Metadata, Amount: u.Amount,
		Failed: u.Outcome == billing.UsageFailed, OccurredAt: u.OccurredAt, AdmittedAt: u.AdmittedAt, Source: u.Source, SourceID: u.SourceID}
	if item.Failed {
		item.FailedWindows = r.failed(u)
		if strings.TrimSpace(u.EventType) != "" {
			key, err := money.NewIdempotencyKey(money.UsageOperation(u.EventType), r.p.LeaseID.String(), strconv.FormatInt(u.N, 10))
			if err != nil {
				return money.UsageItem{}, invalid("event_type", "%s", err.Error())
			}
			item.FailedKey = key
		}
	}
	key, _, ok := spendRequest(u)
	if !ok {
		return item, nil
	}
	at := r.now
	if u.AdmittedAt != nil && u.AdmittedAt.Before(r.now) {
		at = u.AdmittedAt.UTC()
	}
	policy, resolved := r.spend[key]
	if !resolved {
		if err := r.resolveSpend([]billing.LeaseUsage{u}); err != nil {
			return money.UsageItem{}, err
		}
		policy = r.spend[key]
	}
	spend, err := policy.Windows(at)
	if err != nil {
		if !isolated(err) {
			return money.UsageItem{}, err
		}
		spend = nil
	}
	item.Spend = spend
	return item, nil
}

// isolated reports an error that concerns one customer alone: not one of
// the transaction (a database error aborts it), of the event store (out of
// reach, the whole sync answers 503) or of the request's context.
func isolated(err error) bool {
	var pg *pgconn.PgError
	return !errors.As(err, &pg) && !errors.Is(err, eventstore.ErrUnavailable) && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, pgx.ErrTxClosed)
}

// Details is an item or view refusal on the wire.
func Details(err error) (int, billing.ErrorDetails) {
	if coded, ok := asCoded(err); ok {
		d := billing.ErrorDetails{Type: api.TypeForCode(coded.Status, coded.Code), Code: coded.Code, Message: coded.Message, Metadata: coded.Metadata}
		if coded.Message == "" {
			d.Message = err.Error()
		}
		if coded.Param != "" {
			param := coded.Param
			d.Param = &param
		}
		return coded.Status, d
	}
	return http.StatusInternalServerError, billing.ErrorDetails{Type: api.TypeForCode(500, billing.CodeInternalError), Code: billing.CodeInternalError, Message: "settlement failed"}
}

// asCoded is a refusal in the wire's terms: an apperr, or a changed
// payload's idempotency conflict.
func asCoded(err error) (*apperr.Error, bool) {
	var coded *apperr.Error
	if errors.As(err, &coded) && coded.Status < 500 {
		return coded, true
	}
	if errors.Is(err, money.ErrIdempotencyKeyReused) {
		return apperr.New(http.StatusUnprocessableEntity, billing.CodeIdempotencyKeyReused, err.Error()), true
	}
	return nil, false
}

// prefixParam names a refused field under its item.
func prefixParam(err error, prefix string) error {
	coded, ok := asCoded(err)
	if !ok {
		return err
	}
	out := *coded
	if out.Param != "" {
		out.Param = prefix + "." + out.Param
	} else {
		out.Param = prefix
	}
	return &out
}

// itemDigest names an item's terms: a resend under its number must repeat
// them.
func itemDigest(u billing.LeaseUsage) string {
	u.OccurredAt = u.OccurredAt.UTC().Truncate(time.Microsecond)
	if u.AdmittedAt != nil {
		at := u.AdmittedAt.UTC().Truncate(time.Microsecond)
		u.AdmittedAt = &at
	}
	u.Currency = catalog.Currency(money.NormalizeCurrency(string(u.Currency)))
	raw, _ := json.Marshal(u)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func settlementDigest(digests []string) string {
	h := sha256.New()
	for _, d := range digests {
		h.Write([]byte(d))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func compareUUID(a, b uuid.UUID) int { return strings.Compare(string(a[:]), string(b[:])) }

func deref[T any](v *T) T {
	var zero T
	if v == nil {
		return zero
	}
	return *v
}

func satAdd(a, b int64) int64 {
	const maxInt64 = int64(^uint64(0) >> 1)
	switch {
	case b > 0 && a > maxInt64-b:
		return maxInt64
	case b < 0 && a < -maxInt64-1-b:
		return -maxInt64 - 1
	}
	return a + b
}
