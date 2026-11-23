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
	// rolloverAhead is how soon before a fixed window closes an active hold
	// gets its next window's hold.
	rolloverAhead = 2 * limits.SyncInterval
	// leaseIDSkew is how far a lease id's time may run ahead of the server
	// clock: an id with no lease older than the replay horizon less it names
	// a lease that may already be forgotten.
	leaseIDSkew = retention.Day
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
		q := gen.New(tx)
		// A sync's transaction is bounded: readers count a lease live for
		// this much longer than its lifetime.
		if _, err := q.SetConfig(ctx, gen.SetConfigParams{Setting: "transaction_timeout", Value: strconv.Itoa(int(limits.SyncTimeout / time.Millisecond)), IsLocal: true}); err != nil {
			return err
		}
		now := s.Now().UTC().Truncate(time.Microsecond)
		d := s.DB.NewWithPgxTx(tx)
		run := &syncRun{Syncer: s, ctx: ctx, q: q, d: d, merchant: mid.UUID(), now: now, p: p,
			money: s.Money.WithDatabase(d), admitter: s.Admitter.Bind(d, func() time.Time { return now }), failed: func(u billing.LeaseUsage) []money.FailedUsageWindow {
				return failed[windowKey{u.CustomerID, cmp.Or(u.InvokerType, billing.InvokerTypeCustomer), money.NormalizeCurrency(u.Currency)}]
			}}
		var err error
		out, err = run.sync()
		return err
	})
	return out, err
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

// stale reports a hold that must be issued again: a window of it closed, or
// its row grew old.
func (h *hold) stale(now time.Time) bool {
	if h.row.CreatedAt.Before(now.Add(-reissueAge)) {
		return true
	}
	for _, p := range h.parts {
		if p.end != nil && !now.Before(*p.end) {
			return true
		}
	}
	return false
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
	if err := r.lockCustomers(); err != nil {
		return nil, err
	}
	r.touched, r.answered = map[uuid.UUID]bool{}, map[holdKey]int{}
	r.result = &billing.SyncLeasesResult{Wants: make([]billing.LeaseWantResult, len(r.p.Wants)), Holds: []billing.Hold{}, Limits: []billing.CustomerLimit{},
		Usage: billing.LeaseUsageResult{Refused: []billing.LeaseUsageRefusal{}}}
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
					if p.used > 0 && r.locked[h.row.CustomerID] {
						h.changed, h.active = true, true
					}
				}
			}
		}
		if err := r.confirm(); err != nil {
			return nil, err
		}
	}
	if r.p.Close {
		// A closing sync, its usage settled, returns every hold of the lease.
		if err := r.releaseEvery(); err != nil {
			return nil, err
		}
	}
	if err := r.giveBack(); err != nil {
		return nil, err
	}
	for i := range r.p.Wants {
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

// lockCustomers locks, in id order, every customer the sync names and every
// customer whose holds it may change: it writes a lease's parts only under
// their customer's lock, as every other writer of them does.
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
	for _, u := range r.p.Usage {
		add(u.CustomerID)
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
		for k, holds := range r.holds {
			for _, h := range holds {
				needs, err := r.needsWork(k, h)
				if err != nil {
					return err
				}
				if needs {
					add(billing.CustomerID(k.customer))
				}
			}
		}
	}
	slices.SortFunc(ids, compareUUID)
	for _, id := range ids {
		if err := db.EnsureCustomerRowQ(r.ctx, r.q, r.merchant, id); err != nil {
			return err
		}
		if _, err := r.q.LockCustomerForSpend(r.ctx, gen.LockCustomerForSpendParams{MerchantID: r.merchant, ID: id}); err != nil {
			return err
		}
	}
	return nil
}

// needsWork reports a hold this sync may change: a closing sync's, or a live
// lease's hold that is stale, whose customer moved, that another lease waits
// for, that its gate confirmed above what it needs, that usage took from or
// that an idle sync shrinks.
func (r *syncRun) needsWork(k holdKey, h *hold) (bool, error) {
	if r.p.Close {
		return true, nil
	}
	if h.stale(r.now) || r.moved[k.customer] || h.pending() {
		return true, nil
	}
	for _, p := range h.parts {
		if p.used > 0 || r.contended(p) || p.answered > satAdd(p.committed, p.request) {
			return true, nil
		}
	}
	return r.cardsChanged(h)
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

// settle settles the sync's usage in order and answers what it resent.
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
	first := 0
	for first < len(r.p.Usage) && r.p.Usage[first].N <= through {
		first++
	}
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
	// New items settle in order; a gap stops them.
	var items []money.UsageItem
	var index []int
	var refusals []billing.LeaseUsageRefusal
	refuseNew := func(i int, err error) {
		status, details := Details(err)
		refusals = append(refusals, billing.LeaseUsageRefusal{N: r.p.Usage[i].N, Status: status, Error: &details})
	}
	last := through
	for i := first; i < len(r.p.Usage); i++ {
		u := r.p.Usage[i]
		if u.N != last+1 {
			for j := i; j < len(r.p.Usage); j++ {
				refuse(r.p.Usage[j].N, apperr.New(http.StatusConflict, billing.CodeUsageOutOfOrder,
					fmt.Sprintf("usage item %d skips %d, which the lease has not settled", r.p.Usage[j].N, last+1)).WithParam(fmt.Sprintf("usage[%d].n", j)))
			}
			break
		}
		last = u.N
		item, err := r.usageItem(u)
		if err != nil {
			refuseNew(i, prefixParam(err, fmt.Sprintf("usage[%d]", i)))
			continue
		}
		items = append(items, item)
		index = append(index, i)
	}
	if last == through {
		slices.SortFunc(r.result.Usage.Refused, func(a, b billing.LeaseUsageRefusal) int { return cmp.Compare(a.N, b.N) })
		return through, receipts, nil
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
	var newDigests []string
	for i := first; i < len(r.p.Usage) && r.p.Usage[i].N <= last; i++ {
		newDigests = append(newDigests, digests[i])
	}
	rc.Digest = settlementDigest(newDigests)
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

// releaseEvery returns every open hold of the lease; each is answered with
// nothing held.
func (r *syncRun) releaseEvery() error {
	for _, holds := range r.holds {
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
