package openrails

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/modules/limits"
)

type holdKey struct {
	customer billing.CustomerID
	set      string
}

// localGate is a gate in process memory: one lease, one hold per customer
// and limit set, one mutex per hold.
type localGate struct {
	c        *Client
	outbox   UsageOutbox
	maxHolds int

	// index guards holds, expired and sets. Lock order: mu, index, a hold.
	index sync.RWMutex
	holds map[holdKey]*localHold
	// expired are the holds of a lease the gate failed to renew in time:
	// only their safe amounts are spent, until the next lease's first sync.
	expired map[holdKey]*localHold
	sets    map[string]string

	// liveUntil is when the lease dies unless a sync renews it, by the local
	// clock, measured from the renewing sync's send.
	liveUntil atomic.Int64

	mu        sync.Mutex
	lease     billing.LeaseID
	queues    map[billing.LeaseID]*usageQueue
	wants     map[string]*pendingWant
	returns   map[holdKey]*localHold
	display   map[billing.CustomerID][]billing.CustomerLimit
	refused   []error
	failure   error
	failedAt  time.Time
	attempts  uint64
	synced    chan struct{}
	closing   bool
	stopped   bool
	recovered bool

	kick    chan struct{}
	stop    chan struct{}
	done    chan struct{}
	once    sync.Once
	started atomic.Bool
}

func newLocalGate(c *Client, outbox UsageOutbox, start bool) *localGate {
	g := &localGate{
		c: c, outbox: outbox, maxHolds: maxGateHolds,
		holds: map[holdKey]*localHold{}, expired: map[holdKey]*localHold{}, sets: map[string]string{},
		lease: billing.LeaseID(uuid.New()), queues: map[billing.LeaseID]*usageQueue{},
		wants: map[string]*pendingWant{}, returns: map[holdKey]*localHold{}, display: map[billing.CustomerID][]billing.CustomerLimit{},
		synced: make(chan struct{}), kick: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{}),
		recovered: outbox == nil,
	}
	if start {
		g.ensureStarted()
	}
	return g
}

func (g *localGate) ensureStarted() {
	g.once.Do(func() {
		g.started.Store(true)
		go g.run()
	})
}

func (g *localGate) poke() {
	select {
	case g.kick <- struct{}{}:
	default:
	}
}

func (g *localGate) leaseLive(now time.Time) bool {
	until := g.liveUntil.Load()
	return until != 0 && now.UnixNano() < until
}

// localHold is one hold of the gate's lease. Its mutex serializes the
// decisions on it; every decision takes from all of its limits or none.
type localHold struct {
	key   holdKey
	lease billing.LeaseID
	used  atomic.Int64

	mu        sync.Mutex
	pricing   *holdPricing
	parts     map[string]*partState
	events    map[string]bool
	safe      *int64
	safeSpent int64
	open      int
	pending   int
	gone      bool
	// issued: the server holds it (an empty hold for a limit set that holds
	// nothing is the gate's own and is never returned).
	issued bool
}

// partState is a part's held units, as the server last answered, what open
// tickets took of them (the hold's commitment) and what captured usage not
// yet settled spent.
type partState struct {
	limit              string
	start              *time.Time
	held, open, unsent int64
}

func (p *partState) avail() int64 { return p.held - p.open - p.unsent }

func newLocalHold(key holdKey, lease billing.LeaseID, currency string) *localHold {
	h := &localHold{key: key, lease: lease, parts: map[string]*partState{}, events: map[string]bool{}}
	h.pricing = newHoldPricing(billing.Hold{CustomerID: key.customer, LimitSet: key.set, Currency: currency}, h.events)
	return h
}

type decision uint8

const (
	decideAllowed decision = iota
	decideShort
	decideCold
	decideUnpriced
)

// decide takes r's cost from every limit r counts toward, or from none. A
// limit admitting only whole estimates needs the whole cost available; one
// admitting while positive needs any, and takes what is left. Without a live
// lease, or short of capacity, only the hold's safe amount (when withSafe)
// is spent, taking the full cost. low reports a hold the next such request
// would not fit.
func (h *localHold) decide(now time.Time, r request, live, withSafe bool) (takes []take, low bool, out decision, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.gone || h.pricing == nil {
		return nil, false, decideCold, nil
	}
	takes, money, err := h.pricing.cost(now, r, withSafe && h.safe != nil)
	if errors.Is(err, errUnpriced) {
		return nil, false, decideUnpriced, nil
	}
	if err != nil {
		return nil, false, decideCold, err
	}
	full := slices.Clone(takes)
	short := !live
	for i := 0; live && i < len(takes); i++ {
		t := takes[i]
		p := h.parts[t.part]
		if p == nil {
			short = true
			break
		}
		avail := p.avail()
		if t.overrun == catalog.OverrunAdmitWhilePositive {
			if avail <= 0 {
				short = true
			} else if avail < t.cost {
				takes[i].cost = avail
			}
		} else if avail < t.cost {
			short = true
		}
		low = low || avail-t.cost < t.cost
	}
	if short {
		if !withSafe || h.safe == nil || *h.safe-h.safeSpent < money {
			return nil, false, decideShort, nil
		}
		takes, low = full, true
		h.safeSpent += money
	}
	for _, t := range takes {
		h.part(t.part).open += t.cost
	}
	h.open++
	h.used.Store(now.UnixNano())
	return takes, low, decideAllowed, nil
}

func (h *localHold) part(id string) *partState {
	p := h.parts[id]
	if p == nil {
		p = &partState{limit: partLimit(id), start: partStart(id)}
		h.parts[id] = p
	}
	return p
}

func (h *localHold) release(takes []take) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, t := range takes {
		h.part(t.part).open -= t.cost
	}
	h.open--
}

// captured turns an admission's take into its usage's cost until it settles.
func (h *localHold) captured(taken, actual []take) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := range taken {
		p := h.part(taken[i].part)
		p.open -= taken[i].cost
		p.unsent += actual[i].cost
	}
	h.open--
	h.pending++
}

// actual is what usage r counts in the parts taken; the taken costs where
// the hold cannot price it.
func (h *localHold) actual(r request, taken []take) ([]take, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	out, err := h.pricing.costOf(r, taken)
	if errors.Is(err, errUnpriced) {
		return slices.Clone(taken), nil
	}
	return out, err
}

// answer applies a sync's hold to it, after settled usage left it.
func (h *localHold) answer(b billing.Hold) {
	h.pricing = newHoldPricing(b, h.events)
	seen := map[string]bool{}
	for _, l := range b.Limits {
		id := partID(l.Limit, l.WindowStartsAt)
		seen[id] = true
		p := h.part(id)
		p.limit, p.start, p.held = l.Limit, l.WindowStartsAt, l.Held
	}
	for id, p := range h.parts {
		if !seen[id] {
			p.held = 0
			if p.open == 0 && p.unsent == 0 {
				delete(h.parts, id)
			}
		}
	}
	h.safe, h.safeSpent, h.issued = b.Safe, 0, true
}

// settled removes settled usage's cost; a hold the sync did not answer also
// loses it from held, which the server consumed.
func (h *localHold) settled(takes []take, answered bool) {
	for _, t := range takes {
		p := h.part(t.part)
		p.unsent -= t.cost
		if !answered {
			p.held -= t.cost
		}
	}
	h.pending--
}

type localTicket struct {
	g     *localGate
	hold  *localHold
	takes []take
}

// notCaptured is a capture that left its ticket open.
type notCaptured struct{ error }

func (e notCaptured) Unwrap() error { return e.error }

func (lt *localTicket) capture(ctx context.Context, t *Ticket, u billing.CaptureUsage) error {
	item := captureItem(t.params, &t.limitSet, u, time.Now())
	r := request{usage: &limits.Usage{EventType: item.EventType, Dimensions: item.Dimensions, Metadata: item.Metadata}, stated: item.Amount}
	actual, err := lt.hold.actual(r, lt.takes)
	if err != nil {
		return notCaptured{err}
	}
	return lt.g.enqueue(ctx, lt.hold.lease, item, lt.hold, lt.takes, actual)
}

func (lt *localTicket) release() { lt.hold.release(lt.takes) }

// usageQueue is a lease's unsettled usage, in order.
type usageQueue struct {
	next  int64
	items []*queued
}

type queued struct {
	item    billing.LeaseUsage
	hold    *localHold
	takes   []take
	durable bool
}

func (g *localGate) queueLocked(lease billing.LeaseID) *usageQueue {
	q := g.queues[lease]
	if q == nil {
		q = &usageQueue{}
		g.queues[lease] = q
	}
	return q
}

// enqueue numbers an item in its lease and queues it for the next sync. With
// an outbox it is stored before enqueue returns; later items wait for it.
func (g *localGate) enqueue(ctx context.Context, lease billing.LeaseID, item billing.LeaseUsage, h *localHold, taken, actual []take) error {
	g.mu.Lock()
	if g.stopped {
		g.mu.Unlock()
		return notCaptured{errGateClosed}
	}
	if lease.IsZero() {
		lease = g.lease
	}
	q := g.queueLocked(lease)
	q.next++
	item.N = q.next
	e := &queued{item: item, hold: h, takes: actual, durable: g.outbox == nil}
	q.items = append(q.items, e)
	if h != nil {
		h.captured(taken, actual)
	}
	g.mu.Unlock()
	if g.outbox == nil {
		return nil
	}
	err := g.outbox.Put(ctx, []OutboxItem{{LeaseID: lease, Usage: item}})
	g.mu.Lock()
	e.durable = true
	g.mu.Unlock()
	if err != nil {
		return fmt.Errorf("openrails: gate: usage %d is queued in memory, not in the outbox: %w", item.N, err)
	}
	return nil
}

func (g *localGate) record(ctx context.Context, item billing.LeaseUsage) error {
	g.ensureStarted()
	err := g.enqueue(ctx, billing.LeaseID{}, item, nil, nil, nil)
	var nc notCaptured
	if errors.As(err, &nc) {
		return nc.error
	}
	return err
}

// pendingWant is one want of the next sync and the requests waiting for it.
type pendingWant struct {
	attrs     string
	eventType string
	want      billing.LeaseWant
	done      chan struct{}
	answer    wantAnswer
}

// wantAnswer is a sync's answer to a want: a limit set (its hold applied),
// a refusal, an error, OpenRails unreachable, or retry with a new sync.
type wantAnswer struct {
	limitSet    string
	refusal     *billing.Refusal
	err         error
	unavailable error
	retry       bool
}

func eventTypeOf(p billing.AdmitParams) string {
	if p.Usage == nil {
		return ""
	}
	return p.Usage.EventType
}

func (g *localGate) holdFor(attrs string, customer billing.CustomerID) (*localHold, string) {
	g.index.RLock()
	defer g.index.RUnlock()
	set, ok := g.sets[attrs]
	if !ok {
		return nil, ""
	}
	if h := g.holds[holdKey{customer, set}]; h != nil {
		return h, set
	}
	return g.expired[holdKey{customer, set}], set
}

func (g *localGate) admit(ctx context.Context, gate *Gate, p billing.AdmitParams) (*Ticket, error) {
	g.ensureStarted()
	r := admitRequest(p)
	attrs := attrsKey(p)
	answered := false
	for round := 0; ; round++ {
		now := time.Now()
		if h, set := g.holdFor(attrs, p.CustomerID); h != nil {
			takes, low, out, err := h.decide(now, r, g.leaseLive(now), false)
			switch {
			case err != nil:
				return nil, err
			case out == decideAllowed:
				if low {
					g.prefetch(attrs, p)
				}
				return &Ticket{gate: gate, params: p, limitSet: set, local: &localTicket{g: g, hold: h, takes: takes}}, nil
			case out == decideUnpriced && answered:
				return nil, errNotPriced(eventTypeOf(p))
			}
		}
		if round >= maxColdRounds {
			return refusedTicket(gate, p, capacityHeld(1)), nil
		}
		if failure := g.recentFailure(now); failure != nil {
			return g.safe(gate, p, r, attrs, failure)
		}
		a, err := g.await(ctx, attrs, p)
		if err != nil {
			return nil, err
		}
		switch {
		case a.retry:
		case a.unavailable != nil:
			return g.safe(gate, p, r, attrs, a.unavailable)
		case a.err != nil:
			return nil, a.err
		case a.refusal != nil && a.refusal.DenyCode == billing.DenyCapacityHeld:
			if !waitRetry(ctx, a.refusal) {
				return refusedTicket(gate, p, a.refusal), nil
			}
		case a.refusal != nil:
			return refusedTicket(gate, p, a.refusal), nil
		default:
			answered = true
		}
	}
}

// safe decides a request while OpenRails is unreachable: from the hold
// while its lease lives, else from its safe amount, else ErrUnavailable.
func (g *localGate) safe(gate *Gate, p billing.AdmitParams, r request, attrs string, failure error) (*Ticket, error) {
	now := time.Now()
	h, set := g.holdFor(attrs, p.CustomerID)
	if h == nil {
		return nil, unavailable(failure)
	}
	takes, _, out, err := h.decide(now, r, g.leaseLive(now), true)
	if err != nil {
		return nil, err
	}
	if out != decideAllowed {
		return nil, unavailable(failure)
	}
	return &Ticket{gate: gate, params: p, limitSet: set, local: &localTicket{g: g, hold: h, takes: takes}}, nil
}

// recentFailure is the last sync's failure while it is fresh: a cold
// request then answers at once instead of waiting for another attempt.
func (g *localGate) recentFailure(now time.Time) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.failure != nil && now.Sub(g.failedAt) < limits.SyncInterval {
		return g.failure
	}
	return nil
}

// await adds the request's want to the next sync and waits for its answer.
// Wants of one invoker-attribute key share one want.
func (g *localGate) await(ctx context.Context, attrs string, p billing.AdmitParams) (wantAnswer, error) {
	g.mu.Lock()
	if g.closing {
		g.mu.Unlock()
		return wantAnswer{}, errGateClosed
	}
	e := g.wantLocked(attrs, p)
	g.mu.Unlock()
	g.poke()
	select {
	case <-ctx.Done():
		return wantAnswer{}, ctx.Err()
	case <-e.done:
		return e.answer, nil
	}
}

func (g *localGate) wantLocked(attrs string, p billing.AdmitParams) *pendingWant {
	e := g.wants[attrs]
	if e == nil {
		e = &pendingWant{attrs: attrs, done: make(chan struct{})}
		g.wants[attrs] = e
	}
	e.want, e.eventType = leaseWant(p), eventTypeOf(p)
	return e
}

// prefetch asks the next sync to top up a hold running low.
func (g *localGate) prefetch(attrs string, p billing.AdmitParams) {
	g.mu.Lock()
	if !g.closing && g.wants[attrs] == nil {
		g.wantLocked(attrs, p)
	}
	g.mu.Unlock()
	g.poke()
}

func (g *localGate) run() {
	defer close(g.done)
	ticker := time.NewTicker(limits.SyncInterval)
	defer ticker.Stop()
	for {
		g.recover()
		g.syncOnce()
		select {
		case <-g.stop:
			return
		case <-ticker.C:
		case <-g.kick:
		}
	}
}

// recover queues the outbox's pending usage once, against its leases.
func (g *localGate) recover() {
	g.mu.Lock()
	done := g.recovered
	g.mu.Unlock()
	if done {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), syncTimeout)
	defer cancel()
	items, err := g.outbox.Pending(ctx)
	if err != nil {
		return
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].LeaseID != items[j].LeaseID {
			return items[i].LeaseID.String() < items[j].LeaseID.String()
		}
		return items[i].Usage.N < items[j].Usage.N
	})
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, it := range items {
		if it.LeaseID == g.lease || it.LeaseID.IsZero() {
			continue
		}
		q := g.queueLocked(it.LeaseID)
		if n := len(q.items); n > 0 && q.items[n-1].item.N >= it.Usage.N {
			continue
		}
		q.items = append(q.items, &queued{item: it.Usage, durable: true})
		q.next = it.Usage.N
	}
	g.recovered = true
}

// sendableLocked is a queue's head: up to limit items stored as needed.
func sendableLocked(q *usageQueue, limit int) []billing.LeaseUsage {
	if q == nil {
		return nil
	}
	var out []billing.LeaseUsage
	for _, e := range q.items {
		if !e.durable || len(out) == limit {
			break
		}
		out = append(out, e.item)
	}
	return out
}

// rolloverLocked starts a new lease. The old one's usage is still sent
// against it; its holds keep only their safe amounts when keepSafe.
func (g *localGate) rolloverLocked(keepSafe bool) {
	g.lease = billing.LeaseID(uuid.New())
	g.liveUntil.Store(0)
	g.index.Lock()
	for _, h := range g.holds {
		h.mu.Lock()
		h.gone = !keepSafe
		h.mu.Unlock()
	}
	if keepSafe {
		g.expired = g.holds
	} else {
		g.expired = map[holdKey]*localHold{}
	}
	g.holds = map[holdKey]*localHold{}
	g.index.Unlock()
	g.returns = map[holdKey]*localHold{}
}

// evictLocked returns the least recently used idle holds past the bound.
func (g *localGate) evictLocked() {
	g.index.Lock()
	defer g.index.Unlock()
	if over := len(g.holds) - g.maxHolds; over > 0 {
		type candidate struct {
			key  holdKey
			hold *localHold
			used int64
		}
		var idle []candidate
		for k, h := range g.holds {
			h.mu.Lock()
			if h.open == 0 && h.pending == 0 {
				idle = append(idle, candidate{k, h, h.used.Load()})
			}
			h.mu.Unlock()
		}
		sort.Slice(idle, func(i, j int) bool { return idle[i].used < idle[j].used })
		for _, c := range idle[:min(over, len(idle))] {
			delete(g.holds, c.key)
			c.hold.mu.Lock()
			c.hold.gone = true
			issued := c.hold.issued
			c.hold.mu.Unlock()
			if issued {
				g.returns[c.key] = c.hold
			}
		}
	}
	if len(g.sets) > maxGateLimitSets {
		g.sets = map[string]string{}
	}
	if len(g.display) > g.maxHolds {
		g.display = map[billing.CustomerID][]billing.CustomerLimit{}
	}
}

// syncOnce is one sync of the lease, then the usage of earlier leases.
func (g *localGate) syncOnce() {
	now := time.Now()
	g.mu.Lock()
	if g.liveUntil.Load() != 0 && !g.leaseLive(now) {
		g.rolloverLocked(true)
	}
	g.evictLocked()
	lease := g.lease
	usage := sendableLocked(g.queues[lease], billing.MaxSyncUsageItems)
	returns := make([]billing.HoldReturn, 0, len(g.returns))
	returned := map[holdKey]*localHold{}
	for k, h := range g.returns {
		if len(returns) == billing.MaxSyncReturns {
			break
		}
		returns = append(returns, billing.HoldReturn{CustomerID: k.customer, LimitSet: k.set})
		returned[k] = h
		delete(g.returns, k)
	}
	var entries []*pendingWant
	g.index.RLock()
	committed := commitments(g.holds)
	for attrs, e := range g.wants {
		if len(entries) == billing.MaxSyncWants {
			break
		}
		if set, ok := g.sets[attrs]; ok && returned[holdKey{e.want.CustomerID, set}] != nil {
			continue
		}
		entries = append(entries, e)
		delete(g.wants, attrs)
	}
	g.index.RUnlock()
	g.mu.Unlock()

	wants := make([]billing.LeaseWant, len(entries))
	for i, e := range entries {
		wants[i] = e.want
	}
	params := billing.SyncLeasesParams{LeaseID: lease, Usage: nonNilUsage(usage), Wants: wants, Returns: returns, Committed: committed}
	sent := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), syncTimeout)
	res, err := g.c.SyncLeases(ctx, params)
	cancel()
	acks := g.apply(lease, params, entries, returned, res, err, sent)
	g.ack(acks)
	g.resendEarlier()
	g.mu.Lock()
	more := len(g.wants) > 0 && err == nil || len(sendableLocked(g.queues[g.lease], 1)) > 0 && len(usage) == billing.MaxSyncUsageItems
	g.mu.Unlock()
	if more {
		g.poke()
	}
}

// commitments are what open tickets took of each hold, as the sync is built.
func commitments(holds map[holdKey]*localHold) []billing.HoldCommitment {
	out := []billing.HoldCommitment{}
	for k, h := range holds {
		h.mu.Lock()
		if h.open > 0 && h.issued {
			c := billing.HoldCommitment{CustomerID: k.customer, LimitSet: k.set}
			for _, p := range h.parts {
				if p.open > 0 {
					c.Limits = append(c.Limits, billing.LimitCommitment{Limit: p.limit, WindowStartsAt: p.start, Committed: p.open})
				}
			}
			if len(c.Limits) > 0 && len(out) < billing.MaxSyncCommitted {
				out = append(out, c)
			}
		}
		h.mu.Unlock()
	}
	return out
}

func nonNilUsage(u []billing.LeaseUsage) []billing.LeaseUsage {
	if u == nil {
		return []billing.LeaseUsage{}
	}
	return u
}

// apply takes in one sync's answer, or its failure, and answers its wants.
func (g *localGate) apply(lease billing.LeaseID, params billing.SyncLeasesParams, entries []*pendingWant, returned map[holdKey]*localHold, res *billing.SyncLeasesResult, err error, sent time.Time) []billing.LeaseUsageRef {
	g.mu.Lock()
	defer func() {
		g.attempts++
		close(g.synced)
		g.synced = make(chan struct{})
		g.mu.Unlock()
		for _, e := range entries {
			close(e.done)
		}
	}()
	if err == nil && len(res.Wants) != len(entries) {
		err = fmt.Errorf("%w: a sync answered %d wants for %d", billing.ErrUnreachable, len(res.Wants), len(entries))
	}
	if err != nil {
		return g.failedLocked(lease, params, entries, returned, err)
	}
	g.failure, g.failedAt = nil, time.Time{}
	if lease == g.lease {
		g.liveUntil.Store(sent.Add(limits.LeaseLifetime).UnixNano())
	}
	acks, settled := g.settleLocked(lease, res.Usage)

	g.index.Lock()
	if lease == g.lease {
		g.expired = map[holdKey]*localHold{}
	}
	for k, h := range returned {
		if g.holds[k] == h {
			delete(g.holds, k)
		}
	}
	answered := map[*localHold]bool{}
	for _, b := range res.Holds {
		if lease != g.lease {
			break
		}
		k := holdKey{b.CustomerID, b.LimitSet}
		h := g.holds[k]
		if h == nil {
			h = newLocalHold(k, lease, b.Currency)
			g.holds[k] = h
		}
		h.mu.Lock()
		for _, s := range settled[h] {
			h.settled(s, true)
		}
		h.answer(b)
		h.mu.Unlock()
		answered[h] = true
	}
	for h, list := range settled {
		if answered[h] {
			continue
		}
		h.mu.Lock()
		for _, s := range list {
			h.settled(s, false)
		}
		h.mu.Unlock()
	}
	for i, wr := range res.Wants {
		e := entries[i]
		e.answer = g.wantResultLocked(lease, e, wr)
	}
	g.index.Unlock()

	for _, line := range res.Limits {
		g.display[line.CustomerID] = nil
	}
	for _, line := range res.Limits {
		g.display[line.CustomerID] = append(g.display[line.CustomerID], line)
	}
	return acks
}

// wantResultLocked answers one want; mu and index are held.
func (g *localGate) wantResultLocked(lease billing.LeaseID, e *pendingWant, wr billing.LeaseWantResult) wantAnswer {
	switch {
	case wr.Error != nil:
		if wr.Error.Code == billing.CodeLeaseNotHeld {
			if lease == g.lease {
				g.index.Unlock()
				g.rolloverLocked(false)
				g.index.Lock()
			}
			return wantAnswer{retry: true}
		}
		return wantAnswer{err: &billing.StatusError{Status: wr.Status, ErrorDetails: *wr.Error}}
	case wr.LimitSet == nil:
		return wantAnswer{err: fmt.Errorf("%w: a want was answered with no limit set", billing.ErrUnreachable)}
	}
	g.sets[e.attrs] = *wr.LimitSet
	if wr.Refusal != nil {
		r := *wr.Refusal
		return wantAnswer{limitSet: *wr.LimitSet, refusal: &r}
	}
	k := holdKey{e.want.CustomerID, *wr.LimitSet}
	h := g.holds[k]
	if h == nil {
		h = newLocalHold(k, g.lease, e.want.Currency)
		g.holds[k] = h
	}
	if e.eventType != "" {
		h.mu.Lock()
		h.events[e.eventType] = true
		h.pricing.events[e.eventType] = true
		h.mu.Unlock()
	}
	return wantAnswer{limitSet: *wr.LimitSet}
}

// failedLocked handles a sync that failed: a dead lease rolls over, refused
// usage is dropped, and OpenRails unreachable answers its wants so.
func (g *localGate) failedLocked(lease billing.LeaseID, params billing.SyncLeasesParams, entries []*pendingWant, returned map[holdKey]*localHold, err error) []billing.LeaseUsageRef {
	var se *billing.StatusError
	status := errors.As(err, &se) && !errors.Is(err, billing.ErrUnreachable)
	switch {
	case status && se.Code == billing.CodeLeaseNotHeld:
		if lease == g.lease {
			g.rolloverLocked(false)
		}
		for _, e := range entries {
			e.answer = wantAnswer{retry: true}
		}
		return nil
	case status && len(params.Usage) > 0 && (se.Code == billing.CodeUsageOutOfOrder || se.Code == billing.CodeIdempotencyKeyReused || strings.HasPrefix(deref(se.Param), "usage")):
		var acks []billing.LeaseUsageRef
		if q := g.queues[lease]; q != nil {
			last := params.Usage[len(params.Usage)-1].N
			for len(q.items) > 0 && q.items[0].item.N <= last {
				e := q.items[0]
				q.items = q.items[1:]
				acks = append(acks, billing.LeaseUsageRef{LeaseID: lease, N: e.item.N})
				g.dropLocked(e)
			}
		}
		g.refusedLocked(fmt.Errorf("openrails: gate: usage %d..%d of lease %s refused: %w", params.Usage[0].N, params.Usage[len(params.Usage)-1].N, lease, err))
		for _, e := range entries {
			e.answer = wantAnswer{retry: true}
		}
		g.requeueReturnsLocked(returned)
		return acks
	case status:
		for _, e := range entries {
			e.answer = wantAnswer{err: err}
		}
	default:
		g.failure, g.failedAt = err, time.Now()
		for _, e := range entries {
			e.answer = wantAnswer{unavailable: err}
		}
	}
	g.requeueReturnsLocked(returned)
	return nil
}

func (g *localGate) requeueReturnsLocked(returned map[holdKey]*localHold) {
	for k, h := range returned {
		if g.returns[k] == nil {
			g.returns[k] = h
		}
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// dropLocked forgets a queued item that will never settle.
func (g *localGate) dropLocked(e *queued) {
	if e.hold == nil {
		return
	}
	e.hold.mu.Lock()
	for _, t := range e.takes {
		e.hold.part(t.part).unsent -= t.cost
	}
	e.hold.pending--
	e.hold.mu.Unlock()
}

const maxRefusedErrors = 100

func (g *localGate) refusedLocked(err error) {
	if len(g.refused) < maxRefusedErrors {
		g.refused = append(g.refused, err)
	}
}

// settleLocked takes a lease's settled usage off its queue: the outbox refs
// to acknowledge and each hold's settled costs.
func (g *localGate) settleLocked(lease billing.LeaseID, r billing.LeaseUsageResult) ([]billing.LeaseUsageRef, map[*localHold][][]take) {
	q := g.queues[lease]
	if q == nil {
		return nil, nil
	}
	refused := map[int64]billing.LeaseUsageRefusal{}
	for _, f := range r.Refused {
		refused[f.N] = f
	}
	var acks []billing.LeaseUsageRef
	settled := map[*localHold][][]take{}
	for len(q.items) > 0 && q.items[0].item.N <= r.ThroughN {
		e := q.items[0]
		q.items = q.items[1:]
		acks = append(acks, billing.LeaseUsageRef{LeaseID: lease, N: e.item.N})
		if f, ok := refused[e.item.N]; ok {
			cause := error(fmt.Errorf("%w: status %d", billing.ErrUnreachable, f.Status))
			if f.Error != nil {
				cause = &billing.StatusError{Status: f.Status, ErrorDetails: *f.Error}
			}
			g.refusedLocked(fmt.Errorf("openrails: gate: usage %d (%s %s) refused: %w", e.item.N, e.item.Source, e.item.SourceID, cause))
		}
		if e.hold != nil {
			settled[e.hold] = append(settled[e.hold], e.takes)
		}
	}
	if len(q.items) == 0 && lease != g.lease {
		delete(g.queues, lease)
	}
	return acks, settled
}

func (g *localGate) ack(refs []billing.LeaseUsageRef) {
	if g.outbox == nil || len(refs) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), syncTimeout)
	defer cancel()
	_ = g.outbox.Ack(ctx, refs)
}

// resendEarlier sends the usage of earlier leases (rolled over, or read from
// the outbox at start), one usage-only sync per lease.
func (g *localGate) resendEarlier() {
	g.mu.Lock()
	var leases []billing.LeaseID
	for id, q := range g.queues {
		if id != g.lease && len(sendableLocked(q, 1)) > 0 {
			leases = append(leases, id)
		}
	}
	g.mu.Unlock()
	for _, lease := range leases {
		g.mu.Lock()
		usage := sendableLocked(g.queues[lease], billing.MaxSyncUsageItems)
		g.mu.Unlock()
		if len(usage) == 0 {
			continue
		}
		params := billing.SyncLeasesParams{LeaseID: lease, Usage: usage, Wants: []billing.LeaseWant{}, Returns: []billing.HoldReturn{}, Committed: []billing.HoldCommitment{}}
		ctx, cancel := context.WithTimeout(context.Background(), syncTimeout)
		res, err := g.c.SyncLeases(ctx, params)
		cancel()
		g.mu.Lock()
		var acks []billing.LeaseUsageRef
		if err != nil {
			acks = g.failedLocked(lease, params, nil, nil, err)
		} else {
			var settled map[*localHold][][]take
			acks, settled = g.settleLocked(lease, res.Usage)
			for h, list := range settled {
				h.mu.Lock()
				for _, s := range list {
					h.settled(s, false)
				}
				h.mu.Unlock()
			}
		}
		g.mu.Unlock()
		g.ack(acks)
		if err != nil && errors.Is(err, billing.ErrUnreachable) {
			return
		}
	}
}

// flush waits until the usage queued before the call has settled (and, when
// closing, the holds are returned).
func (g *localGate) flush(ctx context.Context, closing bool) error {
	g.ensureStarted()
	g.mu.Lock()
	targets := map[billing.LeaseID]int64{}
	for id, q := range g.queues {
		if n := len(q.items); n > 0 {
			targets[id] = q.items[n-1].item.N
		}
	}
	start := g.attempts
	g.mu.Unlock()
	for {
		g.mu.Lock()
		done := !closing || len(g.returns) == 0
		for id, last := range targets {
			if q := g.queues[id]; q != nil && len(q.items) > 0 && q.items[0].item.N <= last {
				done = false
			}
		}
		failure, attempted, ch := g.failure, g.attempts > start, g.synced
		var refused []error
		if done {
			refused, g.refused = g.refused, nil
		}
		g.mu.Unlock()
		if done {
			return errors.Join(refused...)
		}
		if attempted && failure != nil {
			return unavailable(failure)
		}
		g.poke()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ch:
		}
	}
}

func (g *localGate) flushIfStarted(ctx context.Context) error {
	if !g.started.Load() {
		return nil
	}
	return g.flush(ctx, false)
}

// close sends pending usage, returns every hold and stops the sync loop.
func (g *localGate) close(ctx context.Context) error {
	if !g.started.Load() {
		return nil
	}
	g.mu.Lock()
	g.closing = true
	for attrs, e := range g.wants {
		delete(g.wants, attrs)
		e.answer = wantAnswer{err: errGateClosed}
		close(e.done)
	}
	g.index.Lock()
	for k, h := range g.holds {
		h.mu.Lock()
		h.gone = true
		if h.issued {
			g.returns[k] = h
		}
		h.mu.Unlock()
	}
	g.index.Unlock()
	g.mu.Unlock()
	var err error
	for {
		if err = g.flush(ctx, true); err != nil {
			break
		}
		g.mu.Lock()
		pending := false
		for _, q := range g.queues {
			pending = pending || len(q.items) > 0
		}
		if !pending {
			g.stopped = true
		}
		g.mu.Unlock()
		if !pending {
			break
		}
	}
	g.mu.Lock()
	g.stopped = true
	g.mu.Unlock()
	close(g.stop)
	select {
	case <-g.done:
	case <-ctx.Done():
		err = errors.Join(err, ctx.Err())
	}
	return err
}

// limits is the customer's display lines less its unsent usage.
func (g *localGate) limits(ctx context.Context, customer billing.CustomerID) ([]billing.CustomerLimit, error) {
	g.ensureStarted()
	g.mu.Lock()
	lines, ok := g.display[customer]
	lines = slices.Clone(lines)
	unsent := map[string]int64{}
	for _, q := range g.queues {
		for _, e := range q.items {
			if e.item.CustomerID == customer {
				addUnsent(unsent, e.item.Currency, e.takes)
			}
		}
	}
	g.mu.Unlock()
	if !ok {
		page, err := g.c.ListCustomerLimits(ctx, customer)
		if errors.Is(err, billing.ErrUnreachable) {
			return nil, unavailable(err)
		}
		if err != nil {
			return nil, err
		}
		lines = page.Items
	}
	return lessUnsent(lines, unsent), nil
}

func addUnsent(unsent map[string]int64, currency string, takes []take) {
	for _, t := range takes {
		key := partLimit(t.part) + "\x00"
		if t.money {
			key += currency
		}
		unsent[key] += t.cost
	}
}

func lessUnsent(lines []billing.CustomerLimit, unsent map[string]int64) []billing.CustomerLimit {
	for i, l := range lines {
		key := l.Limit + "\x00"
		if catalog.LimitUnitCurrency(l.Unit) {
			key += l.Unit
		}
		if c := unsent[key]; c != 0 {
			lines[i].Used += c
			lines[i].Remaining -= c
		}
	}
	return lines
}
