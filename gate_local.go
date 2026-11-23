package openrails

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"

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
	// redirect takes usage captured after the gate stopped (a Redis gate's
	// retired fallback); nil refuses it.
	redirect func(context.Context, billing.LeaseUsage) error

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

	mu      sync.Mutex
	lease   billing.LeaseID
	queues  map[billing.LeaseID]*usageQueue
	wants   map[string]*pendingWant
	returns map[holdKey]*localHold
	// applied are the hold versions applied since the last answered sync.
	applied map[holdKey]int64
	// unpersisted are moved items whose old outbox refs are not yet acked.
	unpersisted []move
	display     map[billing.CustomerID][]billing.CustomerLimit
	refused     []error
	// failure is the last sync attempt's failure, nil once one succeeds;
	// unreachable when OpenRails could not be reached.
	failure     error
	unreachable bool
	failedAt    time.Time
	attempts    uint64
	synced      chan struct{}
	closing     bool
	stopped     bool
	recovered   bool

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
		lease: newLeaseID(), queues: map[billing.LeaseID]*usageQueue{},
		wants: map[string]*pendingWant{}, returns: map[holdKey]*localHold{}, applied: map[holdKey]int64{},
		display: map[billing.CustomerID][]billing.CustomerLimit{},
		synced:  make(chan struct{}), kick: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{}),
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

// localHold is one hold of the gate's lease: the server's holds of one
// customer and limit set, merged by part and window. Its mutex serializes
// the decisions on it; every decision takes from all of its limits or none.
type localHold struct {
	key   holdKey
	lease billing.LeaseID
	used  atomic.Int64

	mu        sync.Mutex
	set       holdSet
	pricing   *holdPricing
	parts     map[string]*partState
	events    map[string]bool
	safe      *int64
	safeSpent int64
	open      int
	pending   int
	gone      bool
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

func newLocalHold(key holdKey, lease billing.LeaseID) *localHold {
	return &localHold{key: key, lease: lease, parts: map[string]*partState{}, events: map[string]bool{}}
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
// lease, or short of money, the hold's safe amount (when withSafe) pays the
// money cost in full; units are taken only from a live hold that fits them.
// A hold without parts is cold. low reports a hold the next such request
// would not fit.
func (h *localHold) decide(now time.Time, r request, live, withSafe bool) (takes []take, low bool, out decision, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.gone || h.pricing == nil || len(h.pricing.parts) == 0 {
		return nil, false, decideCold, nil
	}
	takes, money, err := h.pricing.cost(now, r, withSafe && h.safe != nil)
	if errors.Is(err, errUnpriced) || err == nil && len(takes) == 0 {
		return nil, false, decideUnpriced, nil
	}
	if err != nil {
		return nil, false, decideCold, err
	}
	full := slices.Clone(takes)
	short, unitsShort := false, false
	for i, t := range takes {
		p := h.parts[t.part]
		fits := live && p != nil
		if fits {
			avail := p.avail()
			if t.overrun == catalog.OverrunAdmitWhilePositive {
				fits = avail > 0
				if fits && avail < t.cost {
					takes[i].cost = avail
				}
			} else {
				fits = avail >= t.cost
			}
			low = low || avail-t.cost < t.cost
		}
		if !fits {
			short = true
			unitsShort = unitsShort || (!t.money && t.cost > 0)
		}
	}
	short = short || !live
	if short {
		if !withSafe || unitsShort || h.safe == nil || *h.safe-h.safeSpent < money {
			return nil, false, decideShort, nil
		}
		for i, t := range full {
			if t.money {
				takes[i] = t
			}
		}
		low = true
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
	for i := range taken {
		p := h.part(taken[i].part)
		p.open -= taken[i].cost
		p.unsent += actual[i].cost
	}
	h.open--
	h.pending++
}

// uncaptured undoes captured for a capture that failed.
func (h *localHold) uncaptured(taken, actual []take) {
	for i := range taken {
		p := h.part(taken[i].part)
		p.open += taken[i].cost
		p.unsent -= actual[i].cost
	}
	h.open++
	h.pending--
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

// apply takes one answer's holds of the key, after settled usage left it;
// each answer gives the safe amount anew.
func (h *localHold) apply(holds []billing.Hold, now time.Time) {
	h.set.apply(holds, now)
	h.refresh()
	h.safeSpent = 0
}

// refresh recomputes the parts' held and the pricing from the hold set.
func (h *localHold) refresh() {
	merged := h.set.merged()
	h.pricing = newHoldPricing(merged, h.events)
	seen := map[string]bool{}
	for _, l := range merged.Limits {
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
	h.safe = merged.Safe
}

// settled removes settled usage's cost; a hold the sync did not answer also
// loses it from held, which the server consumed. Refused usage consumed
// nothing.
func (h *localHold) settled(takes []take, answered, refused bool) {
	for _, t := range takes {
		h.part(t.part).unsent -= t.cost
		if !answered && !refused {
			h.set.consume(t.part, t.cost)
		}
	}
	h.pending--
	if !answered && !refused {
		h.refresh()
	}
}

type localTicket struct {
	g     *localGate
	hold  *localHold
	takes []take
}

func (lt *localTicket) capture(ctx context.Context, t *Ticket, u billing.CaptureUsage) error {
	item := captureItem(t.params, &t.limitSet, u, time.Now())
	r := request{usage: &limits.Usage{EventType: item.EventType, Dimensions: item.Dimensions, Metadata: item.Metadata}, stated: item.Amount}
	actual, err := lt.hold.actual(r, lt.takes)
	if err != nil {
		return notCaptured{err}
	}
	return lt.g.enqueue(ctx, item, lt.hold, lt.takes, actual)
}

func (lt *localTicket) release() { lt.hold.release(lt.takes) }

// usageQueue is a lease's unsettled usage, in order; next is the highest
// number issued on it.
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

// enqueue numbers an item on the current lease and queues it for the next
// sync. With an outbox it is stored before enqueue returns, and later items
// wait for it; a Put that fails fails the capture, and the item never
// settles.
func (g *localGate) enqueue(ctx context.Context, item billing.LeaseUsage, h *localHold, taken, actual []take) error {
	g.mu.Lock()
	if g.stopped {
		g.mu.Unlock()
		if g.redirect != nil {
			if err := g.redirect(ctx, item); err != nil {
				return notCaptured{err}
			}
			if h != nil {
				h.release(taken)
			}
			return nil
		}
		return notCaptured{errGateClosed}
	}
	lease := g.lease
	q := g.queueLocked(lease)
	q.next++
	item.N = q.next
	e := &queued{item: item, hold: h, takes: actual, durable: g.outbox == nil}
	q.items = append(q.items, e)
	if h != nil {
		h.mu.Lock()
		h.captured(taken, actual)
		h.mu.Unlock()
	}
	g.mu.Unlock()
	if g.outbox == nil {
		return nil
	}
	err := g.outbox.Put(ctx, []billing.OutboxItem{{LeaseID: lease, Usage: item}})
	g.mu.Lock()
	if err == nil {
		e.durable = true
		g.mu.Unlock()
		return nil
	}
	// The item leaves the queue; a later item numbered meanwhile is refused
	// out of order and moves to a new lease.
	if i := slices.Index(q.items, e); i >= 0 {
		q.items = slices.Delete(q.items, i, i+1)
	}
	if q.next == item.N {
		q.next--
	}
	if h != nil {
		h.mu.Lock()
		h.uncaptured(taken, actual)
		h.mu.Unlock()
	}
	g.mu.Unlock()
	// A Put that failed may have stored it anyway.
	ackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), syncTimeout)
	_ = g.outbox.Ack(ackCtx, []billing.LeaseUsageRef{{LeaseID: lease, N: item.N}})
	cancel()
	return notCaptured{fmt.Errorf("openrails: gate: the outbox did not store the usage: %w", err)}
}

func (g *localGate) record(ctx context.Context, item billing.LeaseUsage) error {
	g.ensureStarted()
	err := g.enqueue(ctx, item, nil, nil, nil)
	var nc notCaptured
	if errors.As(err, &nc) {
		return nc.error
	}
	return err
}

// pendingWant is one want of the next sync and the requests waiting for it.
type pendingWant struct {
	key       string
	attrs     string
	eventType string
	want      billing.LeaseWant
	done      chan struct{}
	answer    wantAnswer
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
	key := attrs
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
		a, err := g.await(ctx, key, attrs, p)
		if err != nil {
			return nil, err
		}
		switch a.step(ctx, p) {
		case coldSafe:
			return g.safe(gate, p, r, attrs, a.unavailable)
		case coldFail:
			return nil, a.err
		case coldOwnWant:
			key = ownWantKey(attrs, p)
		case coldRefused:
			return refusedTicket(gate, p, a.refusal), nil
		case coldAnswered:
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

// recentFailure is the last sync's failure to reach OpenRails while it is
// fresh: a cold request then answers at once instead of waiting for another
// attempt.
func (g *localGate) recentFailure(now time.Time) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.failure != nil && g.unreachable && now.Sub(g.failedAt) < limits.SyncInterval {
		return g.failure
	}
	return nil
}

// await adds the request's want to the next sync and waits for its answer.
// Requests of one want key share one want.
func (g *localGate) await(ctx context.Context, key, attrs string, p billing.AdmitParams) (wantAnswer, error) {
	g.mu.Lock()
	if g.closing {
		g.mu.Unlock()
		return wantAnswer{}, errGateClosed
	}
	e := g.wantLocked(key, attrs, p)
	g.mu.Unlock()
	g.poke()
	select {
	case <-ctx.Done():
		return wantAnswer{}, ctx.Err()
	case <-e.done:
		return e.answer, nil
	}
}

func (g *localGate) wantLocked(key, attrs string, p billing.AdmitParams) *pendingWant {
	e := g.wants[key]
	if e == nil {
		e = &pendingWant{key: key, attrs: attrs, done: make(chan struct{})}
		g.wants[key] = e
	}
	e.want, e.eventType = leaseWant(p), eventTypeOf(p)
	return e
}

// prefetch asks the next sync to top up a hold running low.
func (g *localGate) prefetch(attrs string, p billing.AdmitParams) {
	g.mu.Lock()
	if !g.closing && g.wants[attrs] == nil {
		g.wantLocked(attrs, attrs, p)
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
// against it, with close; its holds keep only their safe amounts when
// keepSafe.
func (g *localGate) rolloverLocked(keepSafe bool) {
	g.lease = newLeaseID()
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
	g.applied = map[holdKey]int64{}
}

// evictLocked returns the least recently used idle holds past the bound. A
// hold is found idle and marked gone under its own lock, so no decision
// takes from a hold being returned.
func (g *localGate) evictLocked() {
	g.index.Lock()
	defer g.index.Unlock()
	if over := len(g.holds) - g.maxHolds; over > 0 {
		type candidate struct {
			key  holdKey
			hold *localHold
			used int64
		}
		idle := make([]candidate, 0, len(g.holds))
		for k, h := range g.holds {
			idle = append(idle, candidate{k, h, h.used.Load()})
		}
		sort.Slice(idle, func(i, j int) bool { return idle[i].used < idle[j].used })
		for _, c := range idle {
			if over == 0 {
				break
			}
			c.hold.mu.Lock()
			if c.hold.open == 0 && c.hold.pending == 0 && !c.hold.gone {
				c.hold.gone = true
				delete(g.holds, c.key)
				g.returns[c.key] = c.hold
				over--
			}
			c.hold.mu.Unlock()
		}
	}
	if len(g.sets) > maxGateLimitSets {
		g.sets = map[string]string{}
	}
	if len(g.display) > g.maxHolds {
		g.display = map[billing.CustomerID][]billing.CustomerLimit{}
	}
}

// syncOnce is one sync of the lease, then the usage of earlier leases. A
// closing gate's syncs close its lease.
func (g *localGate) syncOnce() {
	now := time.Now()
	g.mu.Lock()
	closing := g.closing
	if !closing && g.liveUntil.Load() != 0 && !g.leaseLive(now) {
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
	applied := make([]billing.HoldApplied, 0, len(g.applied))
	for k, v := range g.applied {
		if len(applied) == billing.MaxSyncApplied {
			break
		}
		applied = append(applied, billing.HoldApplied{CustomerID: k.customer, LimitSet: k.set, Version: v})
	}
	var entries []*pendingWant
	g.index.RLock()
	committed := commitments(g.holds)
	for key, e := range g.wants {
		if len(entries) == billing.MaxSyncWants {
			break
		}
		if set, ok := g.sets[e.attrs]; ok && returned[holdKey{e.want.CustomerID, set}] != nil {
			continue
		}
		entries = append(entries, e)
		delete(g.wants, key)
	}
	g.index.RUnlock()
	g.mu.Unlock()

	wants := make([]billing.LeaseWant, len(entries))
	for i, e := range entries {
		wants[i] = e.want
	}
	params := billing.SyncLeasesParams{LeaseID: lease, Usage: nonNilUsage(usage), Wants: wants, Returns: returns, Committed: committed, Applied: applied, Close: closing}
	sent := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), syncTimeout)
	res, err := g.c.SyncLeases(ctx, params)
	cancel()
	acks, moves := g.apply(lease, params, entries, returned, res, err, sent)
	g.ack(acks)
	g.persistMoves(moves)
	g.resendEarlier()
	g.mu.Lock()
	more := len(g.wants) > 0 && err == nil || len(sendableLocked(g.queues[g.lease], 1)) > 0 && (len(usage) == billing.MaxSyncUsageItems || len(moves) > 0)
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
		if h.open > 0 {
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

// move is a queued item numbered again on the current lease, after the
// server refused a gap before it; from is its old ref.
type move struct {
	from billing.LeaseUsageRef
	to   billing.LeaseID
	e    *queued
}

// apply takes in one sync's answer, or its failure, and answers its wants.
// A lease the answer reports dead is replaced; its holds are not applied.
func (g *localGate) apply(lease billing.LeaseID, params billing.SyncLeasesParams, entries []*pendingWant, returned map[holdKey]*localHold, res *billing.SyncLeasesResult, err error, sent time.Time) ([]billing.LeaseUsageRef, []move) {
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
		return nil, g.failedLocked(lease, params, entries, returned, err)
	}
	g.failure, g.unreachable, g.failedAt = nil, false, time.Time{}
	current := lease == g.lease
	live := current && res.Lease.Live
	if live && !params.Close {
		g.liveUntil.Store(sent.Add(limits.LeaseLifetime).UnixNano())
	}
	for _, a := range params.Applied {
		k := holdKey{a.CustomerID, a.LimitSet}
		if v, ok := g.applied[k]; ok && v == a.Version {
			delete(g.applied, k)
		}
	}
	acks, settled, moves, collided := g.settleLocked(lease, res.Usage)
	// Usage past a gap moves to a new lease, even a closing gate's.
	roll := current && ((!res.Lease.Live || collided) && !params.Close || len(moves) > 0)

	now := time.Now()
	g.index.Lock()
	if live {
		g.expired = map[holdKey]*localHold{}
	}
	for k, h := range returned {
		if g.holds[k] == h {
			delete(g.holds, k)
		}
	}
	answered := map[*localHold]bool{}
	if live {
		byKey := map[holdKey][]billing.Hold{}
		var order []holdKey
		for _, b := range res.Holds {
			k := holdKey{b.CustomerID, b.LimitSet}
			if byKey[k] == nil {
				order = append(order, k)
			}
			byKey[k] = append(byKey[k], b)
		}
		for _, k := range order {
			h := g.holds[k]
			if h == nil && returned[k] != nil {
				// Its return released it.
				continue
			}
			if h == nil {
				h = newLocalHold(k, lease)
				g.holds[k] = h
			}
			h.mu.Lock()
			for _, s := range settled[h] {
				h.settled(s.takes, true, s.refused)
			}
			h.apply(byKey[k], now)
			if h.set.Version > 0 {
				g.applied[k] = h.set.Version
			}
			h.mu.Unlock()
			answered[h] = true
		}
	}
	for h, list := range settled {
		if answered[h] {
			continue
		}
		h.mu.Lock()
		for _, s := range list {
			h.settled(s.takes, false, s.refused)
		}
		h.mu.Unlock()
	}
	for i, wr := range res.Wants {
		e := entries[i]
		e.answer = g.wantResultLocked(e, wr, live)
		roll = roll || current && !params.Close && e.answer.retry && wr.Error != nil
	}
	if params.Close && current {
		// The closed lease returned every hold.
		g.holds = map[holdKey]*localHold{}
		g.returns = map[holdKey]*localHold{}
	}
	g.index.Unlock()
	if roll {
		g.rolloverLocked(false)
	}
	moves = g.placeMovesLocked(moves)

	for _, line := range res.Limits {
		g.display[line.CustomerID] = nil
	}
	for _, line := range res.Limits {
		g.display[line.CustomerID] = append(g.display[line.CustomerID], line)
	}
	return acks, moves
}

// wantResultLocked answers one want; mu and index are held. Its hold must
// be one the answer applied: a gate never makes a hold of its own.
func (g *localGate) wantResultLocked(e *pendingWant, wr billing.LeaseWantResult, live bool) wantAnswer {
	switch {
	case wr.Error != nil && wr.Error.Code == billing.CodeLeaseNotHeld:
		return wantAnswer{retry: true}
	case wr.Error != nil:
		return wantFailure(wr.Status, *wr.Error)
	case wr.LimitSet == nil:
		return wantAnswer{unavailable: fmt.Errorf("%w: a want was answered with no limit set", billing.ErrUnreachable)}
	}
	if len(g.sets) >= maxGateLimitSets {
		g.sets = map[string]string{}
	}
	g.sets[e.attrs] = *wr.LimitSet
	if wr.Refusal != nil {
		r := *wr.Refusal
		sent := e.want
		return wantAnswer{limitSet: *wr.LimitSet, refusal: &r, sent: &sent}
	}
	h := g.holds[holdKey{e.want.CustomerID, *wr.LimitSet}]
	if !live || h == nil {
		return wantAnswer{retry: true}
	}
	if e.eventType != "" {
		h.mu.Lock()
		if !h.events[e.eventType] {
			h.events[e.eventType] = true
			h.refresh()
		}
		h.mu.Unlock()
	}
	return wantAnswer{limitSet: *wr.LimitSet}
}

// failedLocked handles a sync whose call failed. Usage is never dropped for
// it: OpenRails unreachable (5xx and 429 included) answers the wants so, a
// dead lease rolls over, and any other refusal answers the wants with it and
// keeps the usage, except that a forgotten earlier lease's usage moves to
// the current one, where any too old to bill is refused alone.
func (g *localGate) failedLocked(lease billing.LeaseID, params billing.SyncLeasesParams, entries []*pendingWant, returned map[holdKey]*localHold, err error) []move {
	g.requeueReturnsLocked(returned)
	current := lease == g.lease
	switch classify(err) {
	case faultLeaseNotHeld:
		for _, e := range entries {
			e.answer = wantAnswer{retry: true}
		}
		if current && !params.Close {
			g.rolloverLocked(false)
		}
		return nil
	case faultUnreachable:
		g.failure, g.unreachable = unavailable(err), true
		for _, e := range entries {
			e.answer = wantAnswer{unavailable: err}
		}
	default:
		var se *billing.StatusError
		if !current && errors.As(err, &se) && se.Code == billing.CodeLeaseNotFound && len(params.Usage) > 0 {
			return g.placeMovesLocked(g.takeMovesLocked(lease, params.Usage[0].N))
		}
		g.failure, g.unreachable = err, false
		for _, e := range entries {
			e.answer = wantAnswer{err: err}
		}
	}
	g.failedAt = time.Now()
	return nil
}

func (g *localGate) requeueReturnsLocked(returned map[holdKey]*localHold) {
	for k, h := range returned {
		if g.returns[k] == nil {
			g.returns[k] = h
		}
	}
}

const maxRefusedErrors = 100

func (g *localGate) refusedLocked(err error) {
	if len(g.refused) < maxRefusedErrors {
		g.refused = append(g.refused, err)
	}
}

// settledItem is one settled item's cost in its hold.
type settledItem struct {
	takes   []take
	refused bool
}

// settleLocked takes a lease's settled usage off its queue: the outbox refs
// to acknowledge, each hold's settled costs, the items to number again past
// a gap the server refused, and whether the lease settled numbers the gate
// never issued, so its numbering cannot be trusted.
func (g *localGate) settleLocked(lease billing.LeaseID, r billing.LeaseUsageResult) ([]billing.LeaseUsageRef, map[*localHold][]settledItem, []move, bool) {
	q := g.queues[lease]
	if q == nil {
		return nil, nil, nil, false
	}
	collided := r.ThroughN > q.next
	refused := map[int64]billing.LeaseUsageRefusal{}
	gap := int64(0)
	for _, f := range r.Refused {
		if f.N > r.ThroughN {
			if f.Error != nil && f.Error.Code == billing.CodeUsageOutOfOrder && (gap == 0 || f.N < gap) {
				gap = f.N
			}
			continue
		}
		refused[f.N] = f
	}
	var acks []billing.LeaseUsageRef
	settled := map[*localHold][]settledItem{}
	for len(q.items) > 0 && q.items[0].item.N <= r.ThroughN {
		e := q.items[0]
		q.items = q.items[1:]
		acks = append(acks, billing.LeaseUsageRef{LeaseID: lease, N: e.item.N})
		f, isRefused := refused[e.item.N]
		if isRefused {
			cause := error(fmt.Errorf("%w: status %d", billing.ErrUnreachable, f.Status))
			if f.Error != nil {
				cause = &billing.StatusError{Status: f.Status, ErrorDetails: *f.Error}
			}
			g.refusedLocked(fmt.Errorf("openrails: gate: usage %d (%s %s) refused: %w", e.item.N, e.item.Source, e.item.SourceID, cause))
		}
		if e.hold != nil {
			settled[e.hold] = append(settled[e.hold], settledItem{takes: e.takes, refused: isRefused})
		}
	}
	var moves []move
	if gap > 0 {
		moves = g.takeMovesLocked(lease, gap)
	}
	if len(q.items) == 0 && lease != g.lease {
		delete(g.queues, lease)
	}
	return acks, settled, moves, collided
}

// takeMovesLocked takes a lease's queued items from n on, which the server
// refused past a gap and never settled; an item still being stored stays,
// with those after it, and moves once refused in turn.
func (g *localGate) takeMovesLocked(lease billing.LeaseID, n int64) []move {
	q := g.queues[lease]
	if q == nil {
		return nil
	}
	i := slices.IndexFunc(q.items, func(e *queued) bool { return e.item.N >= n })
	if i < 0 {
		return nil
	}
	j := i
	var moves []move
	for ; j < len(q.items) && q.items[j].durable; j++ {
		moves = append(moves, move{from: billing.LeaseUsageRef{LeaseID: lease, N: q.items[j].item.N}, e: q.items[j]})
	}
	q.items = slices.Delete(q.items, i, j)
	if len(q.items) == 0 && lease != g.lease {
		delete(g.queues, lease)
	}
	return moves
}

// placeMovesLocked numbers moved items on the current lease (after any
// rollover). With an outbox they wait to be sent until persistMoves has
// stored them under their new numbers.
func (g *localGate) placeMovesLocked(moves []move) []move {
	if len(moves) == 0 {
		return nil
	}
	q := g.queueLocked(g.lease)
	for i := range moves {
		q.next++
		moves[i].e.item.N = q.next
		moves[i].e.durable = g.outbox == nil
		moves[i].to = g.lease
		q.items = append(q.items, moves[i].e)
	}
	if g.outbox == nil {
		return nil
	}
	return moves
}

// persistMoves forgets moved items' old refs in the outbox, then stores
// them under their new ones: a crash between loses them, never bills them
// twice. They are sent after; until their old refs are acked they wait,
// retried each sync.
func (g *localGate) persistMoves(moves []move) {
	g.mu.Lock()
	moves = append(g.unpersisted, moves...)
	g.unpersisted = nil
	g.mu.Unlock()
	if len(moves) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), syncTimeout)
	defer cancel()
	from := make([]billing.LeaseUsageRef, len(moves))
	items := make([]billing.OutboxItem, len(moves))
	for i, m := range moves {
		from[i] = m.from
		items[i] = billing.OutboxItem{LeaseID: m.to, Usage: m.e.item}
	}
	if err := g.outbox.Ack(ctx, from); err != nil {
		g.mu.Lock()
		g.unpersisted = append(moves, g.unpersisted...)
		g.mu.Unlock()
		return
	}
	_ = g.outbox.Put(ctx, items)
	g.mu.Lock()
	for _, m := range moves {
		m.e.durable = true
	}
	g.mu.Unlock()
	g.poke()
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
// the outbox at start), one usage-only sync per lease, each closing it.
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
		params := billing.SyncLeasesParams{LeaseID: lease, Usage: usage, Wants: []billing.LeaseWant{}, Returns: []billing.HoldReturn{},
			Committed: []billing.HoldCommitment{}, Applied: []billing.HoldApplied{}, Close: true}
		ctx, cancel := context.WithTimeout(context.Background(), syncTimeout)
		res, err := g.c.SyncLeases(ctx, params)
		cancel()
		g.mu.Lock()
		var acks []billing.LeaseUsageRef
		var moves []move
		if err != nil {
			moves = g.failedLocked(lease, params, nil, nil, err)
		} else {
			var settled map[*localHold][]settledItem
			acks, settled, moves, _ = g.settleLocked(lease, res.Usage)
			moves = g.placeMovesLocked(moves)
			for h, list := range settled {
				h.mu.Lock()
				for _, s := range list {
					h.settled(s.takes, false, s.refused)
				}
				h.mu.Unlock()
			}
		}
		g.mu.Unlock()
		g.ack(acks)
		g.persistMoves(moves)
		if err != nil && classify(err) == faultUnreachable {
			return
		}
	}
}

// flush waits until the usage queued before the call has settled (and, when
// closing, the holds are returned), or a sync fails, or ctx (bounded when it
// has no deadline) ends.
func (g *localGate) flush(ctx context.Context, closing bool) error {
	g.ensureStarted()
	ctx, cancel := boundContext(ctx)
	defer cancel()
	g.mu.Lock()
	targets := map[*queued]bool{}
	for _, q := range g.queues {
		for _, e := range q.items {
			targets[e] = true
		}
	}
	start := g.attempts
	g.mu.Unlock()
	var kicked time.Time
	for {
		g.mu.Lock()
		done := !closing || len(g.returns) == 0
		for _, q := range g.queues {
			for _, e := range q.items {
				done = done && !targets[e]
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
			return failure
		}
		// One early sync a cadence; a backlog's syncs follow each other.
		if time.Since(kicked) >= limits.SyncInterval {
			g.poke()
			kicked = time.Now()
		}
		select {
		case <-ctx.Done():
			return waitErr(ctx)
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

// idle reports a gate with no open ticket and no waiting want.
func (g *localGate) idle() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.wants) > 0 {
		return false
	}
	g.index.RLock()
	defer g.index.RUnlock()
	for _, set := range []map[holdKey]*localHold{g.holds, g.expired} {
		for _, h := range set {
			h.mu.Lock()
			open := h.open
			h.mu.Unlock()
			if open > 0 {
				return false
			}
		}
	}
	return true
}

// close sends pending usage with syncs that close the lease, returning
// every hold, and stops the sync loop, within ctx.
func (g *localGate) close(ctx context.Context) error {
	if !g.started.Load() {
		return nil
	}
	g.mu.Lock()
	g.closing = true
	for key, e := range g.wants {
		delete(g.wants, key)
		e.answer = wantAnswer{err: errGateClosed}
		close(e.done)
	}
	g.index.Lock()
	for k, h := range g.holds {
		h.mu.Lock()
		h.gone = true
		g.returns[k] = h
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
		err = errors.Join(err, waitErr(ctx))
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
