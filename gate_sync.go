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

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/modules/limits"
)

// maxColdRounds bounds the syncs one request waits through for its view.
const maxColdRounds = 20

// gateState is a gate in process memory: one lease for its life, the views
// of the customers it serves, and its usage queue.
type gateState struct {
	c      *Client
	outbox UsageOutbox
	lease  billing.LeaseID

	// vmu guards views, sets and prices. Lock order: mu, vmu, a view.
	vmu    sync.RWMutex
	views  map[viewKey]*custView
	sets   map[string]*billing.LimitSet
	prices map[[2]string]*pricing

	mu sync.Mutex
	// builds counts the syncs built; gates is the last answer's live leases.
	builds int64
	gates  int
	// wanted are the views the next sync asks for, by attributes key, and
	// waiters the requests waiting for them.
	wanted   map[string]billing.ViewParams
	waiters  map[string]*viewWait
	releases []string
	// active is anything done since the last sync; reported is a last sync
	// that reported something in flight; leased is a lease a sync created.
	active   bool
	reported bool
	leased   bool
	tickets  map[*Ticket]struct{}
	queues   map[billing.LeaseID]*usageQueue
	refused  []error
	// failure is the last sync attempt's failure, nil once one succeeds;
	// unreachable when OpenRails could not be reached, since when it has
	// not been.
	failure     error
	unreachable bool
	failedAt    time.Time
	downSince   time.Time
	attempts    uint64
	synced      chan struct{}
	writing     int
	closing     bool
	// leaseClosed is a sync that carried close answered.
	leaseClosed bool
	stopped     bool
	recovered   bool

	// The outbox writer numbers and stores captured usage in batches, so
	// numbers never skip: a batch whose Put fails gives its numbers back.
	// wclosed is a writer that stopped: it takes no more.
	wmu     sync.Mutex
	writes  []*write
	wclosed bool

	kick    chan struct{}
	wkick   chan struct{}
	stop    chan struct{}
	done    chan struct{}
	once    sync.Once
	started atomic.Bool
}

// viewWait is the requests waiting for a sync to answer one view, and how
// many syncs deferred it.
type viewWait struct {
	done     chan struct{}
	err      error
	deferred int
}

func newGateState(c *Client, outbox UsageOutbox) *gateState {
	return &gateState{c: c, outbox: outbox, lease: newLeaseID(),
		views: map[viewKey]*custView{}, sets: map[string]*billing.LimitSet{}, prices: map[[2]string]*pricing{},
		wanted: map[string]billing.ViewParams{}, waiters: map[string]*viewWait{}, tickets: map[*Ticket]struct{}{},
		queues: map[billing.LeaseID]*usageQueue{}, synced: make(chan struct{}), recovered: outbox == nil,
		kick: make(chan struct{}, 1), wkick: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{})}
}

func (g *gateState) ensureStarted() {
	g.once.Do(func() {
		g.started.Store(true)
		go g.run()
		if g.outbox != nil {
			go g.writer()
		}
	})
}

func (g *gateState) poke() {
	select {
	case g.kick <- struct{}{}:
	default:
	}
}

// offline is the outage a gate is in: since its first attempt that could
// not reach OpenRails, until one succeeds.
func (g *gateState) offline() outage {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.failure == nil || !g.unreachable {
		return outage{gates: g.gates}
	}
	return outage{since: g.downSince, gates: g.gates}
}

// lookup is the limit set, view and pricing of a request's attributes.
func (g *gateState) lookup(attrs string, p billing.AdmitParams) (*billing.LimitSet, *custView, *pricing) {
	g.vmu.RLock()
	defer g.vmu.RUnlock()
	set := g.sets[attrs]
	if set == nil {
		return nil, nil, nil
	}
	return set, g.views[viewKey{p.CustomerID, string(p.Currency)}], g.prices[[2]string{string(p.Currency), eventTypeOf(p)}]
}

// lines are a view's lines a set claims, nil when one is missing.
func (v *custView) claimed(set *billing.LimitSet) []*lineState {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := make([]*lineState, len(set.Limits))
	for i, key := range set.Limits {
		if out[i] = v.lines[key]; out[i] == nil {
			return nil
		}
	}
	return out
}

// serve marks a request's view wanted by the next sync.
func (g *gateState) serve(attrs string, p billing.AdmitParams) {
	g.mu.Lock()
	g.active = true
	if _, ok := g.wanted[attrs]; !ok && len(g.wanted) < billing.MaxSyncViews {
		g.wanted[attrs] = viewParams(p)
	}
	g.mu.Unlock()
}

func (g *gateState) admit(ctx context.Context, gate *Gate, p billing.AdmitParams) (*Ticket, error) {
	g.ensureStarted()
	attrs := attrsKey(p)
	r := admitRequest(p)
	g.serve(attrs, p)
	if p.AccrualRateDeltaPerHour != 0 {
		return g.admitHeld(ctx, gate, p, nil, nil)
	}
	for round := 0; ; round++ {
		now := time.Now()
		set, v, pr := g.lookup(attrs, p)
		if set != nil && set.Refusal != nil {
			return refusedTicket(gate, p, set.Refusal), nil
		}
		if set != nil && v != nil {
			lines := v.claimed(set)
			if lines != nil {
				costs, err := pr.costs(lines, r)
				if errors.Is(err, errUnpriced) {
					return g.admitHeld(ctx, gate, p, nil, nil)
				}
				if err != nil {
					return nil, err
				}
				d := v.decide(*set, costs, now, g.offline())
				switch d.verdict {
				case verdictLocal:
					return g.open(gate, p, v, d.takes, now), nil
				case verdictRefuse:
					return refusedTicket(gate, p, d.refusal), nil
				case verdictNear:
					return g.admitHeld(ctx, gate, p, v, d.takes)
				case verdictShort:
					return nil, unavailable(g.lastFailure())
				}
			}
		}
		if round >= maxColdRounds {
			return nil, unavailable(errors.New("no view answered the request"))
		}
		if failure := g.recentFailure(now); failure != nil {
			return nil, unavailable(failure)
		}
		if err := g.await(ctx, attrs, p); err != nil {
			return nil, err
		}
	}
}

// open is a locally admitted ticket.
func (g *gateState) open(gate *Gate, p billing.AdmitParams, v *custView, takes []take, now time.Time) *Ticket {
	g.mu.Lock()
	defer g.mu.Unlock()
	t := &Ticket{gate: gate, params: p, view: v, takes: takes, admittedAt: now, build: g.builds}
	g.tickets[t] = struct{}{}
	return t
}

// admitHeld decides a request on the server, as a request hold of the
// gate's lease: near a limit (its takes already pinned in v), for a request
// the view cannot price, or one that declares an accrual rate.
func (g *gateState) admitHeld(ctx context.Context, gate *Gate, p billing.AdmitParams, v *custView, takes []take) (*Ticket, error) {
	t, err := g.holdOnServer(ctx, gate, p, v)
	if v != nil && len(takes) > 0 {
		if err != nil || !t.Allowed() {
			v.unpin(takes)
		} else {
			g.mu.Lock()
			build := g.builds
			// A sync built after the hold answers the view that shows it.
			g.wanted[attrsKey(p)] = viewParams(p)
			g.active = true
			g.mu.Unlock()
			v.pin(takes, build)
		}
	}
	return t, err
}

func (g *gateState) holdOnServer(ctx context.Context, gate *Gate, p billing.AdmitParams, v *custView) (*Ticket, error) {
	params := p
	lease := g.lease
	params.LeaseID, params.ExpiresAt = &lease, nil
	if err := g.ensureLease(ctx); err != nil {
		return nil, err
	}
	g.adopt(p.RequestID)
	verdicts, err := g.c.Admit(ctx, []billing.AdmitParams{params})
	if err != nil && (ctx.Err() != nil || errors.Is(err, billing.ErrUnreachable) || unreachable(err)) {
		// The server may have made the hold: the next sync releases it,
		// unless a retry under the request id adopts it first. One caller's
		// ctx says nothing of OpenRails.
		g.mu.Lock()
		g.releases = append(g.releases, p.RequestID)
		g.active = true
		g.mu.Unlock()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		g.failed(err, true)
		return nil, unavailable(err)
	}
	if err != nil {
		return nil, err
	}
	if len(verdicts) != 1 {
		return nil, fmt.Errorf("%w: admit answered %d verdicts for one request", billing.ErrUnreachable, len(verdicts))
	}
	verdict := verdicts[0]
	if verdict.Admission == nil {
		if verdict.Error != nil {
			return nil, &billing.StatusError{Status: verdict.Status, ErrorDetails: *verdict.Error}
		}
		return nil, fmt.Errorf("%w: admit answered neither an admission nor an error", billing.ErrUnreachable)
	}
	if r := verdict.Admission.Refusal(); r != nil || !verdict.Allowed() {
		if r == nil {
			r = &billing.Refusal{DenyCode: "not_admitted"}
		}
		return refusedTicket(gate, p, r), nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	t := &Ticket{gate: gate, params: p, held: true, view: v, admittedAt: time.Now(), build: g.builds}
	g.tickets[t] = struct{}{}
	return t, nil
}

// adopt keeps the lease's hold of a request id that an earlier, unanswered
// attempt queued for release: this request owns it now.
func (g *gateState) adopt(requestID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.releases = slices.DeleteFunc(g.releases, func(id string) bool { return id == requestID })
}

// ensureLease makes sure the lease exists before a hold names it: a gate's
// first request can be held before its first sync answered.
func (g *gateState) ensureLease(ctx context.Context) error {
	for {
		g.mu.Lock()
		leased, ch, start := g.leased, g.synced, g.attempts
		g.mu.Unlock()
		if leased {
			return nil
		}
		g.poke()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ch:
		}
		g.mu.Lock()
		failure := g.failure
		tried := g.attempts > start
		g.mu.Unlock()
		if tried && failure != nil {
			return unavailable(failure)
		}
	}
}

// recentFailure is the last sync's failure to reach OpenRails while it is
// fresh: a request with no view answers at once instead of waiting.
func (g *gateState) recentFailure(now time.Time) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.failure != nil && g.unreachable && now.Sub(g.failedAt) < limits.SyncInterval {
		return g.failure
	}
	return nil
}

func (g *gateState) lastFailure() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.failure
}

func (g *gateState) failed(err error, unreachable bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.failLocked(unavailable(err), unreachable, time.Now())
}

// failLocked records a failed attempt made at at; the first unreachable
// one starts an outage.
func (g *gateState) failLocked(err error, unreachable bool, at time.Time) {
	if unreachable && (g.failure == nil || !g.unreachable) {
		g.downSince = at
	}
	g.failure, g.unreachable, g.failedAt = err, unreachable, time.Now()
}

// await asks the next sync for the request's view and waits for its answer.
// Requests of one attributes key share one wait.
func (g *gateState) await(ctx context.Context, attrs string, p billing.AdmitParams) error {
	g.mu.Lock()
	if g.closing {
		g.mu.Unlock()
		return errGateClosed
	}
	w := g.waiters[attrs]
	if w == nil {
		w = &viewWait{done: make(chan struct{})}
		g.waiters[attrs] = w
	}
	g.wanted[attrs] = viewParams(p)
	g.active = true
	g.mu.Unlock()
	g.poke()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-w.done:
		return w.err
	}
}

func (g *gateState) capture(ctx context.Context, t *Ticket, u billing.CaptureUsage) error {
	item := captureItem(t, u, time.Now())
	if t.held {
		// The view that carries the captured hold clears its pin.
		return g.enqueue(ctx, item, t.view, nil, t)
	}
	_, _, pr := g.lookup(attrsKey(t.params), t.params)
	lines := make([]*lineState, len(t.takes))
	for i, tk := range t.takes {
		lines[i] = tk.line
	}
	costs, err := pr.costs(lines, usageRequest(item))
	if err != nil {
		costs = make([]int64, len(t.takes))
		for i, tk := range t.takes {
			costs[i] = tk.cost
		}
	}
	actual := make([]take, len(t.takes))
	t.view.mu.Lock()
	for i, tk := range t.takes {
		actual[i] = tk
		actual[i].cost = costs[i]
		tk.span.open = satAdd(tk.span.open, -tk.cost)
		tk.span.unsent = satAdd(tk.span.unsent, costs[i])
	}
	t.view.mu.Unlock()
	if err := g.enqueue(ctx, item, t.view, actual, t); err != nil {
		t.view.mu.Lock()
		for i, tk := range t.takes {
			tk.span.open = satAdd(tk.span.open, tk.cost)
			tk.span.unsent = satAdd(tk.span.unsent, -costs[i])
		}
		t.view.mu.Unlock()
		return err
	}
	return nil
}

func (g *gateState) release(t *Ticket) {
	g.mu.Lock()
	delete(g.tickets, t)
	g.active = true
	if t.held {
		g.releases = append(g.releases, t.params.RequestID)
	}
	g.mu.Unlock()
	if !t.held && t.view != nil {
		t.view.mu.Lock()
		for _, tk := range t.takes {
			tk.span.open = satAdd(tk.span.open, -tk.cost)
		}
		t.view.mu.Unlock()
	}
	g.poke()
}

// record queues usage no admission held, counted in the view of its
// attributes when the gate has one.
func (g *gateState) record(ctx context.Context, item billing.LeaseUsage) error {
	g.ensureStarted()
	p := billing.AdmitParams{CustomerID: item.CustomerID, Invoker: item.Invoker, InvokerType: item.InvokerType, Currency: item.Currency,
		Usage: &billing.UsageEstimate{EventType: item.EventType}}
	set, v, pr := g.lookup(attrsKey(p), p)
	var takes []take
	if set != nil && v != nil {
		if lines := v.claimed(set); lines != nil {
			if costs, err := pr.costs(lines, usageRequest(item)); err == nil {
				now := time.Now()
				v.mu.Lock()
				for i, l := range lines {
					if s, ok := l.spanAt(now); ok {
						takes = append(takes, takeOf(l, s, costs[i]))
						s.unsent = satAdd(s.unsent, costs[i])
					}
				}
				v.mu.Unlock()
			}
		}
	}
	if err := g.enqueue(ctx, item, v, takes, nil); err != nil {
		if v != nil {
			v.mu.Lock()
			for _, t := range takes {
				t.span.unsent = satAdd(t.span.unsent, -t.cost)
			}
			v.mu.Unlock()
		}
		return err
	}
	return nil
}

// usageQueue is a lease's unsettled usage, in order; next is the highest
// number issued on it.
type usageQueue struct {
	next  int64
	items []*queued
}

// queued is one usage item, what it costs in its view, the view the sync
// carrying it asks for, the sync count when it was captured, how many syncs
// carried it, and whether its ticket was reported in flight.
type queued struct {
	item     billing.LeaseUsage
	view     *custView
	takes    []take
	attrs    string
	ask      billing.ViewParams
	build    int64
	sends    int
	reported bool
}

// write is one item for the outbox writer.
type write struct {
	q    *queued
	done chan error
}

func (g *gateState) queueLocked(lease billing.LeaseID) *usageQueue {
	q := g.queues[lease]
	if q == nil {
		q = &usageQueue{}
		g.queues[lease] = q
	}
	return q
}

// enqueue numbers an item on the lease and queues it for the next sync;
// with an outbox it is stored first, in the writer's next batch. A ticket's
// capture leaves the open tickets once its item is queued.
func (g *gateState) enqueue(ctx context.Context, item billing.LeaseUsage, v *custView, takes []take, t *Ticket) error {
	g.ensureStarted()
	g.mu.Lock()
	if g.stopped {
		g.mu.Unlock()
		return errGateClosed
	}
	e := &queued{item: item, view: v, takes: takes, build: g.builds}
	if t != nil && v != nil {
		e.attrs, e.ask = attrsKey(t.params), viewParams(t.params)
	}
	// A ticket the last sync reported stays reported until its usage settles.
	e.reported = t != nil && !t.held && t.build < g.builds-1
	if g.outbox == nil {
		q := g.queueLocked(g.lease)
		q.next++
		e.item.N = q.next
		q.items = append(q.items, e)
		if t != nil {
			delete(g.tickets, t)
		}
		g.active = true
		g.mu.Unlock()
		g.poke()
		return nil
	}
	g.mu.Unlock()
	w := &write{q: e, done: make(chan error, 1)}
	g.wmu.Lock()
	if g.wclosed {
		g.wmu.Unlock()
		return errGateClosed
	}
	g.writes = append(g.writes, w)
	g.wmu.Unlock()
	select {
	case g.wkick <- struct{}{}:
	default:
	}
	// The writer answers whatever ctx does: an item it stored is queued.
	if err := <-w.done; err != nil {
		return err
	}
	if t != nil {
		g.mu.Lock()
		delete(g.tickets, t)
		g.mu.Unlock()
	}
	g.poke()
	return nil
}

// writer stores queued usage in the outbox in batches and numbers it on the
// lease; a batch whose Put fails gives its numbers back, so the lease never
// skips one.
func (g *gateState) writer() {
	for {
		select {
		case <-g.stop:
			g.failWrites(errGateClosed)
			return
		case <-g.wkick:
		}
		g.wmu.Lock()
		batch := g.writes
		g.writes = nil
		g.wmu.Unlock()
		if len(batch) == 0 {
			continue
		}
		g.mu.Lock()
		if g.stopped {
			g.mu.Unlock()
			for _, w := range batch {
				w.done <- errGateClosed
			}
			continue
		}
		q := g.queueLocked(g.lease)
		items := make([]billing.OutboxItem, len(batch))
		for i, w := range batch {
			q.next++
			w.q.item.N = q.next
			items[i] = billing.OutboxItem{LeaseID: g.lease, Usage: w.q.item}
		}
		g.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), syncTimeout)
		err := g.outbox.Put(ctx, items)
		cancel()
		g.mu.Lock()
		if err != nil {
			q.next -= int64(len(batch))
		} else {
			for _, w := range batch {
				q.items = append(q.items, w.q)
			}
			g.active = true
		}
		g.mu.Unlock()
		if err != nil {
			err = fmt.Errorf("openrails: gate: the outbox did not store the usage: %w", err)
		}
		for _, w := range batch {
			w.done <- err
		}
	}
}

// failWrites answers the queued writes with err and takes no more.
func (g *gateState) failWrites(err error) {
	g.wmu.Lock()
	batch := g.writes
	g.writes, g.wclosed = nil, true
	g.wmu.Unlock()
	for _, w := range batch {
		w.done <- err
	}
}

func (g *gateState) run() {
	defer close(g.done)
	ticker := time.NewTicker(limits.SyncInterval)
	defer ticker.Stop()
	kicked := true
	for {
		g.recover()
		if kicked || !g.idle() {
			// An earlier process's usage settles first, so the views this
			// sync answers include it.
			g.resendEarlier()
			g.syncOnce()
		}
		select {
		case <-g.stop:
			return
		case <-ticker.C:
			kicked = false
		case <-g.kick:
			kicked = true
		}
	}
}

// idle reports a gate with nothing to sync: no open ticket, no queued usage
// or release, no view asked for, nothing done since the last sync and
// nothing reported in flight. It stops syncing and lets its lease lapse.
func (g *gateState) idle() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.active || g.reported || g.closing || len(g.tickets) > 0 || len(g.releases) > 0 || len(g.wanted) > 0 || len(g.waiters) > 0 {
		return false
	}
	for _, q := range g.queues {
		if len(q.items) > 0 {
			return false
		}
	}
	return true
}

// recover queues the outbox's pending usage once, against its leases.
func (g *gateState) recover() {
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
		q.items = append(q.items, &queued{item: it.Usage})
		q.next = it.Usage.N
	}
	g.recovered = true
}

// headLocked is a queue's first items, up to limit.
func headLocked(q *usageQueue, limit int) []*queued {
	if q == nil {
		return nil
	}
	return q.items[:min(len(q.items), limit)]
}

// inFlightLocked is what the gate reports in flight: the takes of tickets
// open since before the previous sync (number prev) was built, and the costs
// of usage captured before it that no sync has settled, by customer and
// limit. carried is this sync's usage: an item it carries for the first time
// is left to its settlement.
func (g *gateState) inFlightLocked(carried map[*queued]bool, prev int64) []billing.InFlight {
	type key struct {
		customer    billing.CustomerID
		limit, unit string
	}
	sum := map[key]int64{}
	var order []key
	add := func(v *custView, takes []take) {
		if v == nil {
			return
		}
		for _, t := range takes {
			k := key{v.key.customer, t.limit, t.unit}
			if _, ok := sum[k]; !ok {
				order = append(order, k)
			}
			sum[k] = satAdd(sum[k], t.cost)
		}
	}
	for t := range g.tickets {
		if !t.held && t.build < prev {
			add(t.view, t.takes)
		}
	}
	for _, e := range g.queues[g.lease].itemsOrNil() {
		if e.reported || e.build < prev && (e.sends > 0 || !carried[e]) {
			add(e.view, e.takes)
		}
	}
	out := make([]billing.InFlight, 0, len(order))
	for _, k := range order {
		if sum[k] > 0 && len(out) < billing.MaxSyncInFlight {
			out = append(out, billing.InFlight{CustomerID: k.customer, Limit: k.limit, Unit: k.unit, Amount: sum[k]})
		}
	}
	return out
}

func (q *usageQueue) itemsOrNil() []*queued {
	if q == nil {
		return nil
	}
	return q.items
}

// syncOnce is one sync of the lease, then the usage of earlier leases. A
// closing gate's sync closes its lease.
func (g *gateState) syncOnce() {
	g.mu.Lock()
	closing := g.closing
	g.builds++
	build := g.builds
	head := headLocked(g.queues[g.lease], billing.MaxSyncUsageItems)
	carried := map[*queued]bool{}
	usage := make([]billing.LeaseUsage, len(head))
	for i, e := range head {
		carried[e] = true
		usage[i] = e.item
		// The view of usage this sync settles comes back with it.
		if e.attrs != "" && len(g.wanted) < billing.MaxSyncViews {
			if _, ok := g.wanted[e.attrs]; !ok {
				g.wanted[e.attrs] = e.ask
			}
		}
	}
	report := g.inFlightLocked(carried, build-1)
	for _, e := range head {
		e.sends++
	}
	var keys []string
	var views []billing.ViewParams
	for k, vp := range g.wanted {
		if len(views) == billing.MaxSyncViews {
			break
		}
		keys, views = append(keys, k), append(views, vp)
		delete(g.wanted, k)
	}
	releases := g.releases[:min(len(g.releases), billing.MaxSyncReleases)]
	g.releases = slices.Clone(g.releases[len(releases):])
	g.active = false
	g.mu.Unlock()

	params := billing.SyncLeasesParams{LeaseID: g.lease, Usage: usage, InFlight: report, Views: views, Releases: releases, Close: closing}
	sent := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), syncTimeout)
	res, err := g.c.SyncLeases(ctx, params)
	cancel()
	acks := g.apply(params, keys, res, err, sent, build)
	g.write(acks)
	g.mu.Lock()
	more := len(g.wanted) > 0 && err == nil || len(headLocked(g.queues[g.lease], 1)) > 0 && (len(usage) == billing.MaxSyncUsageItems || partial(usage, res, err))
	g.mu.Unlock()
	if more {
		g.poke()
	}
}

// partial reports a sync that settled some of its usage and not the rest,
// which the server left for the next sync.
func partial(usage []billing.LeaseUsage, res *billing.SyncLeasesResult, err error) bool {
	return err == nil && len(usage) > 0 && res.Usage.ThroughN >= usage[0].N && res.Usage.ThroughN < usage[len(usage)-1].N
}

// apply takes in one sync's answer, or its failure: settled usage leaves
// the views it was counted in at the moment the same views are replaced,
// and the requests waiting for a view are answered.
func (g *gateState) apply(params billing.SyncLeasesParams, keys []string, res *billing.SyncLeasesResult, err error, sent time.Time, build int64) []billing.LeaseUsageRef {
	if err == nil && len(res.LimitSets) != len(params.Views) {
		err = fmt.Errorf("%w: a sync answered %d limit sets for %d views", billing.ErrUnreachable, len(res.LimitSets), len(params.Views))
	}
	g.mu.Lock()
	if err != nil {
		g.releases = append(params.Releases, g.releases...)
		down := unreachable(err)
		if down {
			// Asked again once OpenRails answers; a refusal is not.
			for i, k := range keys {
				if _, ok := g.wanted[k]; !ok {
					g.wanted[k] = params.Views[i]
				}
			}
			err = unavailable(err)
		}
		g.failLocked(err, down, sent)
		g.answerWaitersLocked(keys, nil, nil, g.failure)
		g.attempts++
		g.writing++
		g.mu.Unlock()
		return nil
	}
	g.failure, g.unreachable, g.failedAt, g.downSince = nil, false, time.Time{}, time.Time{}
	g.gates, g.reported, g.leased = res.Gates, len(params.InFlight) > 0, true
	g.leaseClosed = g.leaseClosed || params.Close
	acks, settled := g.settleLocked(g.lease, res.Usage)
	g.attempts++
	g.writing++
	g.mu.Unlock()

	// Settled usage leaves each view as the view is replaced; a new view is
	// published once its lines are in, and limit sets after their views.
	answered := map[viewKey]billing.View{}
	for _, v := range res.Views {
		answered[viewKey{v.CustomerID, normalizeCurrency(string(v.Currency))}] = v
	}
	g.vmu.RLock()
	touched := map[*custView]bool{}
	for v := range settled {
		touched[v] = true
	}
	fresh := map[viewKey]*custView{}
	for k := range answered {
		if v := g.views[k]; v != nil {
			touched[v] = true
		} else {
			fresh[k] = newCustView(k)
		}
	}
	g.vmu.RUnlock()
	for k, v := range fresh {
		v.mu.Lock()
		v.apply(answered[k], sent, build, res.Gates)
		v.mu.Unlock()
	}
	for v := range touched {
		v.mu.Lock()
		view, ok := answered[v.key]
		for _, e := range settled[v] {
			for _, t := range e.takes {
				t.span.unsent = satAdd(t.span.unsent, -t.cost)
				if !ok {
					// No new view shows it: the window holds it as the server does.
					t.span.remaining = satAdd(t.span.remaining, -t.cost)
				}
			}
		}
		if ok {
			v.apply(view, sent, build, res.Gates)
		}
		v.mu.Unlock()
	}
	g.vmu.Lock()
	for k, v := range fresh {
		if g.views[k] == nil {
			g.views[k] = v
		}
	}
	for _, p := range res.Pricing {
		g.prices[[2]string{normalizeCurrency(string(p.Currency)), p.EventType}] = newPricing(p)
	}
	// A refused request's attributes decide nothing locally until a view
	// answers them again.
	refused := map[string]error{}
	for _, f := range res.RefusedViews {
		if f.Index < 0 || f.Index >= len(keys) {
			continue
		}
		cause := error(fmt.Errorf("%w: status %d", billing.ErrUnreachable, f.Status))
		if f.Error != nil {
			cause = &billing.StatusError{Status: f.Status, ErrorDetails: *f.Error}
		}
		refused[keys[f.Index]] = cause
		delete(g.sets, keys[f.Index])
	}
	for i, ls := range res.LimitSets {
		if ls == nil {
			continue
		}
		if len(g.sets) >= maxGateSets {
			g.sets = map[string]*billing.LimitSet{}
		}
		g.sets[keys[i]] = ls
	}
	g.vmu.Unlock()
	g.evict()

	g.mu.Lock()
	var setKeys, deferred []string
	for i, ls := range res.LimitSets {
		switch k := keys[i]; {
		case ls != nil:
			setKeys = append(setKeys, k)
		case refused[k] == nil:
			// Out of time: asked again.
			deferred = append(deferred, k)
			if _, ok := g.wanted[k]; !ok {
				g.wanted[k] = params.Views[i]
			}
		}
	}
	g.answerWaitersLocked(setKeys, refused, deferred, nil)
	g.mu.Unlock()
	return acks
}

// answerWaitersLocked wakes the requests waiting for the answered views,
// with their refusal for refused ones, or with err when the sync failed.
// Ones whose view the sync deferred wait on, up to maxColdRounds deferrals.
func (g *gateState) answerWaitersLocked(answered []string, refused map[string]error, deferred []string, err error) {
	wake := func(k string, err error) {
		if w := g.waiters[k]; w != nil {
			w.err = err
			close(w.done)
			delete(g.waiters, k)
		}
	}
	for _, k := range answered {
		wake(k, err)
	}
	for k, cause := range refused {
		wake(k, cause)
	}
	for _, k := range deferred {
		if w := g.waiters[k]; w != nil {
			if w.deferred++; w.deferred >= maxColdRounds {
				wake(k, unavailable(errors.New("no sync answered the request's view")))
			}
		}
	}
	if err != nil {
		for k, w := range g.waiters {
			if !slices.Contains(deferred, k) {
				w.err = err
				close(w.done)
				delete(g.waiters, k)
			}
		}
	}
}

// evict drops the views no request used within viewKeep, and the least
// recently used past maxGateViews. A view with anything in flight stays: the
// server's views leave out this gate's report, so only it counts that.
func (g *gateState) evict() {
	g.vmu.Lock()
	defer g.vmu.Unlock()
	now := time.Now()
	type used struct {
		k viewKey
		t time.Time
	}
	var all []used
	for k, v := range g.views {
		v.mu.Lock()
		t := v.used
		busy := v.busyLocked()
		v.mu.Unlock()
		if busy {
			continue
		}
		if !t.IsZero() && now.Sub(t) > viewKeep {
			delete(g.views, k)
			continue
		}
		all = append(all, used{k, t})
	}
	if over := len(all) - maxGateViews; over > 0 {
		sort.Slice(all, func(i, j int) bool { return all[i].t.Before(all[j].t) })
		for _, u := range all[:over] {
			delete(g.views, u.k)
		}
	}
}

// busyLocked reports a view with something in flight in a window it
// decides in.
func (v *custView) busyLocked() bool {
	if len(v.pinned) > 0 {
		return true
	}
	for _, l := range v.lines {
		for _, s := range []*span{l.cur, l.next} {
			if s != nil && (s.open != 0 || s.unsent != 0 || s.pinned != 0) {
				return true
			}
		}
	}
	return false
}

// settledItem is a settled item's view and costs.
type settledItem struct {
	takes []take
}

// settleLocked takes a lease's settled usage off its queue: the outbox refs
// to acknowledge and each view's settled costs. A refused item posted
// nothing; it leaves its view the same way.
func (g *gateState) settleLocked(lease billing.LeaseID, r billing.LeaseUsageResult) ([]billing.LeaseUsageRef, map[*custView][]settledItem) {
	q := g.queues[lease]
	if q == nil {
		return nil, nil
	}
	refused := map[int64]billing.LeaseUsageRefusal{}
	for _, f := range r.Refused {
		if f.N <= r.ThroughN {
			refused[f.N] = f
		}
	}
	var acks []billing.LeaseUsageRef
	settled := map[*custView][]settledItem{}
	for len(q.items) > 0 && q.items[0].item.N <= r.ThroughN {
		e := q.items[0]
		q.items = q.items[1:]
		acks = append(acks, billing.LeaseUsageRef{LeaseID: lease, N: e.item.N})
		if f, ok := refused[e.item.N]; ok {
			cause := error(fmt.Errorf("%w: status %d", billing.ErrUnreachable, f.Status))
			if f.Error != nil {
				cause = &billing.StatusError{Status: f.Status, ErrorDetails: *f.Error}
			}
			if len(g.refused) < maxRefusedErrors {
				g.refused = append(g.refused, fmt.Errorf("openrails: gate: usage %d (%s %s) refused: %w", e.item.N, e.item.Source, e.item.SourceID, cause))
			}
		}
		if e.view != nil {
			settled[e.view] = append(settled[e.view], settledItem{takes: e.takes})
		}
	}
	if len(q.items) == 0 && lease != g.lease {
		delete(g.queues, lease)
	}
	return acks, settled
}

const maxRefusedErrors = 100

// write acknowledges a settlement in the outbox, then wakes the flushes
// waiting for it.
func (g *gateState) write(acks []billing.LeaseUsageRef) {
	if g.outbox != nil && len(acks) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), syncTimeout)
		_ = g.outbox.Ack(ctx, acks)
		cancel()
	}
	g.mu.Lock()
	g.writing--
	close(g.synced)
	g.synced = make(chan struct{})
	g.mu.Unlock()
}

// resendEarlier sends the usage of earlier leases, read from the outbox at
// start, in usage-only syncs that close them (their processes are gone),
// each lease until its queue is empty or a sync settles nothing. Items an
// earlier lease cannot settle (a forgotten lease, or past a gap its outbox
// lost) are stored again on the current lease: their old refs are acked
// first, so a crash loses them rather than bill them twice.
func (g *gateState) resendEarlier() {
	g.mu.Lock()
	var leases []billing.LeaseID
	for id, q := range g.queues {
		if id != g.lease && len(q.items) > 0 {
			leases = append(leases, id)
		}
	}
	g.mu.Unlock()
	for _, lease := range leases {
		for {
			progress, ok := g.resendOnce(lease)
			if !ok {
				return
			}
			if !progress {
				break
			}
		}
	}
}

// resendOnce is one usage-only sync of an earlier lease: whether it settled
// or moved anything, and false when OpenRails could not be reached.
func (g *gateState) resendOnce(lease billing.LeaseID) (progress, ok bool) {
	g.mu.Lock()
	head := headLocked(g.queues[lease], billing.MaxSyncUsageItems)
	usage := make([]billing.LeaseUsage, len(head))
	for i, e := range head {
		usage[i] = e.item
	}
	g.mu.Unlock()
	if len(usage) == 0 {
		return false, true
	}
	ctx, cancel := context.WithTimeout(context.Background(), syncTimeout)
	res, err := g.c.SyncLeases(ctx, billing.SyncLeasesParams{LeaseID: lease, Usage: usage, Close: true})
	cancel()
	if err != nil && unreachable(err) {
		return false, false
	}
	var moved []*queued
	var acks []billing.LeaseUsageRef
	g.mu.Lock()
	g.writing++
	switch {
	case err != nil:
		if code, ok := billing.ErrorCodeFrom(err); ok && code == billing.CodeLeaseNotFound {
			moved = g.takeLocked(lease, 0)
		}
	default:
		acks, _ = g.settleLocked(lease, res.Usage)
		for _, f := range res.Usage.Refused {
			if f.N > res.Usage.ThroughN && f.Error != nil && f.Error.Code == billing.CodeUsageOutOfOrder {
				moved = g.takeLocked(lease, f.N)
				break
			}
		}
	}
	g.mu.Unlock()
	for _, e := range moved {
		acks = append(acks, billing.LeaseUsageRef{LeaseID: lease, N: e.item.N})
	}
	g.write(acks)
	for _, e := range moved {
		e.item.N = 0
		_ = g.enqueue(context.Background(), e.item, nil, nil, nil)
	}
	return len(acks) > 0, true
}

// takeLocked takes a lease's queued items from number n on.
func (g *gateState) takeLocked(lease billing.LeaseID, n int64) []*queued {
	q := g.queues[lease]
	if q == nil {
		return nil
	}
	i := slices.IndexFunc(q.items, func(e *queued) bool { return e.item.N >= n })
	if i < 0 {
		return nil
	}
	out := slices.Clone(q.items[i:])
	q.items = q.items[:i]
	if len(q.items) == 0 {
		delete(g.queues, lease)
	}
	return out
}

// flush waits until the usage queued before the call has settled, or a
// sync fails, or ctx (bounded when it has no deadline) ends.
func (g *gateState) flush(ctx context.Context, closing bool) error {
	if !g.started.Load() {
		return nil
	}
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
		done := g.writing == 0 && (!closing || len(g.releases) == 0)
		for _, q := range g.queues {
			for _, e := range q.items {
				done = done && !targets[e]
			}
		}
		failure, down, attempted, ch := g.failure, g.unreachable, g.attempts > start, g.synced
		var refused []error
		if done {
			refused, g.refused = g.refused, nil
		}
		g.mu.Unlock()
		if done {
			return errors.Join(refused...)
		}
		// Closing rides out OpenRails being unreachable until its bound:
		// captured usage is never dropped for a transient failure.
		if attempted && failure != nil && (!closing || !down) {
			return failure
		}
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

// close sends pending usage with syncs that close the lease, and stops the
// sync loop, within ctx.
func (g *gateState) close(ctx context.Context) error {
	if !g.started.Load() {
		return nil
	}
	g.mu.Lock()
	g.closing = true
	for k, w := range g.waiters {
		w.err = errGateClosed
		close(w.done)
		delete(g.waiters, k)
	}
	g.mu.Unlock()
	err := g.flush(ctx, true)
	if err == nil {
		err = g.closeLease(ctx)
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
	return g.unsent(err)
}

// unsent names the usage a closed gate still holds, wrapping why it was not
// sent: the next process resends it from an outbox; without one it is lost.
func (g *gateState) unsent(cause error) error {
	g.mu.Lock()
	if g.failure != nil && !errors.Is(cause, g.failure) {
		// Why the last sync failed, beside why Close stopped waiting.
		cause = errors.Join(g.failure, cause)
	}
	n := 0
	var held []string
	for lease, q := range g.queues {
		if len(q.items) == 0 {
			continue
		}
		n += len(q.items)
		held = append(held, fmt.Sprintf("lease %s items %d to %d", lease, q.items[0].item.N, q.items[len(q.items)-1].item.N))
	}
	g.mu.Unlock()
	if n == 0 {
		return cause
	}
	if cause == nil {
		cause = ErrUnavailable
	}
	slices.Sort(held)
	return fmt.Errorf("openrails: gate: closed with %d usage items unsent (%s): %w", n, strings.Join(held, "; "), cause)
}

// closeLease waits for a sync built after Close began, which closes the
// lease; one refused ends the wait with its failure, and one that could not
// reach OpenRails is retried, at most once a sync interval.
func (g *gateState) closeLease(ctx context.Context) error {
	g.mu.Lock()
	start := g.attempts
	g.mu.Unlock()
	var kicked time.Time
	for {
		g.mu.Lock()
		closed, failure, down, tried, ch := g.leaseClosed, g.failure, g.unreachable, g.attempts > start, g.synced
		g.mu.Unlock()
		switch {
		case closed:
			return nil
		case tried && failure != nil && !down:
			return failure
		}
		if wait := limits.SyncInterval - time.Since(kicked); wait > 0 {
			select {
			case <-ctx.Done():
				return waitErr(ctx)
			case <-time.After(wait):
			}
		}
		g.poke()
		kicked = time.Now()
		select {
		case <-ctx.Done():
			return waitErr(ctx)
		case <-ch:
		}
	}
}

// limits is the customer's lines from the gate's views, or a read when it
// has none.
func (g *gateState) limits(ctx context.Context, customer billing.CustomerID) ([]billing.CustomerLimit, error) {
	g.vmu.RLock()
	var views []*custView
	for k, v := range g.views {
		if k.customer == customer {
			views = append(views, v)
		}
	}
	g.vmu.RUnlock()
	if len(views) == 0 {
		page, err := g.c.ListCustomerLimits(ctx, billing.CustomerLimitListParams{CustomerID: customer})
		if errors.Is(err, billing.ErrUnreachable) {
			return nil, unavailable(err)
		}
		if err != nil {
			return nil, err
		}
		return page.Items, nil
	}
	var out []billing.CustomerLimit
	for _, v := range views {
		out = append(out, v.display()...)
	}
	slices.SortFunc(out, func(a, b billing.CustomerLimit) int {
		if a.Limit != b.Limit {
			if a.Limit < b.Limit {
				return -1
			}
			return 1
		}
		if a.Unit < b.Unit {
			return -1
		}
		if a.Unit > b.Unit {
			return 1
		}
		return 0
	})
	return out, nil
}
