// Package leases syncs gates' leases: it settles their usage in order,
// answers their wants with holds sized from traffic, and takes back the holds
// they return, in one transaction per sync.
package leases

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
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
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/admission"
	"github.com/open-rails/openrails/internal/modules/limits"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/retention"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// Sizing and bookkeeping constants. None is a setting: the hold rule holds
// for any value, and only speed depends on them.
const (
	// HeadroomShare: one customer's holds on a limit take at most
	// 1/HeadroomShare of what is free there.
	HeadroomShare = 2
	// maxReceipts and maxReceiptBytes bound the receipts a lease keeps: a
	// gate resends only what its lost replies left unacknowledged.
	maxReceipts     = 64
	maxReceiptBytes = 128 << 10
	// reissueAge is how old a lease's hold row grows before it is issued
	// again, well inside the hold lifetime.
	reissueAge = 7 * retention.Day
	// urgentAge is how old a hold row grows before its reissue goes ahead
	// of any other work: well before the hold lifetime, past which it counts
	// nowhere.
	urgentAge = retention.AdmissionMaxHold - reissueAge
	// rolloverAhead is how soon before a fixed window closes an active hold
	// gets its next window's hold.
	rolloverAhead = 2 * limits.SyncInterval
	// leaseIDSkew is how far a lease id's time may run ahead of the server
	// clock: an id with no lease older than the replay horizon less it names
	// a lease that may already be forgotten.
	leaseIDSkew = retention.Day
	// maxRenewals bounds the holds one sync renews, reissues, rolls ahead or
	// releases on close beyond what its own items name, most urgent first:
	// the rest wait for later syncs, so a window that closes for every
	// customer at once never runs a sync past its timeout.
	maxRenewals = 64
	// commitMargin is how far inside limits.SyncTimeout a sync must reach its
	// commit; one that ran longer is refused and retried.
	commitMargin = time.Second
	// workBudget is how far into its transaction a sync still starts a want
	// or a renewal, well inside the commit guard: a want it has no time for
	// is answered 503 service_unavailable and sent again, the renewals wait.
	workBudget = 2500 * time.Millisecond
	// lockBudget is how far into its transaction a sync may still wait for
	// a lock, just under the work budget: lock_timeout bounds every wait by
	// what is left of it, so a sync that cannot take its locks in time is
	// answered a retryable 503 long before the commit guard. Past
	// lockTimeoutStep of drift the timeout is cut to what is left.
	lockBudget      = workBudget - 500*time.Millisecond
	lockTimeoutStep = 250 * time.Millisecond
	// A sync settles a prefix of its new usage: the items of at most
	// maxSettleCustomers customers, in chunks of settleChunk customers, and
	// no chunk after the first once settleBudget of real transaction time
	// has passed. The rest are neither settled nor refused, and come again,
	// so a backlog drains over several syncs inside the commit guard.
	maxSettleCustomers = 100
	settleChunk        = 20
	settleBudget       = 1500 * time.Millisecond
)

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
	N    int64  `json:"n"`
	Code string `json:"code"`
}

// answer is a refused item's answer rebuilt from its receipt.
func (e refusedEntry) answer() billing.LeaseUsageRefusal {
	status, typ, meaning := http.StatusUnprocessableEntity, "", "the usage item was refused when it settled"
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

// validate refuses a sync whose shape is wrong before anything is read.
func validate(p billing.SyncLeasesParams) error {
	switch {
	case p.LeaseID.IsZero():
		return invalid("lease_id", "lease_id is required")
	case p.LeaseID.UUID().Version() != 7:
		return invalid("lease_id", "lease_id must be a UUIDv7")
	case len(p.Usage) > billing.MaxSyncUsageItems:
		return invalid("usage", "a sync carries at most %d usage items", billing.MaxSyncUsageItems)
	case len(p.Wants) > billing.MaxSyncWants:
		return invalid("wants", "a sync carries at most %d wants", billing.MaxSyncWants)
	case len(p.Returns) > billing.MaxSyncReturns:
		return invalid("returns", "a sync carries at most %d returns", billing.MaxSyncReturns)
	case len(p.Committed) > billing.MaxSyncCommitted:
		return invalid("committed", "a sync carries at most %d commitments", billing.MaxSyncCommitted)
	case len(p.Applied) > billing.MaxSyncApplied:
		return invalid("applied", "a sync carries at most %d applied versions", billing.MaxSyncApplied)
	}
	for i, c := range p.Committed {
		for j, l := range c.Limits {
			if l.Committed < 0 {
				return invalid(fmt.Sprintf("committed[%d].limits[%d].committed", i, j), "committed must be nonnegative")
			}
		}
	}
	for i, a := range p.Applied {
		if a.Version < 0 {
			return invalid(fmt.Sprintf("applied[%d].version", i), "version must be nonnegative")
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
		if u.Outcome != billing.UsageFailed || u.CustomerID.IsZero() || moneyutil.ValidateCurrency(money.NormalizeCurrency(u.Currency)) != nil {
			continue
		}
		k := windowKey{u.CustomerID, cmp.Or(u.InvokerType, billing.InvokerTypeCustomer), money.NormalizeCurrency(u.Currency)}
		if _, ok := failed[k]; ok || s.FailedWindows == nil {
			continue
		}
		if failed[k], err = s.FailedWindows(ctx, identity.CustomerID(u.CustomerID), k.invokerType, k.currency); err != nil {
			return nil, err
		}
	}
	s.DB.EnsurePartitions(ctx, s.Now().UTC())
	var out *billing.SyncLeasesResult
	err = s.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		began := time.Now()
		q := gen.New(tx)
		// A sync's transaction is bounded: readers count a lease live for
		// this much longer than its lifetime.
		if _, err := q.SetConfig(ctx, gen.SetConfigParams{Setting: "transaction_timeout", Value: strconv.Itoa(int(limits.SyncTimeout / time.Millisecond)), IsLocal: true}); err != nil {
			return err
		}
		if err := setLockTimeout(ctx, q, lockBudget); err != nil {
			return err
		}
		now := s.Now().UTC().Truncate(time.Microsecond)
		d := s.DB.NewWithPgxTx(tx)
		run := &syncRun{Syncer: s, ctx: ctx, q: q, d: d, merchant: mid.UUID(), now: now, p: p, began: began, lockTimeout: lockBudget,
			money: s.Money.WithDatabase(d), admitter: s.Admitter.Bind(d, func() time.Time { return now }), failed: func(u billing.LeaseUsage) []money.FailedUsageWindow {
				return failed[windowKey{u.CustomerID, cmp.Or(u.InvokerType, billing.InvokerTypeCustomer), money.NormalizeCurrency(u.Currency)}]
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
	return out, err
}

// errLockBudget answers a sync that could not take its locks within
// lockBudget: it committed nothing and is sent again.
var errLockBudget = apperr.New(http.StatusServiceUnavailable, billing.CodeServiceUnavailable, "the sync could not take its locks in time; retry it")

// lockTimedOut reports a statement that waited out lock_timeout.
func lockTimedOut(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "55P03"
}

func setLockTimeout(ctx context.Context, q *gen.Queries, d time.Duration) error {
	_, err := q.SetConfig(ctx, gen.SetConfigParams{Setting: "lock_timeout", Value: strconv.FormatInt(max(d.Milliseconds(), 1), 10), IsLocal: true})
	return err
}

// lockWaiting takes the customer locks a sync waits for, in the order
// given, within lockBudget of the transaction's real time: before a wait,
// lock_timeout is cut to what is left once that has drifted lockTimeoutStep
// below it, and set back after, so later statements are not held to the
// remainder.
func (r *syncRun) lockWaiting(ids []uuid.UUID) error {
	for _, id := range ids {
		left := lockBudget - time.Since(r.began)
		if left <= 0 {
			return errLockBudget
		}
		if left < r.lockTimeout-lockTimeoutStep {
			if err := setLockTimeout(r.ctx, r.q, left); err != nil {
				return err
			}
			r.lockTimeout = left
		}
		if err := db.EnsureCustomerRowQ(r.ctx, r.q, r.merchant, id); err != nil {
			return err
		}
		if _, err := r.q.LockCustomerForSpend(r.ctx, gen.LockCustomerForSpendParams{MerchantID: r.merchant, ID: id}); err != nil {
			return err
		}
	}
	if r.lockTimeout == lockBudget {
		return nil
	}
	r.lockTimeout = lockBudget
	return setLockTimeout(r.ctx, r.q, lockBudget)
}

// pastBudget reports a sync past budget, by the transaction's real elapsed
// time.
func (r *syncRun) pastBudget(budget time.Duration) (bool, error) {
	elapsed, err := r.q.SyncElapsed(r.ctx)
	return time.Duration(elapsed)*time.Microsecond > budget, err
}

// deferWant answers a want the sync had no time for: retryable, it comes
// again with the next sync.
func (r *syncRun) deferWant(i int) {
	param := fmt.Sprintf("wants[%d]", i)
	r.result.Wants[i] = billing.LeaseWantResult{Status: http.StatusServiceUnavailable, Error: &billing.ErrorDetails{
		Type: api.TypeForCode(http.StatusServiceUnavailable, billing.CodeServiceUnavailable), Code: billing.CodeServiceUnavailable, Param: &param,
		Message: "the sync ran out of time before this want; send it with the next sync"}}
}

// withinTimeout is a sync's last statement: one that ran past its timeout
// less the margin rolls back, since readers count its lease dead only that
// long after its lifetime. Elapsed is the transaction's real time, which a
// test's clock does not move.
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
	d        *db.DB
	merchant uuid.UUID
	now      time.Time
	p        billing.SyncLeasesParams
	money    *money.MoneyService
	admitter *admission.Admitter
	failed   func(billing.LeaseUsage) []money.FailedUsageWindow
	// began is the transaction's start by the real clock, and lockTimeout
	// the lock_timeout in force.
	began       time.Time
	lockTimeout time.Duration

	lease    gen.BillingLease
	live     bool
	prevSync time.Time
	holds    map[holdKey][]*hold
	defs     []limits.Definition
	meters   map[string]limits.Meter
	result   *billing.SyncLeasesResult
	touched  map[uuid.UUID]bool
	// locked are the customers whose locks the sync holds: it writes only
	// their holds' parts.
	locked map[uuid.UUID]bool
	// moved are the customers of the lease's holds whose standing moved
	// since its previous sync: their idle holds are sized again.
	moved map[uuid.UUID]bool
	// cards caches the rate cards in force per event type and currency.
	cards map[[2]string][]byte
	// version is the version this sync's answers carry, once one changed.
	version int64
	// answered maps a hold key to the want that answered it first.
	answered map[holdKey]int
	// safe is the merchant's usage_gate_safe_spend once read.
	safe *bool
	// usedBefore is each part's unsized usage before this sync settled.
	usedBefore map[partRef]int64
	// prefix is the new usage this sync may settle.
	prefix prefix
}

// prefix is the run of new usage items one sync may settle: from first, the
// items whose numbers follow on (end exclusive), naming at most
// maxSettleCustomers customers. gap is an item at end that skips a number.
type prefix struct {
	first, end int
	gap        bool
	customers  []billing.CustomerID
}

// usagePrefix finds the sync's prefix of new usage: items at or below the
// lease's settled number are resends.
func (r *syncRun) usagePrefix() prefix {
	through := r.lease.SettledThroughN
	var p prefix
	for p.first < len(r.p.Usage) && r.p.Usage[p.first].N <= through {
		p.first++
	}
	seen := map[billing.CustomerID]bool{}
	last := through
	p.end = p.first
	for i := p.first; i < len(r.p.Usage); i++ {
		u := r.p.Usage[i]
		if u.N != last+1 {
			p.gap = true
			break
		}
		if !u.CustomerID.IsZero() && !seen[u.CustomerID] {
			if len(seen) == maxSettleCustomers {
				break
			}
			seen[u.CustomerID] = true
			p.customers = append(p.customers, u.CustomerID)
		}
		last = u.N
		p.end = i + 1
	}
	return p
}

// chunkEnd is where the chunk of the prefix from start ends: before the item
// naming its (settleChunk+1)th customer.
func (r *syncRun) chunkEnd(start int) int {
	seen := map[billing.CustomerID]bool{}
	for i := start; i < r.prefix.end; i++ {
		c := r.p.Usage[i].CustomerID
		if !seen[c] {
			if len(seen) == settleChunk {
				return i
			}
			seen[c] = true
		}
	}
	return r.prefix.end
}

type partRef struct {
	hold uuid.UUID
	key  string
}

type holdKey struct {
	customer uuid.UUID
	limitSet string
}

// holdTerms are the invoker attributes a lease's hold was resolved for,
// which its renewals resolve again.
type holdTerms struct {
	Invoker     string      `json:"invoker"`
	InvokerType string      `json:"invoker_type"`
	TrustLevel  string      `json:"trust_level"`
	Roles       []uuid.UUID `json:"roles"`
	Resource    string      `json:"resource"`
	EventType   string      `json:"event_type"`
}

// hold is one of the lease's holds as the sync sees and changes it.
type hold struct {
	row     gen.BillingLimitHold
	terms   holdTerms
	parts   []*part
	version int64
	applied int64
	wanted  bool
	active  bool
	// changed is an answered size that changed: the hold takes the sync's
	// version and is answered.
	changed  bool
	released bool
	// replaced is a hold whose capacity moved to the one issued after it.
	replaced bool
	fresh    bool
	// versionDirty is a version or applied version to write.
	versionDirty bool
}

// part is one limit of a hold: held is what it sets aside, answered the size
// its gate was last told (less what usage took since) and committed what the
// gate's open tickets have taken of it, as this sync reports.
type part struct {
	key       string
	unit      string
	start     time.Time
	end       *time.Time
	held      int64
	answered  int64
	committed int64
	request   int64
	used      int64
	contended *time.Time
	overrun   catalog.Overrun
}

func (h *hold) part(key string) *part {
	for _, p := range h.parts {
		if p.key == key {
			return p
		}
	}
	return nil
}

// contains reports whether every windowed part of the hold covers at.
func (h *hold) contains(at time.Time) bool {
	for _, p := range h.parts {
		if p.key == catalog.BalanceLimit {
			continue
		}
		if at.Before(p.start) || (p.end != nil && !at.Before(*p.end)) {
			return false
		}
	}
	return true
}

// pending reports a confirmed hold that sets aside more than its answer and
// its tickets need: the sync releases the rest.
func (h *hold) pending() bool {
	if h.applied < h.version {
		return false
	}
	for _, p := range h.parts {
		if p.held > max(p.answered, p.committed) {
			return true
		}
	}
	return false
}

func (r *syncRun) sync() (*billing.SyncLeasesResult, error) {
	if err := r.lockLease(); err != nil {
		return nil, err
	}
	if _, err := r.q.ReadMerchantSettingsLock(r.ctx, r.merchant); err != nil {
		return nil, err
	}
	var err error
	if r.defs, err = limits.Definitions(r.ctx, r.q, r.merchant); err != nil {
		return nil, err
	}
	if len(r.defs) > 0 {
		if r.meters, err = limits.Meters(r.ctx, r.q, r.merchant); err != nil {
			return nil, err
		}
	}
	if err := r.loadHolds(); err != nil {
		return nil, err
	}
	if err := r.findMoved(); err != nil {
		return nil, err
	}
	r.prefix = r.usagePrefix()
	if err := r.lockCustomers(); err != nil {
		return nil, err
	}
	r.touched, r.answered = map[uuid.UUID]bool{}, map[holdKey]int{}
	r.result = &billing.SyncLeasesResult{Wants: make([]billing.LeaseWantResult, len(r.p.Wants)), Holds: []billing.Hold{}, Limits: []billing.CustomerLimit{},
		Usage: billing.LeaseUsageResult{Refused: []billing.LeaseUsageRefusal{}}}
	r.usedBefore = map[partRef]int64{}
	for _, holds := range r.holds {
		for _, h := range holds {
			for _, p := range h.parts {
				r.usedBefore[partRef{h.row.ID, p.key}] = p.used
			}
		}
	}
	// Usage settles first, always.
	through, receipts, err := r.settle()
	if err != nil {
		return nil, err
	}
	// Settlement took what the usage used from the holds; each one it took
	// from is answered. The holds are read again under the customer locks.
	if err := r.loadHolds(); err != nil {
		return nil, err
	}
	if r.live {
		for _, holds := range r.holds {
			for _, h := range holds {
				for _, p := range h.parts {
					if p.used > r.usedBefore[partRef{h.row.ID, p.key}] && r.locked[h.row.CustomerID] {
						h.changed, h.active = true, true
					}
				}
			}
		}
	}
	// What the gate gave up is released before wants are sized, so no want
	// is refused capacity its own gate already returned: acknowledged
	// shrinks, a close and returns.
	if r.live {
		if err := r.confirm(); err != nil {
			return nil, err
		}
	}
	if r.p.Close {
		// A closing sync, its usage settled, returns the holds of the
		// customers it locked; the closed lease's others count nowhere.
		if err := r.releaseEvery(); err != nil {
			return nil, err
		}
	}
	if err := r.giveBack(); err != nil {
		return nil, err
	}
	for i := range r.p.Wants {
		over, err := r.pastBudget(workBudget)
		if err != nil {
			return nil, err
		}
		if over {
			r.deferWant(i)
			continue
		}
		if err := r.want(i); err != nil {
			return nil, err
		}
	}
	if r.live && !r.p.Close {
		if err := r.renew(); err != nil {
			return nil, err
		}
	}
	if err := r.writeVersions(); err != nil {
		return nil, err
	}
	encoded, err := encodeReceipts(receipts)
	if err != nil {
		return nil, err
	}
	if err := r.q.UpdateLeaseSettlement(r.ctx, gen.UpdateLeaseSettlementParams{SettledThroughN: through, Receipts: encoded,
		HoldVersion: max(r.lease.HoldVersion, r.version), MerchantID: r.merchant, ID: r.lease.ID}); err != nil {
		return nil, err
	}
	lastSync := r.prevSync
	if r.live && !r.p.Close && r.now.After(lastSync) {
		lastSync = r.now
	}
	expires, closedAt := lastSync.Add(limits.LeaseLifetime), r.lease.ClosedAt
	if r.p.Close && closedAt == nil {
		if err := r.q.CloseLease(r.ctx, gen.CloseLeaseParams{ClosedAt: r.now, MerchantID: r.merchant, ID: r.lease.ID}); err != nil {
			return nil, err
		}
		closedAt = &r.now
		if r.now.Before(r.lease.CreatedAt) {
			closedAt = &r.lease.CreatedAt
		}
	}
	if closedAt != nil && closedAt.Before(expires) {
		expires = *closedAt
	}
	r.result.Lease = billing.Lease{ID: r.p.LeaseID, ExpiresAt: expires, Live: r.live}
	r.result.Usage.ThroughN = through
	if err := r.answer(); err != nil {
		return nil, err
	}
	return r.result, nil
}

// lockLease locks the lease, creating it on its first sync, and renews it
// while it is live. A dead lease is not renewed: its holds never reopen. An
// id older than the replay horizon with no lease names one that may already
// be forgotten: its usage must not post again.
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
	r.lease, r.prevSync = row, row.LastSyncAt
	if r.p.Close {
		r.live = row.ClosedAt == nil && row.LastSyncAt.After(r.now.Add(-limits.LeaseLifetime))
		return nil
	}
	renewed, err := r.q.RenewLease(r.ctx, gen.RenewLeaseParams{Now: r.now, MerchantID: r.merchant, ID: id, LiveSince: r.now.Add(-limits.LeaseLifetime)})
	r.live = renewed == 1
	return err
}

// findMoved reads which customers of the live lease's holds settled or
// accrued since its previous sync.
func (r *syncRun) findMoved() error {
	r.moved = map[uuid.UUID]bool{}
	if !r.live || len(r.holds) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, 0, len(r.holds))
	seen := map[uuid.UUID]bool{}
	for k := range r.holds {
		if !seen[k.customer] {
			seen[k.customer] = true
			ids = append(ids, k.customer)
		}
	}
	moved, err := r.q.ListCustomersMovedSince(r.ctx, gen.ListCustomersMovedSinceParams{MerchantID: r.merchant, CustomerIds: ids,
		Since: r.prevSync, AccruedSince: r.prevSync.Add(-retention.UsageIngestWindow).Truncate(24 * time.Hour)})
	for _, c := range moved {
		r.moved[c] = true
	}
	return err
}

// lockCustomers locks, in id order, the customers of the usage prefix, those
// the sync's wants, returns and applied versions name, and those of the most
// urgent holds no item names whose work frees capacity others wait for
// (class 0, but for a close: a closed lease's holds count nowhere); then,
// without waiting, the customers of the other holds within the bound: one
// held elsewhere is skipped and its holds wait for a later sync. The sync
// writes a lease's parts only under their customer's lock, as every other
// writer of them does.
func (r *syncRun) lockCustomers() error {
	r.locked = map[uuid.UUID]bool{}
	var ids []uuid.UUID
	add := func(c billing.CustomerID) {
		if c.IsZero() || r.locked[c.UUID()] || (r.Allows != nil && !r.Allows(c)) {
			return
		}
		r.locked[c.UUID()] = true
		ids = append(ids, c.UUID())
	}
	for _, c := range r.prefix.customers {
		add(c)
	}
	for _, w := range r.p.Wants {
		add(w.CustomerID)
	}
	for _, ret := range r.p.Returns {
		add(ret.CustomerID)
	}
	if r.live || r.p.Close {
		for _, a := range r.p.Applied {
			if len(r.holds[holdKey{a.CustomerID.UUID(), a.LimitSet}]) > 0 {
				add(a.CustomerID)
			}
		}
	}
	// The most urgent holds no item names, within the bound.
	var extra []uuid.UUID
	if r.live || r.p.Close {
		tasks, err := r.rank(func(holdKey) bool { return true })
		if err != nil {
			return err
		}
		for _, t := range tasks[:min(len(tasks), maxRenewals)] {
			c := t.key.customer
			switch {
			case r.Allows != nil && !r.Allows(billing.CustomerID(c)):
			case t.class == 0 && !r.p.Close:
				add(billing.CustomerID(c))
			case !r.locked[c] && !slices.Contains(extra, c):
				extra = append(extra, c)
			}
		}
	}
	slices.SortFunc(ids, compareUUID)
	if err := r.lockWaiting(ids); err != nil {
		return err
	}
	slices.SortFunc(extra, compareUUID)
	for _, id := range extra {
		_, err := r.q.TryLockCustomerForSpend(r.ctx, gen.TryLockCustomerForSpendParams{MerchantID: r.merchant, ID: id})
		switch {
		case err == nil:
			r.locked[id] = true
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
	}
	return nil
}

// task is a hold the sync may change beyond what its items name, or a key's
// roll-ahead, ranked by urgency.
type task struct {
	key  holdKey
	hold *hold
	// class: 0 a window closed or closes before the next sync, another
	// lease waits for it, its gate gave capacity up, its row nears the hold
	// lifetime, or the lease closes; 1 usage took from it; 2 it grew old or
	// its cards changed; 3 its customer's standing moved; 4 it shrinks while
	// idle.
	class int
	// left is how many requests a used hold still covers.
	left int64
}

// rank lists the work the lease's open holds need, of the keys in, most
// urgent first: a used hold the fewer requests it still covers, any other
// the older.
func (r *syncRun) rank(in func(holdKey) bool) ([]task, error) {
	var tasks []task
	for k, holds := range r.holds {
		if !in(k) {
			continue
		}
		for _, h := range holds {
			if h.released || h.wanted || h.fresh {
				continue
			}
			class, ok, err := r.urgency(k, h)
			if err != nil {
				return nil, err
			}
			if ok {
				tasks = append(tasks, task{key: k, hold: h, class: class, left: requestsLeft(h)})
			}
		}
		if r.live && !r.p.Close && r.rollAheadDue(k) {
			tasks = append(tasks, task{key: k, class: 0})
		}
	}
	slices.SortFunc(tasks, func(a, b task) int {
		if c := cmp.Compare(a.class, b.class); c != 0 {
			return c
		}
		if a.class == 1 {
			if c := cmp.Compare(a.left, b.left); c != 0 {
				return c
			}
		}
		return cmp.Or(compareTasks(a, b), compareUUID(a.key.customer, b.key.customer), strings.Compare(a.key.limitSet, b.key.limitSet))
	})
	return tasks, nil
}

// compareTasks orders holds oldest first, a roll-ahead after its key's holds.
func compareTasks(a, b task) int {
	switch {
	case a.hold == nil && b.hold == nil:
		return 0
	case a.hold == nil:
		return 1
	case b.hold == nil:
		return -1
	}
	return cmp.Or(a.hold.row.CreatedAt.Compare(b.hold.row.CreatedAt), compareUUID(a.hold.row.ID, b.hold.row.ID))
}

// urgency is a hold's task class; ok is false when the sync has nothing to
// do for it, as for a hold answered empty that waits for its gate to apply
// that.
func (r *syncRun) urgency(k holdKey, h *hold) (int, bool, error) {
	if r.p.Close || h.pending() {
		return 0, true, nil
	}
	ended, contended, used, shrinks, empty := false, false, false, false, true
	for _, p := range h.parts {
		ended = ended || (p.end != nil && !r.now.Before(*p.end))
		contended = contended || (r.contended(p) && p.answered > max(p.committed, p.used))
		used = used || p.used > 0
		shrinks = shrinks || p.answered > satAdd(p.committed, p.request)
		empty = empty && p.answered == 0 && p.used == 0
	}
	switch {
	case empty:
		return 0, false, nil
	case ended || contended || h.row.CreatedAt.Before(r.now.Add(-urgentAge)):
		return 0, true, nil
	case used:
		return 1, true, nil
	case h.row.CreatedAt.Before(r.now.Add(-reissueAge)):
		return 2, true, nil
	}
	changed, err := r.cardsChanged(h)
	switch {
	case err != nil:
		return 0, false, err
	case changed:
		return 2, true, nil
	case r.touched[k.customer] || r.moved[k.customer]:
		return 3, true, nil
	case shrinks:
		return 4, true, nil
	}
	return 0, false, nil
}

// requestsLeft is how many more requests the hold's parts cover beyond its
// tickets, the fewest of any part that prices one.
func requestsLeft(h *hold) int64 {
	left := int64(^uint64(0) >> 1)
	for _, p := range h.parts {
		if p.request > 0 {
			left = min(left, max(p.answered-p.committed, 0)/p.request)
		}
	}
	return left
}

// contended reports a part another lease was refused capacity_held for
// since the lease's previous sync.
func (r *syncRun) contended(p *part) bool {
	return p.contended != nil && p.contended.After(r.prevSync.Add(-limits.SyncInterval))
}

// cardsChanged reports a hold whose event type's rate cards are no longer
// the ones it pinned: it is issued again with the cards in force.
func (r *syncRun) cardsChanged(h *hold) (bool, error) {
	if h.terms.EventType == "" {
		return false, nil
	}
	current, err := r.cardsFor(h.terms.EventType, h.row.Currency)
	if err != nil {
		return false, err
	}
	return !sameCards(current, h.row.RateCards), nil
}

// sameCards reports whether two pinned card lists say the same: a hold's
// were stored as jsonb, which keeps neither key order nor spacing.
func sameCards(a, b []byte) bool {
	decode := func(raw []byte) (any, bool) {
		if len(bytes.TrimSpace(raw)) == 0 {
			return nil, true
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var v any
		if err := dec.Decode(&v); err != nil {
			return nil, false
		}
		return v, true
	}
	x, okA := decode(a)
	y, okB := decode(b)
	return okA && okB && reflect.DeepEqual(x, y)
}

func (r *syncRun) cardsFor(eventType, currency string) ([]byte, error) {
	if r.cards == nil {
		r.cards = map[[2]string][]byte{}
	}
	k := [2]string{eventType, currency}
	if c, ok := r.cards[k]; ok {
		return c, nil
	}
	c, err := r.money.PinnedCards(r.ctx, r.q, eventType, currency)
	if err != nil {
		return nil, err
	}
	r.cards[k] = c
	return c, nil
}

// loadHolds reads the lease's open holds and gives each part what the gate
// reports its open tickets took.
func (r *syncRun) loadHolds() error {
	since := limits.HeldSince(r.now)
	rows, err := r.q.ListLeaseHolds(r.ctx, gen.ListLeaseHoldsParams{MerchantID: r.merchant, LeaseID: r.lease.ID, HeldSince: since})
	if err != nil {
		return err
	}
	parts, err := r.q.ListLeaseHoldParts(r.ctx, gen.ListLeaseHoldPartsParams{MerchantID: r.merchant, LeaseID: r.lease.ID, HeldSince: since})
	if err != nil {
		return err
	}
	byID := map[uuid.UUID]*hold{}
	r.holds = map[holdKey][]*hold{}
	for _, row := range rows {
		h := &hold{row: row, version: deref(row.Version), applied: deref(row.AppliedVersion)}
		if len(row.Terms) > 0 {
			if err := json.Unmarshal(row.Terms, &h.terms); err != nil {
				return fmt.Errorf("decode hold terms: %w", err)
			}
		}
		byID[row.ID] = h
		k := holdKey{row.CustomerID, deref(row.LimitSet)}
		r.holds[k] = append(r.holds[k], h)
	}
	for _, p := range parts {
		h := byID[p.HoldID]
		if h == nil {
			continue
		}
		answered := p.Held
		if p.AnsweredUnits != nil {
			answered = *p.AnsweredUnits
		}
		h.parts = append(h.parts, &part{key: p.LimitKey, unit: p.Unit, start: p.WindowStartsAt.UTC(), end: p.WindowEndsAt, held: p.Held,
			answered: answered, request: deref(p.RequestUnits), used: deref(p.UsedUnits), contended: p.ContendedAt, overrun: r.overrunOf(p.LimitKey)})
	}
	r.commit()
	return nil
}

func (r *syncRun) overrunOf(key string) catalog.Overrun {
	if d, ok := r.def(key); ok {
		return d.Overrun
	}
	return catalog.OverrunRequireEstimate
}

// commit gives each part what the gate reports its open tickets took of it:
// the newest hold's part of that window, else the part of the hold that
// covers now. A hold the sync does not list has none.
func (r *syncRun) commit() {
	for _, c := range r.p.Committed {
		if c.CustomerID.IsZero() || (r.Allows != nil && !r.Allows(c.CustomerID)) {
			continue
		}
		k := holdKey{c.CustomerID.UUID(), c.LimitSet}
		holds := r.holds[k]
		for _, l := range c.Limits {
			var target *part
			for i := len(holds) - 1; i >= 0 && target == nil; i-- {
				if p := holds[i].part(l.Limit); p != nil && sameWindow(p, l.WindowStartsAt) {
					target = p
				}
			}
			if target == nil {
				if h := r.current(k, r.now); h != nil {
					target = h.part(l.Limit)
				}
			}
			if target != nil {
				target.committed = satAdd(target.committed, l.Committed)
			}
		}
	}
}

// sameWindow reports whether a commitment's window is the part's: null for
// a part with no end (balance, lifetime, rate).
func sameWindow(p *part, start *time.Time) bool {
	if start == nil || p.end == nil {
		return start == nil && p.end == nil
	}
	return p.start.Equal(start.UTC())
}

// current is the lease's hold for a customer and limit set whose windows
// cover at, the newest of them, else its newest.
func (r *syncRun) current(k holdKey, at time.Time) *hold {
	var best *hold
	for _, h := range r.holds[k] {
		if h.released {
			continue
		}
		if best == nil {
			best = h
			continue
		}
		in, bestIn := h.contains(at), best.contains(at)
		if (in && !bestIn) || (in == bestIn && h.row.CreatedAt.After(best.row.CreatedAt)) {
			best = h
		}
	}
	return best
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
			over, err := r.pastBudget(settleBudget)
			if err != nil {
				return 0, nil, err
			}
			if over {
				break
			}
		}
		stop := r.chunkEnd(end)
		var items []money.UsageItem
		var index []int
		for i := end; i < stop; i++ {
			item, err := r.usageItem(r.p.Usage[i])
			if err != nil {
				refuseNew(i, prefixParam(err, fmt.Sprintf("usage[%d]", i)))
				continue
			}
			items = append(items, item)
			index = append(index, i)
		}
		if len(items) > 0 {
			batch, err := r.money.PriceUsageTx(r.ctx, r.q, items)
			if err != nil {
				return 0, nil, err
			}
			if err := r.money.SettleUsageTx(r.ctx, r.q, batch); err != nil {
				return 0, nil, err
			}
			for j, res := range batch.Results {
				i := index[j]
				if res.Err != nil {
					if _, coded := asCoded(res.Err); !coded {
						return 0, nil, res.Err
					}
					refuseNew(i, prefixParam(res.Err, fmt.Sprintf("usage[%d]", i)))
					continue
				}
				r.touched[r.p.Usage[i].CustomerID.UUID()] = true
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

// usageItem is a lease's usage report as settlement takes it. Usage decided
// against a hold of its event type is priced by the cards the hold pinned,
// which its gate decided with; any other is priced by the cards in force.
func (r *syncRun) usageItem(u billing.LeaseUsage) (money.UsageItem, error) {
	switch {
	case u.CustomerID.IsZero():
		return money.UsageItem{}, invalid("customer_id", "customer_id required")
	case r.Allows != nil && !r.Allows(u.CustomerID):
		return money.UsageItem{}, apperr.New(http.StatusForbidden, billing.CodeServiceCredentialCustomerScopeDenied, "the credential's customer scope does not include the customer").WithParam("customer_id")
	case len(u.SourceID) > 255 || len(u.Source) > 255:
		return money.UsageItem{}, invalid("source_id", "source and source_id must be at most 255 bytes")
	case u.Outcome != "" && u.Outcome != billing.UsageSucceeded && u.Outcome != billing.UsageFailed:
		return money.UsageItem{}, invalid("outcome", `outcome must be "succeeded" or "failed"`)
	case u.InvokerType != "" && u.InvokerType != billing.InvokerTypeCustomer && u.InvokerType != billing.InvokerTypeDelegated:
		return money.UsageItem{}, invalid("invoker_type", `invoker_type must be "customer" or "delegated"`)
	}
	item := money.UsageItem{Customer: identity.CustomerID(u.CustomerID), Invoker: u.Invoker, Delegated: u.InvokerType == billing.InvokerTypeDelegated,
		Currency: u.Currency, EventType: u.EventType, Dimensions: u.Dimensions, Metadata: u.Metadata, Amount: u.Amount,
		Failed: u.Outcome == billing.UsageFailed, OccurredAt: u.OccurredAt}
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
	if u.LimitSet != nil {
		at := u.OccurredAt
		if at.IsZero() {
			at = r.now
		}
		if h := r.current(holdKey{u.CustomerID.UUID(), *u.LimitSet}, at); h != nil && h.row.Currency == money.NormalizeCurrency(u.Currency) {
			sh := &money.SettleHold{ID: h.row.ID, CreatedAt: h.row.CreatedAt, Consume: r.live}
			for _, p := range h.parts {
				sh.Parts = append(sh.Parts, money.HoldPart{Key: p.key, Unit: p.unit, WindowStartsAt: p.start, WindowEndsAt: p.end, Held: p.held})
			}
			item.Hold = sh
			if h.terms.EventType == strings.TrimSpace(u.EventType) {
				item.Cards = h.row.RateCards
			}
		}
	}
	return item, nil
}

// confirm records the versions the gate reports applied and releases what
// each confirmed hold sets aside beyond its answer and this sync's
// commitment. A hold left with nothing is released.
func (r *syncRun) confirm() error {
	for _, a := range r.p.Applied {
		if a.CustomerID.IsZero() || !r.locked[a.CustomerID.UUID()] {
			continue
		}
		for _, h := range r.holds[holdKey{a.CustomerID.UUID(), a.LimitSet}] {
			if v := min(a.Version, h.version); v > h.applied {
				h.applied, h.versionDirty = v, true
			}
		}
	}
	for k, holds := range r.holds {
		if !r.locked[k.customer] {
			continue
		}
		for _, h := range holds {
			if h.released || h.applied < h.version {
				continue
			}
			empty := true
			for _, p := range h.parts {
				if keep := max(p.answered, p.committed); p.held > keep {
					p.held = keep
					if err := r.write(h, p); err != nil {
						return err
					}
				}
				empty = empty && p.held == 0 && p.answered == 0 && p.used == 0
			}
			if empty {
				if err := r.release(h); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// giveBack releases the holds the sync returns.
func (r *syncRun) giveBack() error {
	for _, ret := range r.p.Returns {
		if ret.CustomerID.IsZero() || (r.Allows != nil && !r.Allows(ret.CustomerID)) {
			continue
		}
		for _, h := range r.holds[holdKey{ret.CustomerID.UUID(), ret.LimitSet}] {
			if h.released {
				continue
			}
			if err := r.release(h); err != nil {
				return err
			}
		}
	}
	return nil
}

// releaseEvery returns the closing lease's open holds of the customers the
// sync locked; each is answered with nothing held.
func (r *syncRun) releaseEvery() error {
	for k, holds := range r.holds {
		if !r.locked[k.customer] {
			continue
		}
		for _, h := range holds {
			if h.released {
				continue
			}
			if err := r.release(h); err != nil {
				return err
			}
			h.changed = true
		}
	}
	return nil
}

func (r *syncRun) release(h *hold) error {
	if err := r.q.ReleaseLeaseHold(r.ctx, gen.ReleaseLeaseHoldParams{AsOf: r.now, MerchantID: r.merchant, ID: h.row.ID, CreatedAt: h.row.CreatedAt}); err != nil {
		return err
	}
	h.released = true
	return nil
}

// nextVersion is the version this sync's answers carry.
func (r *syncRun) nextVersion() int64 {
	if r.version == 0 {
		r.version = r.lease.HoldVersion + 1
	}
	return r.version
}

// writeVersions gives every hold whose answer changed the sync's version
// and stores the versions that moved.
func (r *syncRun) writeVersions() error {
	for _, holds := range r.holds {
		for _, h := range holds {
			if h.changed && !h.fresh {
				h.version, h.versionDirty = r.nextVersion(), true
			}
			if !h.versionDirty || h.released || h.fresh {
				continue
			}
			if err := r.q.SetLeaseHoldVersions(r.ctx, gen.SetLeaseHoldVersionsParams{Version: h.version, AppliedVersion: min(h.applied, h.version), AsOf: r.now,
				MerchantID: r.merchant, ID: h.row.ID, CreatedAt: h.row.CreatedAt}); err != nil {
				return err
			}
		}
	}
	return nil
}

// Details is an item or want refusal on the wire.
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
	u.Currency = money.NormalizeCurrency(u.Currency)
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

// LimitSet is the opaque key of a set of limits in one currency, for one
// event type ("" for host-stated requests) and the resolved policy's money
// terms.
func LimitSet(currency, eventType, moneyTerms string, keys []string) string {
	sorted := slices.Clone(keys)
	slices.Sort(sorted)
	sum := sha256.Sum256([]byte(currency + "\x00" + eventType + "\x00" + moneyTerms + "\x00" + strings.Join(sorted, "\x00")))
	return "ls_" + hex.EncodeToString(sum[:16])
}

func compareUUID(a, b uuid.UUID) int { return strings.Compare(string(a[:]), string(b[:])) }

func deref[T any](v *T) T {
	var zero T
	if v == nil {
		return zero
	}
	return *v
}
