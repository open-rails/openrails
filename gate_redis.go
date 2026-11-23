package openrails

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/modules/limits"
)

// A shared gate keeps one lease for the merchant's processes in Redis. The
// group's keys share one hash slot ({...:<ns>}); each hold's keys share
// their own ({...:<ns>:<hold>}), so a decision is one script on one slot.
//
//	G:lease    hash  id, valid (unix ms the lease lives until), n:<lease>
//	G:u:<id>   stream of a lease's unsettled usage, numbered n
//	G:leases   set   leases with a usage stream
//	G:lock     the syncing process, renewed while it syncs
//	G:kick     list  wakes the syncing process for an early sync
//	G:wants    hash  attrs -> want for the next sync
//	G:answers:<gen>  hash  attrs -> that sync's answer (expires)
//	G:sets     hash  attrs -> limit set
//	G:gen      completed sync attempts
//	G:state    hash  ok, err (unix ms), cause
//	G:holds    hash  hold -> {customer, set}
//	G:returns  set   holds to return
//	G:procs    hash  process -> heartbeat (unix ms)
//	G:limits:<customer>  display lines
//	G:unsent:<customer>  hash  limit|unit -> unsent cost
//	H          hash  lease, ver, p (pricing), safe, safe_spent, used,
//	                 ev:<event type>, h:<part> held, o:<part> taken by open
//	                 tickets, u:<part> captured and unsettled, e:<part> end
//	H:t        hash  ticket -> o|<process>|<part>=<cost>;... or c|<n>|...
const (
	gateKeyPrefix = "openrails:gate:v1:"
	// gateLockTTL is how long the syncing process keeps the group's lock
	// without renewing it; past it another process syncs.
	gateLockTTL = 3 * limits.SyncInterval
	// gateProcTTL is when a silent process's open tickets are reclaimed.
	gateProcTTL = 5 * limits.SyncInterval
	// gateRedisDown is how long decisions use the process's own lease after
	// Redis failed.
	gateRedisDown = limits.SyncInterval
	// gateReclaimEvery is how many syncs pass between reclaims.
	gateReclaimEvery = 10
	gateDisplayTTL   = time.Minute
	gatePoll         = 5 * time.Millisecond
)

type sharedGate struct {
	c        *Client
	rdb      redis.UniversalClient
	fallback *localGate
	proc     string

	nsOnce sync.Mutex
	ns     string

	downUntil atomic.Int64

	mu      sync.Mutex
	lease   string
	valid   int64
	failure error
	failAt  time.Time
	sets    map[string]string
	prices  map[string]*sharedPricing
	gen     int64
	genCh   chan struct{}
	syncs   int

	waiting atomic.Int32

	ctx    context.Context
	cancel context.CancelFunc
	stop   chan struct{}
	done   chan struct{}
}

type sharedPricing struct {
	lease   string
	ver     string
	pricing *holdPricing
}

func newSharedGate(c *Client, rdb redis.UniversalClient, fallback *localGate) *sharedGate {
	ctx, cancel := context.WithCancel(context.Background())
	return &sharedGate{c: c, rdb: rdb, fallback: fallback, proc: uuid.NewString(),
		sets: map[string]string{}, prices: map[string]*sharedPricing{}, genCh: make(chan struct{}),
		ctx: ctx, cancel: cancel, stop: make(chan struct{}), done: make(chan struct{})}
}

// namespace is the group's key namespace: the server and merchant. A handle
// that has not yet learned its merchant has none, and decides from its own
// lease meanwhile.
func (s *sharedGate) namespace() (string, bool) {
	s.nsOnce.Lock()
	defer s.nsOnce.Unlock()
	if s.ns != "" {
		return s.ns, true
	}
	mid := s.c.MerchantID()
	if mid.IsZero() {
		return "", false
	}
	sum := sha256.Sum256([]byte(s.c.baseURL + "\x00" + mid.String()))
	s.ns = hex.EncodeToString(sum[:10])
	return s.ns, true
}

func (s *sharedGate) g(ns, suffix string) string { return gateKeyPrefix + "{" + ns + "}:" + suffix }

func holdID(customer billing.CustomerID, set string) string {
	sum := sha256.Sum256([]byte(customer.String() + "\x00" + set))
	return hex.EncodeToString(sum[:12])
}

func (s *sharedGate) h(ns, hold string) string { return gateKeyPrefix + "{" + ns + ":" + hold + "}" }

func (s *sharedGate) down() bool { return time.Now().UnixNano() < s.downUntil.Load() }

func (s *sharedGate) markDown() { s.downUntil.Store(time.Now().Add(gateRedisDown).UnixNano()) }

// redisFailed reports an error of Redis itself (not a miss), marking Redis
// down.
func (s *sharedGate) redisFailed(err error) bool {
	if err == nil || errors.Is(err, redis.Nil) {
		return false
	}
	s.markDown()
	return true
}

// errRedis is Redis failing a decision: the process decides from its own
// lease instead.
var errRedis = errors.New("openrails: gate: Redis unavailable")

func (s *sharedGate) start() { go s.run() }

func (s *sharedGate) admit(ctx context.Context, gate *Gate, p billing.AdmitParams) (*Ticket, error) {
	ns, ok := s.namespace()
	if !ok || s.down() {
		return s.fallback.admit(ctx, gate, p)
	}
	t, err := s.admitShared(ctx, gate, ns, p)
	if errors.Is(err, errRedis) {
		return s.fallback.admit(ctx, gate, p)
	}
	return t, err
}

func (s *sharedGate) admitShared(ctx context.Context, gate *Gate, ns string, p billing.AdmitParams) (*Ticket, error) {
	r := admitRequest(p)
	attrs := attrsKey(p)
	answered := false
	for round := 0; ; round++ {
		set, ok, err := s.setFor(ctx, ns, attrs)
		if err != nil {
			return nil, err
		}
		if ok {
			t, out, err := s.decide(ctx, gate, ns, p, r, set, false)
			switch {
			case err != nil:
				return nil, err
			case out == decideAllowed:
				return t, nil
			case out == decideUnpriced && answered:
				return nil, errNotPriced(eventTypeOf(p))
			}
		}
		if round >= maxColdRounds {
			return refusedTicket(gate, p, capacityHeld(1)), nil
		}
		if failure := s.recentFailure(); failure != nil {
			return s.safe(ctx, gate, ns, p, r, attrs, failure)
		}
		a, err := s.await(ctx, ns, attrs, p)
		if err != nil {
			return nil, err
		}
		switch {
		case a.retry:
		case a.unavailable != nil:
			return s.safe(ctx, gate, ns, p, r, attrs, a.unavailable)
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
			s.forget(holdID(p.CustomerID, a.limitSet))
		}
	}
}

func (s *sharedGate) safe(ctx context.Context, gate *Gate, ns string, p billing.AdmitParams, r request, attrs string, failure error) (*Ticket, error) {
	set, ok, err := s.setFor(ctx, ns, attrs)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, unavailable(failure)
	}
	t, out, err := s.decide(ctx, gate, ns, p, r, set, true)
	if err != nil {
		return nil, err
	}
	if out != decideAllowed {
		return nil, unavailable(failure)
	}
	return t, nil
}

func (s *sharedGate) recentFailure() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failure != nil && time.Since(s.failAt) < limits.SyncInterval {
		return s.failure
	}
	return nil
}

func (s *sharedGate) setFor(ctx context.Context, ns, attrs string) (string, bool, error) {
	s.mu.Lock()
	set, ok := s.sets[attrs]
	s.mu.Unlock()
	if ok {
		return set, true, nil
	}
	set, err := s.rdb.HGet(ctx, s.g(ns, "sets"), attrs).Result()
	if errors.Is(err, redis.Nil) {
		return "", false, nil
	}
	if s.redisFailed(err) {
		return "", false, errRedis
	}
	s.mu.Lock()
	if len(s.sets) >= maxGateLimitSets {
		s.sets = map[string]string{}
	}
	s.sets[attrs] = set
	s.mu.Unlock()
	return set, true, nil
}

func (s *sharedGate) forget(hold string) {
	s.mu.Lock()
	delete(s.prices, hold)
	s.mu.Unlock()
}

// pricedHold is the hold's pricing as this process cached it; nil when the
// hold is not in Redis.
func (s *sharedGate) pricedHold(ctx context.Context, ns, hold string) (*sharedPricing, error) {
	s.mu.Lock()
	sp := s.prices[hold]
	s.mu.Unlock()
	if sp != nil {
		return sp, nil
	}
	fields, err := s.rdb.HGetAll(ctx, s.h(ns, hold)).Result()
	if s.redisFailed(err) {
		return nil, errRedis
	}
	if len(fields) == 0 || fields["p"] == "" {
		return nil, nil
	}
	var stored storedPricing
	if err := json.Unmarshal([]byte(fields["p"]), &stored); err != nil {
		return nil, fmt.Errorf("openrails: gate: hold pricing in Redis: %w", err)
	}
	events := map[string]bool{}
	for f := range fields {
		if e, ok := strings.CutPrefix(f, "ev:"); ok {
			events[e] = true
		}
	}
	sp = &sharedPricing{lease: fields["lease"], ver: fields["ver"], pricing: newHoldPricing(stored.Hold, events)}
	s.mu.Lock()
	if len(s.prices) >= maxGateHolds {
		s.prices = map[string]*sharedPricing{}
	}
	s.prices[hold] = sp
	s.mu.Unlock()
	return sp, nil
}

type storedPricing struct {
	Hold billing.Hold `json:"hold"`
}

func (s *sharedGate) leaseState() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lease, s.lease != "" && time.Now().UnixMilli() < s.valid
}

// decide is one decision on the group's hold: one script that takes the
// cost from every limit or from none.
func (s *sharedGate) decide(ctx context.Context, gate *Gate, ns string, p billing.AdmitParams, r request, set string, withSafe bool) (*Ticket, decision, error) {
	hold := holdID(p.CustomerID, set)
	lease, live := s.leaseState()
	for attempt := 0; attempt < 2; attempt++ {
		sp, err := s.pricedHold(ctx, ns, hold)
		if err != nil {
			return nil, decideCold, err
		}
		holdLease, holdLive := lease, live
		if withSafe && sp != nil && sp.lease != lease && sp.lease != "" {
			holdLease, holdLive = sp.lease, false
		}
		if sp == nil || sp.lease != holdLease || holdLease == "" {
			return nil, decideCold, nil
		}
		takes, money, err := sp.pricing.cost(time.Now(), r, withSafe)
		if errors.Is(err, errUnpriced) {
			return nil, decideUnpriced, nil
		}
		if err != nil {
			return nil, decideCold, err
		}
		ticket := uuid.NewString()
		args := []any{holdLease, sp.ver, ticket, s.proc, boolArg(holdLive), "", len(takes)}
		if withSafe {
			args[5] = strconv.FormatInt(money, 10)
		}
		for _, t := range takes {
			mode := "0"
			if t.overrun == "admit_while_positive" {
				mode = "1"
			}
			args = append(args, t.part, strconv.FormatInt(t.cost, 10), mode)
		}
		res, err := takeScript.Run(ctx, s.rdb, []string{s.h(ns, hold), s.h(ns, hold) + ":t"}, args...).Slice()
		if s.redisFailed(err) {
			return nil, decideCold, errRedis
		}
		if err != nil || len(res) == 0 {
			return nil, decideCold, fmt.Errorf("openrails: gate: take script: %v", err)
		}
		switch code, _ := res[0].(int64); code {
		case 1:
			for i := range takes {
				takes[i].cost, _ = strconv.ParseInt(fmt.Sprint(res[2+i]), 10, 64)
			}
			if low, _ := res[1].(int64); low == 1 && !withSafe {
				go s.prefetch(ns, attrsKey(p), p)
			}
			return &Ticket{gate: gate, params: p, limitSet: set, shared: &sharedTicket{s: s, ns: ns, hold: hold, lease: holdLease, id: ticket, takes: takes, pricing: sp.pricing}}, decideAllowed, nil
		case -1:
			s.forget(hold)
			continue
		case -3:
			return nil, decideShort, nil
		default:
			s.forget(hold)
			return nil, decideCold, nil
		}
	}
	return nil, decideCold, nil
}

func boolArg(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

type sharedTicket struct {
	s       *sharedGate
	ns      string
	hold    string
	lease   string
	id      string
	takes   []take
	pricing *holdPricing
}

func encodeTakes(takes []take) string {
	parts := make([]string, len(takes))
	for i, t := range takes {
		parts[i] = t.part + "=" + strconv.FormatInt(t.cost, 10)
	}
	return strings.Join(parts, ";")
}

func unsentArg(currency string, takes []take) string {
	unsent := map[string]int64{}
	for _, t := range takes {
		key := partLimit(t.part) + "|"
		if t.money {
			key += currency
		}
		unsent[key] += t.cost
	}
	parts := make([]string, 0, len(unsent))
	for k, c := range unsent {
		parts = append(parts, k+"="+strconv.FormatInt(c, 10))
	}
	slices.Sort(parts)
	return strings.Join(parts, ";")
}

// capture numbers the usage in the lease's stream, then turns the ticket's
// take into its cost. A process that cannot reach Redis reports it on its
// own lease, with no hold.
func (st *sharedTicket) capture(ctx context.Context, t *Ticket, u billing.CaptureUsage) error {
	s := st.s
	item := captureItem(t.params, &t.limitSet, u, time.Now())
	r := request{usage: &limits.Usage{EventType: item.EventType, Dimensions: item.Dimensions, Metadata: item.Metadata}, stated: item.Amount}
	actual, err := st.pricing.costOf(r, st.takes)
	if errors.Is(err, errUnpriced) {
		actual, err = slices.Clone(st.takes), nil
	}
	if err != nil {
		return notCaptured{err}
	}
	body, err := json.Marshal(item)
	if err != nil {
		return notCaptured{err}
	}
	n, err := usageScript.Run(ctx, s.rdb, []string{s.g(st.ns, "lease"), s.g(st.ns, "u:"+st.lease), s.g(st.ns, "leases"), s.g(st.ns, "unsent:"+item.CustomerID.String())},
		st.lease, body, st.hold, st.id, unsentArg(item.Currency, actual)).Int64()
	if s.redisFailed(err) || err != nil {
		item.LimitSet = nil
		s.fallback.ensureStarted()
		return s.fallback.enqueue(ctx, billing.LeaseID{}, item, nil, nil, nil)
	}
	if err := captureScript.Run(ctx, s.rdb, []string{s.h(st.ns, st.hold), s.h(st.ns, st.hold) + ":t"}, st.id, n, encodeTakes(actual)).Err(); err != nil {
		s.redisFailed(err)
	}
	return nil
}

func (st *sharedTicket) release(ctx context.Context) error {
	s := st.s
	err := releaseScript.Run(ctx, s.rdb, []string{s.h(st.ns, st.hold), s.h(st.ns, st.hold) + ":t"}, st.id).Err()
	if s.redisFailed(err) {
		return fmt.Errorf("%w: %w", errRedis, err)
	}
	return nil
}

func (s *sharedGate) record(ctx context.Context, item billing.LeaseUsage) error {
	ns, ok := s.namespace()
	lease, _ := s.leaseState()
	if !ok || s.down() || lease == "" {
		return s.fallback.record(ctx, item)
	}
	body, err := json.Marshal(item)
	if err != nil {
		return err
	}
	err = usageScript.Run(ctx, s.rdb, []string{s.g(ns, "lease"), s.g(ns, "u:"+lease), s.g(ns, "leases"), s.g(ns, "unsent:"+item.CustomerID.String())},
		lease, body, "", "", "").Err()
	if s.redisFailed(err) || err != nil {
		return s.fallback.record(ctx, item)
	}
	return nil
}

// storedWant is a want in Redis; storedAnswer a sync's answer to it.
type storedWant struct {
	Want      billing.LeaseWant `json:"want"`
	EventType string            `json:"event_type"`
}

type storedAnswer struct {
	Gen         int64                `json:"gen"`
	LimitSet    string               `json:"limit_set,omitempty"`
	Refusal     *billing.Refusal     `json:"refusal,omitempty"`
	Error       *billing.StatusError `json:"error,omitempty"`
	Unavailable string               `json:"unavailable,omitempty"`
	Retry       bool                 `json:"retry,omitempty"`
}

func (a storedAnswer) answer() wantAnswer {
	switch {
	case a.Retry:
		return wantAnswer{retry: true}
	case a.Unavailable != "":
		return wantAnswer{unavailable: fmt.Errorf("%w: %s", billing.ErrUnreachable, a.Unavailable)}
	case a.Error != nil:
		return wantAnswer{err: a.Error}
	case a.Refusal != nil:
		return wantAnswer{limitSet: a.LimitSet, refusal: a.Refusal}
	}
	return wantAnswer{limitSet: a.LimitSet}
}

// register adds a want to the group's next sync and wakes the syncing
// process: the sync generation it waits past.
func (s *sharedGate) register(ctx context.Context, ns, attrs string, p billing.AdmitParams) (int64, error) {
	body, err := json.Marshal(storedWant{Want: leaseWant(p), EventType: eventTypeOf(p)})
	if err != nil {
		return 0, err
	}
	gen, err := wantScript.Run(ctx, s.rdb, []string{s.g(ns, "wants"), s.g(ns, "kick"), s.g(ns, "gen")}, attrs, body).Int64()
	if s.redisFailed(err) {
		return 0, errRedis
	}
	return gen, err
}

func (s *sharedGate) prefetch(ns, attrs string, p billing.AdmitParams) {
	ctx, cancel := context.WithTimeout(context.Background(), syncTimeout)
	defer cancel()
	_, _ = s.register(ctx, ns, attrs, p)
}

// await waits for the first sync past the want's registration that answers
// it.
func (s *sharedGate) await(ctx context.Context, ns, attrs string, p billing.AdmitParams) (wantAnswer, error) {
	gen0, err := s.register(ctx, ns, attrs, p)
	if err != nil {
		return wantAnswer{}, err
	}
	s.waiting.Add(1)
	defer s.waiting.Add(-1)
	seen := gen0
	for {
		ch, gen := s.watch()
		for g := max(seen+1, gen-10); g <= gen; g++ {
			raw, err := s.rdb.HGet(ctx, s.g(ns, "answers:"+strconv.FormatInt(g, 10)), attrs).Result()
			if s.redisFailed(err) {
				return wantAnswer{}, errRedis
			}
			var a storedAnswer
			if err == nil && json.Unmarshal([]byte(raw), &a) == nil && a.Gen > gen0 {
				s.mu.Lock()
				if a.LimitSet != "" {
					s.sets[attrs] = a.LimitSet
				}
				s.mu.Unlock()
				if err := s.refreshLease(ctx, ns); err != nil {
					return wantAnswer{}, err
				}
				return a.answer(), nil
			}
		}
		seen = max(seen, gen)
		select {
		case <-ctx.Done():
			return wantAnswer{}, ctx.Err()
		case <-ch:
		}
	}
}

// refreshLease reads the group's lease, which a sync may have just
// started or renewed.
func (s *sharedGate) refreshLease(ctx context.Context, ns string) error {
	v, err := s.rdb.HMGet(ctx, s.g(ns, "lease"), "id", "valid").Result()
	if s.redisFailed(err) {
		return errRedis
	}
	if len(v) == 2 {
		s.mu.Lock()
		s.lease, _ = v[0].(string)
		s.valid, _ = strconv.ParseInt(fmt.Sprint(v[1]), 10, 64)
		s.mu.Unlock()
	}
	return nil
}

// watch is the generation this process last saw and a channel closed when
// it advances; the run loop polls it while anyone waits.
func (s *sharedGate) watch() (chan struct{}, int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.genCh, s.gen
}

func (s *sharedGate) observe(gen int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if gen > s.gen {
		s.gen = gen
		close(s.genCh)
		s.genCh = make(chan struct{})
	}
}

func (s *sharedGate) run() {
	defer close(s.done)
	ctx := s.ctx
	go s.pollGen(ctx)
	holder := false
	var last time.Time
	for {
		select {
		case <-s.stop:
			if holder {
				ns, _ := s.namespace()
				c, done := context.WithTimeout(context.Background(), syncTimeout)
				_ = unlockScript.Run(c, s.rdb, []string{s.g(ns, "lock")}, s.proc).Err()
				done()
			}
			return
		default:
		}
		ns, ok := s.namespace()
		if !ok {
			if !s.sleep(limits.SyncInterval) {
				return
			}
			continue
		}
		c, done := context.WithTimeout(ctx, syncTimeout)
		var err error
		holder, err = s.heartbeat(c, ns, holder)
		done()
		if s.redisFailed(err) || !holder {
			if !s.sleep(limits.SyncInterval) {
				return
			}
			continue
		}
		if time.Since(last) < limits.SyncInterval {
			_, err := s.rdb.BLPop(ctx, limits.SyncInterval, s.g(ns, "kick")).Result()
			if ctx.Err() != nil {
				continue
			}
			if s.redisFailed(err) {
				continue
			}
		}
		last = time.Now()
		c, done = context.WithTimeout(ctx, 2*syncTimeout)
		renewed := make(chan struct{})
		go s.keepLock(c, ns, renewed)
		if err := s.syncGroup(c, ns); err != nil {
			s.redisFailed(err)
		}
		done()
		<-renewed
	}
}

// keepLock renews the group's lock while a sync runs.
func (s *sharedGate) keepLock(ctx context.Context, ns string, done chan struct{}) {
	defer close(done)
	t := time.NewTicker(limits.SyncInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = renewScript.Run(ctx, s.rdb, []string{s.g(ns, "lock")}, s.proc, gateLockTTL.Milliseconds()).Err()
		}
	}
}

func (s *sharedGate) sleep(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-s.stop:
		return false
	case <-t.C:
		return true
	}
}

// pollGen follows the group's sync generation for this process's waiters.
func (s *sharedGate) pollGen(ctx context.Context) {
	t := time.NewTicker(gatePoll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		ns, ok := s.namespace()
		if !ok || s.down() || s.waiting.Load() == 0 {
			continue
		}
		gen, err := s.rdb.Get(ctx, s.g(ns, "gen")).Int64()
		if err == nil {
			s.observe(gen)
		}
	}
}

// heartbeat records the process, reads the lease and the last sync's state,
// and takes or renews the group's lock.
func (s *sharedGate) heartbeat(ctx context.Context, ns string, holder bool) (bool, error) {
	now := time.Now().UnixMilli()
	var lease, state *redis.SliceCmd
	_, err := s.rdb.Pipelined(ctx, func(p redis.Pipeliner) error {
		p.HSet(ctx, s.g(ns, "procs"), s.proc, now)
		lease = p.HMGet(ctx, s.g(ns, "lease"), "id", "valid")
		state = p.HMGet(ctx, s.g(ns, "state"), "ok", "err", "cause")
		return nil
	})
	if err != nil && !errors.Is(err, redis.Nil) {
		return false, err
	}
	s.mu.Lock()
	if v := lease.Val(); len(v) == 2 {
		s.lease, _ = v[0].(string)
		valid, _ := v[1].(string)
		s.valid, _ = strconv.ParseInt(valid, 10, 64)
	}
	if v := state.Val(); len(v) == 3 {
		okAt, _ := strconv.ParseInt(fmt.Sprint(v[0]), 10, 64)
		errAt, _ := strconv.ParseInt(fmt.Sprint(v[1]), 10, 64)
		if errAt > okAt {
			s.failure, s.failAt = fmt.Errorf("%w: %v", billing.ErrUnreachable, v[2]), time.UnixMilli(errAt)
		} else {
			s.failure = nil
		}
	}
	s.mu.Unlock()
	ttl := gateLockTTL.Milliseconds()
	if holder {
		kept, err := renewScript.Run(ctx, s.rdb, []string{s.g(ns, "lock")}, s.proc, ttl).Int64()
		if err == nil && kept == 1 {
			return true, nil
		}
		if err != nil {
			return false, err
		}
	}
	return s.rdb.SetNX(ctx, s.g(ns, "lock"), s.proc, gateLockTTL).Result()
}

// usageEntry is one stream entry of a lease's usage.
type usageEntry struct {
	id     string
	item   billing.LeaseUsage
	hold   string
	ticket string
	unsent string
}

func (s *sharedGate) readUsage(ctx context.Context, ns, lease string) ([]usageEntry, error) {
	msgs, err := s.rdb.XRangeN(ctx, s.g(ns, "u:"+lease), "-", "+", billing.MaxSyncUsageItems).Result()
	if err != nil {
		return nil, err
	}
	out := make([]usageEntry, 0, len(msgs))
	for _, m := range msgs {
		e := usageEntry{id: m.ID}
		body, _ := m.Values["item"].(string)
		if err := json.Unmarshal([]byte(body), &e.item); err != nil {
			continue
		}
		e.item.N, _ = strconv.ParseInt(fmt.Sprint(m.Values["n"]), 10, 64)
		e.hold, _ = m.Values["hold"].(string)
		e.ticket, _ = m.Values["ticket"].(string)
		e.unsent, _ = m.Values["unsent"].(string)
		out = append(out, e)
	}
	return out, nil
}

type holdRef struct {
	Customer billing.CustomerID `json:"customer"`
	Set      string             `json:"set"`
}

// syncGroup is one sync of the group's lease, by the process holding the
// lock, then the usage of the group's earlier leases.
func (s *sharedGate) syncGroup(ctx context.Context, ns string) error {
	now := time.Now()
	info, err := s.rdb.HMGet(ctx, s.g(ns, "lease"), "id", "valid").Result()
	if err != nil {
		return err
	}
	lease, _ := info[0].(string)
	valid, _ := strconv.ParseInt(fmt.Sprint(info[1]), 10, 64)
	if lease == "" || (valid != 0 && now.UnixMilli() >= valid) {
		lease = billing.LeaseID(uuid.New()).String()
		if err := s.rdb.HSet(ctx, s.g(ns, "lease"), "id", lease, "valid", 0).Err(); err != nil {
			return err
		}
	}
	leaseID, err := billing.ParseLeaseID(lease)
	if err != nil {
		return err
	}
	if err := s.evict(ctx, ns); err != nil {
		return err
	}
	entries, err := s.readUsage(ctx, ns, lease)
	if err != nil {
		return err
	}
	rawWants, err := s.rdb.HGetAll(ctx, s.g(ns, "wants")).Result()
	if err != nil {
		return err
	}
	retIDs, err := s.rdb.SRandMemberN(ctx, s.g(ns, "returns"), billing.MaxSyncReturns).Result()
	if err != nil {
		return err
	}
	returns := []billing.HoldReturn{}
	returned := map[string]bool{}
	if len(retIDs) > 0 {
		refs, err := s.rdb.HMGet(ctx, s.g(ns, "holds"), retIDs...).Result()
		if err != nil {
			return err
		}
		for i, raw := range refs {
			var ref holdRef
			if str, ok := raw.(string); ok && json.Unmarshal([]byte(str), &ref) == nil {
				returns = append(returns, billing.HoldReturn{CustomerID: ref.Customer, LimitSet: ref.Set})
				returned[retIDs[i]] = true
			}
		}
	}
	type sentWant struct {
		attrs string
		want  storedWant
	}
	held := map[string]string{}
	if len(returned) > 0 && len(rawWants) > 0 {
		attrs := make([]string, 0, len(rawWants))
		for a := range rawWants {
			attrs = append(attrs, a)
		}
		sets, err := s.rdb.HMGet(ctx, s.g(ns, "sets"), attrs...).Result()
		if err != nil {
			return err
		}
		for i, v := range sets {
			if set, ok := v.(string); ok {
				held[attrs[i]] = set
			}
		}
	}
	var sent []sentWant
	for attrs, raw := range rawWants {
		if len(sent) == billing.MaxSyncWants {
			break
		}
		var w storedWant
		if json.Unmarshal([]byte(raw), &w) != nil {
			continue
		}
		if set, ok := held[attrs]; ok && returned[holdID(w.Want.CustomerID, set)] {
			continue
		}
		sent = append(sent, sentWant{attrs, w})
	}
	committed, err := s.commitments(ctx, ns, lease)
	if err != nil {
		return err
	}
	params := billing.SyncLeasesParams{LeaseID: leaseID, Usage: []billing.LeaseUsage{}, Wants: make([]billing.LeaseWant, len(sent)), Returns: returns, Committed: committed}
	for _, e := range entries {
		params.Usage = append(params.Usage, e.item)
	}
	for i, w := range sent {
		params.Wants[i] = w.want.Want
	}
	if len(sent) > 0 {
		attrs := make([]string, len(sent))
		for i, w := range sent {
			attrs[i] = w.attrs
		}
		if err := s.rdb.HDel(ctx, s.g(ns, "wants"), attrs...).Err(); err != nil {
			return err
		}
	}
	gen, err := s.rdb.Get(ctx, s.g(ns, "gen")).Int64()
	if err != nil && !errors.Is(err, redis.Nil) {
		return err
	}
	gen++
	sentAt := time.Now()
	call, cancel := context.WithTimeout(ctx, syncTimeout)
	res, syncErr := s.c.SyncLeases(call, params)
	cancel()
	if syncErr == nil && len(res.Wants) != len(sent) {
		syncErr = fmt.Errorf("%w: a sync answered %d wants for %d", billing.ErrUnreachable, len(res.Wants), len(sent))
	}

	pipe := s.rdb.Pipeline()
	answers := map[string]any{}
	if syncErr != nil {
		var se *billing.StatusError
		status := errors.As(syncErr, &se) && !errors.Is(syncErr, billing.ErrUnreachable)
		for _, w := range sent {
			a := storedAnswer{Gen: gen}
			switch {
			case status && se.Code == billing.CodeLeaseNotHeld:
				a.Retry = true
			case status:
				a.Error = se
			default:
				a.Unavailable = syncErr.Error()
			}
			answers[w.attrs] = mustJSON(a)
		}
		switch {
		case status && se.Code == billing.CodeLeaseNotHeld:
			pipe.HSet(ctx, s.g(ns, "lease"), "valid", 1)
		case status && len(entries) > 0 && (se.Code == billing.CodeUsageOutOfOrder || se.Code == billing.CodeIdempotencyKeyReused || strings.HasPrefix(deref(se.Param), "usage")):
			s.dropEntries(ctx, pipe, ns, lease, entries)
		case !status:
			pipe.HSet(ctx, s.g(ns, "state"), "err", time.Now().UnixMilli(), "cause", syncErr.Error())
		}
	} else {
		pipe.HSet(ctx, s.g(ns, "state"), "ok", time.Now().UnixMilli())
		pipe.HSet(ctx, s.g(ns, "lease"), "id", lease, "valid", sentAt.Add(limits.LeaseLifetime).UnixMilli())
		if err := s.applyAnswer(ctx, pipe, ns, lease, entries, res, returned); err != nil {
			return err
		}
		for i, wr := range res.Wants {
			a := storedAnswer{Gen: gen}
			switch {
			case wr.Error != nil && wr.Error.Code == billing.CodeLeaseNotHeld:
				a.Retry = true
				pipe.HSet(ctx, s.g(ns, "lease"), "valid", 1)
			case wr.Error != nil:
				a.Error = &billing.StatusError{Status: wr.Status, ErrorDetails: *wr.Error}
			case wr.LimitSet == nil:
				a.Unavailable = "a want was answered with no limit set"
			default:
				a.LimitSet, a.Refusal = *wr.LimitSet, wr.Refusal
				pipe.HSet(ctx, s.g(ns, "sets"), sent[i].attrs, *wr.LimitSet)
				if wr.Refusal == nil {
					s.ensureHold(ctx, pipe, ns, lease, sent[i].want, *wr.LimitSet)
				}
			}
			answers[sent[i].attrs] = mustJSON(a)
		}
	}
	if len(answers) > 0 {
		key := s.g(ns, "answers:"+strconv.FormatInt(gen, 10))
		pipe.HSet(ctx, key, answers)
		pipe.Expire(ctx, key, gateDisplayTTL)
	}
	pipe.Set(ctx, s.g(ns, "gen"), gen, 0)
	if _, err := pipe.Exec(ctx); err != nil {
		return err
	}
	s.observe(gen)
	s.mu.Lock()
	s.syncs++
	reclaim := s.syncs%gateReclaimEvery == 0
	s.mu.Unlock()
	if reclaim {
		if err := s.reclaim(ctx, ns); err != nil {
			return err
		}
		if n, err := s.rdb.HLen(ctx, s.g(ns, "sets")).Result(); err == nil && n > maxGateLimitSets {
			_ = s.rdb.Del(ctx, s.g(ns, "sets")).Err()
		}
	}
	return s.resendEarlier(ctx, ns, lease)
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// applyAnswer settles a sync's usage and applies its holds, display lines
// and returns.
func (s *sharedGate) applyAnswer(ctx context.Context, pipe redis.Pipeliner, ns, lease string, entries []usageEntry, res *billing.SyncLeasesResult, returned map[string]bool) error {
	settled := map[string][]string{}
	var ids []string
	for _, e := range entries {
		if e.item.N > res.Usage.ThroughN {
			break
		}
		ids = append(ids, e.id)
		if e.hold != "" && e.ticket != "" {
			settled[e.hold] = append(settled[e.hold], e.ticket)
		}
		s.unsentLess(ctx, pipe, ns, e)
	}
	for _, f := range res.Usage.Refused {
		pipe.HSet(ctx, s.g(ns, "state"), "refused", fmt.Sprintf("usage %d refused with status %d", f.N, f.Status))
	}
	if len(ids) > 0 {
		pipe.XDel(ctx, s.g(ns, "u:"+lease), ids...)
	}
	for hold := range returned {
		pipe.Del(ctx, s.h(ns, hold), s.h(ns, hold)+":t")
		pipe.HDel(ctx, s.g(ns, "holds"), hold)
		pipe.SRem(ctx, s.g(ns, "returns"), hold)
	}
	for _, b := range res.Holds {
		hold := holdID(b.CustomerID, b.LimitSet)
		body := mustJSON(storedPricing{Hold: b})
		safe := ""
		if b.Safe != nil {
			safe = strconv.FormatInt(*b.Safe, 10)
		}
		args := []any{lease, "1", body, safe, len(b.Limits)}
		for _, l := range b.Limits {
			end := ""
			if l.WindowEndsAt != nil {
				end = strconv.FormatInt(l.WindowEndsAt.UnixMilli(), 10)
			}
			args = append(args, partID(l.Limit, l.WindowStartsAt), strconv.FormatInt(l.Held, 10), end)
		}
		args = append(args, len(settled[hold]))
		for _, t := range settled[hold] {
			args = append(args, t)
		}
		args = append(args, 0)
		settleScript.Eval(ctx, pipe, []string{s.h(ns, hold), s.h(ns, hold) + ":t"}, args...)
		pipe.HSet(ctx, s.g(ns, "holds"), hold, mustJSON(holdRef{Customer: b.CustomerID, Set: b.LimitSet}))
		delete(settled, hold)
	}
	for hold, tickets := range settled {
		args := []any{lease, "0", "", "", 0, len(tickets)}
		for _, t := range tickets {
			args = append(args, t)
		}
		args = append(args, 0)
		settleScript.Eval(ctx, pipe, []string{s.h(ns, hold), s.h(ns, hold) + ":t"}, args...)
	}
	display := map[billing.CustomerID][]billing.CustomerLimit{}
	for _, line := range res.Limits {
		display[line.CustomerID] = append(display[line.CustomerID], line)
	}
	for customer, lines := range display {
		pipe.Set(ctx, s.g(ns, "limits:"+customer.String()), mustJSON(lines), gateDisplayTTL)
	}
	return nil
}

// ensureHold makes the hold an answered want names known to the group:
// one the server did not list holds nothing, and the want's event type is
// one the server priced.
func (s *sharedGate) ensureHold(ctx context.Context, pipe redis.Pipeliner, ns, lease string, w storedWant, set string) {
	hold := holdID(w.Want.CustomerID, set)
	empty := mustJSON(storedPricing{Hold: billing.Hold{CustomerID: w.Want.CustomerID, LimitSet: set, Currency: w.Want.Currency, Limits: []billing.HoldLimit{}}})
	args := []any{lease, "2", empty, "", 0, 0}
	if w.EventType != "" {
		args = append(args, 1, w.EventType)
	} else {
		args = append(args, 0)
	}
	settleScript.Eval(ctx, pipe, []string{s.h(ns, hold), s.h(ns, hold) + ":t"}, args...)
	pipe.HSetNX(ctx, s.g(ns, "holds"), hold, mustJSON(holdRef{Customer: w.Want.CustomerID, Set: set}))
}

func (s *sharedGate) unsentLess(ctx context.Context, pipe redis.Pipeliner, ns string, e usageEntry) {
	if e.unsent == "" {
		return
	}
	key := s.g(ns, "unsent:"+e.item.CustomerID.String())
	for _, kv := range strings.Split(e.unsent, ";") {
		k, v, ok := strings.Cut(kv, "=")
		if c, err := strconv.ParseInt(v, 10, 64); ok && err == nil {
			pipe.HIncrBy(ctx, key, k, -c)
		}
	}
}

// dropEntries forgets usage the server refused for good.
func (s *sharedGate) dropEntries(ctx context.Context, pipe redis.Pipeliner, ns, lease string, entries []usageEntry) {
	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		ids = append(ids, e.id)
		s.unsentLess(ctx, pipe, ns, e)
		if e.hold != "" && e.ticket != "" {
			settleScript.Eval(ctx, pipe, []string{s.h(ns, e.hold), s.h(ns, e.hold) + ":t"}, lease, "3", "", "", 0, 1, e.ticket, 0)
		}
	}
	pipe.XDel(ctx, s.g(ns, "u:"+lease), ids...)
	pipe.HSet(ctx, s.g(ns, "state"), "refused", fmt.Sprintf("usage %d..%d of lease %s refused", entries[0].item.N, entries[len(entries)-1].item.N, lease))
}

// resendEarlier sends the usage of the group's earlier leases.
func (s *sharedGate) resendEarlier(ctx context.Context, ns, current string) error {
	leases, err := s.rdb.SMembers(ctx, s.g(ns, "leases")).Result()
	if err != nil {
		return err
	}
	for _, lease := range leases {
		if lease == current {
			continue
		}
		entries, err := s.readUsage(ctx, ns, lease)
		if err != nil {
			return err
		}
		if len(entries) == 0 {
			if err := emptyLeaseScript.Run(ctx, s.rdb, []string{s.g(ns, "leases"), s.g(ns, "u:"+lease)}, lease).Err(); err != nil {
				return err
			}
			continue
		}
		id, err := billing.ParseLeaseID(lease)
		if err != nil {
			continue
		}
		params := billing.SyncLeasesParams{LeaseID: id, Usage: make([]billing.LeaseUsage, len(entries)), Wants: []billing.LeaseWant{}, Returns: []billing.HoldReturn{}, Committed: []billing.HoldCommitment{}}
		for i, e := range entries {
			params.Usage[i] = e.item
		}
		call, cancel := context.WithTimeout(ctx, syncTimeout)
		res, syncErr := s.c.SyncLeases(call, params)
		cancel()
		pipe := s.rdb.Pipeline()
		var se *billing.StatusError
		switch {
		case syncErr == nil:
			if err := s.applyAnswer(ctx, pipe, ns, lease, entries, &billing.SyncLeasesResult{Usage: res.Usage}, nil); err != nil {
				return err
			}
		case errors.As(syncErr, &se) && !errors.Is(syncErr, billing.ErrUnreachable):
			s.dropEntries(ctx, pipe, ns, lease, entries)
		default:
			return nil
		}
		if _, err := pipe.Exec(ctx); err != nil {
			return err
		}
	}
	return nil
}

// commitments are what open tickets took of the lease's holds, read as the
// sync is built.
func (s *sharedGate) commitments(ctx context.Context, ns, lease string) ([]billing.HoldCommitment, error) {
	refs, err := s.rdb.HGetAll(ctx, s.g(ns, "holds")).Result()
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(refs))
	for id := range refs {
		ids = append(ids, id)
	}
	fields := make([]*redis.MapStringStringCmd, len(ids))
	if _, err := s.rdb.Pipelined(ctx, func(p redis.Pipeliner) error {
		for i, id := range ids {
			fields[i] = p.HGetAll(ctx, s.h(ns, id))
		}
		return nil
	}); err != nil && !errors.Is(err, redis.Nil) {
		return nil, err
	}
	out := []billing.HoldCommitment{}
	for i, id := range ids {
		f := fields[i].Val()
		var ref holdRef
		if f["lease"] != lease || json.Unmarshal([]byte(refs[id]), &ref) != nil {
			continue
		}
		c := billing.HoldCommitment{CustomerID: ref.Customer, LimitSet: ref.Set}
		for k, v := range f {
			part, ok := strings.CutPrefix(k, "o:")
			if n, _ := strconv.ParseInt(v, 10, 64); ok && n > 0 {
				c.Limits = append(c.Limits, billing.LimitCommitment{Limit: partLimit(part), WindowStartsAt: partStart(part), Committed: n})
			}
		}
		if len(c.Limits) > 0 && len(out) < billing.MaxSyncCommitted {
			out = append(out, c)
		}
	}
	return out, nil
}

// evict queues the least recently used idle holds past the bound for
// return.
func (s *sharedGate) evict(ctx context.Context, ns string) error {
	n, err := s.rdb.HLen(ctx, s.g(ns, "holds")).Result()
	if err != nil || n <= maxGateHolds {
		return err
	}
	ids, err := s.rdb.HKeys(ctx, s.g(ns, "holds")).Result()
	if err != nil {
		return err
	}
	used := make([]*redis.StringCmd, len(ids))
	open := make([]*redis.IntCmd, len(ids))
	if _, err := s.rdb.Pipelined(ctx, func(p redis.Pipeliner) error {
		for i, id := range ids {
			used[i] = p.HGet(ctx, s.h(ns, id), "used")
			open[i] = p.HLen(ctx, s.h(ns, id)+":t")
		}
		return nil
	}); err != nil && !errors.Is(err, redis.Nil) {
		return err
	}
	type candidate struct {
		id   string
		used int64
	}
	var idle []candidate
	for i, id := range ids {
		if open[i].Val() == 0 {
			u, _ := strconv.ParseInt(used[i].Val(), 10, 64)
			idle = append(idle, candidate{id, u})
		}
	}
	slices.SortFunc(idle, func(a, b candidate) int { return int(a.used - b.used) })
	over := int(n) - maxGateHolds
	members := make([]any, 0, over)
	for _, c := range idle[:min(over, len(idle))] {
		members = append(members, c.id)
	}
	if len(members) == 0 {
		return nil
	}
	_, err = s.rdb.Pipelined(ctx, func(p redis.Pipeliner) error {
		for _, m := range members {
			p.HSet(ctx, s.h(ns, m.(string)), "lease", "")
		}
		p.SAdd(ctx, s.g(ns, "returns"), members...)
		return nil
	})
	return err
}

// reclaim gives back what silent processes' open tickets took.
func (s *sharedGate) reclaim(ctx context.Context, ns string) error {
	procs, err := s.rdb.HGetAll(ctx, s.g(ns, "procs")).Result()
	if err != nil {
		return err
	}
	cutoff := time.Now().Add(-gateProcTTL).UnixMilli()
	dead := map[string]bool{}
	for proc, at := range procs {
		if ms, _ := strconv.ParseInt(at, 10, 64); ms < cutoff {
			dead[proc] = true
		}
	}
	if len(dead) == 0 {
		return nil
	}
	ids, err := s.rdb.HKeys(ctx, s.g(ns, "holds")).Result()
	if err != nil {
		return err
	}
	for _, id := range ids {
		tickets, err := s.rdb.HGetAll(ctx, s.h(ns, id)+":t").Result()
		if err != nil {
			return err
		}
		var lost []any
		for ticket, v := range tickets {
			if fields := strings.SplitN(v, "|", 3); len(fields) == 3 && fields[0] == "o" && dead[fields[1]] {
				lost = append(lost, ticket)
			}
		}
		if len(lost) > 0 {
			if err := reclaimScript.Run(ctx, s.rdb, []string{s.h(ns, id), s.h(ns, id) + ":t"}, lost...).Err(); err != nil {
				return err
			}
		}
	}
	stale := make([]string, 0, len(dead))
	for proc := range dead {
		stale = append(stale, proc)
	}
	return s.rdb.HDel(ctx, s.g(ns, "procs"), stale...).Err()
}

func (s *sharedGate) limits(ctx context.Context, customer billing.CustomerID) ([]billing.CustomerLimit, error) {
	ns, ok := s.namespace()
	if !ok || s.down() {
		return s.fallback.limits(ctx, customer)
	}
	raw, err := s.rdb.Get(ctx, s.g(ns, "limits:"+customer.String())).Result()
	if s.redisFailed(err) {
		return s.fallback.limits(ctx, customer)
	}
	var lines []billing.CustomerLimit
	if err != nil || json.Unmarshal([]byte(raw), &lines) != nil {
		page, err := s.c.ListCustomerLimits(ctx, customer)
		if errors.Is(err, billing.ErrUnreachable) {
			return nil, unavailable(err)
		}
		if err != nil {
			return nil, err
		}
		lines = page.Items
	}
	unsentRaw, err := s.rdb.HGetAll(ctx, s.g(ns, "unsent:"+customer.String())).Result()
	if s.redisFailed(err) {
		return lines, nil
	}
	unsent := map[string]int64{}
	for k, v := range unsentRaw {
		c, _ := strconv.ParseInt(v, 10, 64)
		unsent[strings.Replace(k, "|", "\x00", 1)] = c
	}
	return lessUnsent(lines, unsent), nil
}

// flush waits until the group has settled the usage numbered before the
// call.
func (s *sharedGate) flush(ctx context.Context) error {
	ns, ok := s.namespace()
	if !ok || s.down() {
		return nil
	}
	leases, err := s.rdb.SMembers(ctx, s.g(ns, "leases")).Result()
	if s.redisFailed(err) {
		return fmt.Errorf("%w: %w", errRedis, err)
	}
	targets := map[string]int64{}
	for _, lease := range leases {
		n, err := s.rdb.HGet(ctx, s.g(ns, "lease"), "n:"+lease).Int64()
		if err == nil {
			targets[lease] = n
		}
	}
	started := time.Now().UnixMilli()
	t := time.NewTicker(gatePoll)
	defer t.Stop()
	var kicked time.Time
	for {
		pending := false
		for lease, last := range targets {
			msgs, err := s.rdb.XRangeN(ctx, s.g(ns, "u:"+lease), "-", "+", 1).Result()
			if s.redisFailed(err) {
				return fmt.Errorf("%w: %w", errRedis, err)
			}
			if len(msgs) > 0 {
				if n, _ := strconv.ParseInt(fmt.Sprint(msgs[0].Values["n"]), 10, 64); n <= last {
					pending = true
				}
			}
		}
		if !pending {
			return nil
		}
		state, err := s.rdb.HMGet(ctx, s.g(ns, "state"), "ok", "err", "cause").Result()
		if err == nil && len(state) == 3 {
			okAt, _ := strconv.ParseInt(fmt.Sprint(state[0]), 10, 64)
			errAt, _ := strconv.ParseInt(fmt.Sprint(state[1]), 10, 64)
			if errAt > started && errAt > okAt {
				return unavailable(fmt.Errorf("%w: %v", billing.ErrUnreachable, state[2]))
			}
		}
		if time.Since(kicked) >= limits.SyncInterval {
			_ = s.rdb.LPush(ctx, s.g(ns, "kick"), "1").Err()
			kicked = time.Now()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// close flushes and leaves the group: its lease lives while another process
// syncs it.
func (s *sharedGate) close(ctx context.Context) error {
	err := s.flush(ctx)
	close(s.stop)
	s.cancel()
	select {
	case <-s.done:
	case <-ctx.Done():
		return errors.Join(err, ctx.Err())
	}
	if ns, ok := s.namespace(); ok {
		c, cancel := context.WithTimeout(context.Background(), syncTimeout)
		_ = s.rdb.HDel(c, s.g(ns, "procs"), s.proc).Err()
		cancel()
	}
	return err
}

// Scripts. Costs and held units are integers below 2^53; HINCRBY takes them
// as the strings the process sent.
const luaCosts = `
local function neg(c) if string.sub(c, 1, 1) == '-' then return string.sub(c, 2) end return '-' .. c end
local function costs(v) return string.match(v, '^[oc]|[^|]*|(.*)$') or '' end
`

// takeScript: KEYS hold, tickets; ARGV lease, ver, ticket, process, live,
// safe cost (empty: none), n, then n of (part, cost, mode 0 whole estimate | 1
// while positive). Answers {1, low, takes...}, {0} not this lease's hold,
// {-1} stale pricing, {-2} a part without a current window, {-3} short.
var takeScript = redis.NewScript(`
local h = KEYS[1]
local v = redis.call('HMGET', h, 'lease', 'ver')
if v[1] ~= ARGV[1] then return {0} end
if v[2] ~= ARGV[2] then return {-1} end
local t = redis.call('TIME')
local now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
local n = tonumber(ARGV[7])
local takes = {}
local short = ARGV[5] ~= '1'
local low = 0
for i = 0, n - 1 do
  local id, cost, mode = ARGV[8 + i * 3], tonumber(ARGV[9 + i * 3]), ARGV[10 + i * 3]
  local f = redis.call('HMGET', h, 'h:' .. id, 'o:' .. id, 'u:' .. id, 'e:' .. id)
  if not f[1] then return {-2} end
  if f[4] and f[4] ~= '' and now >= tonumber(f[4]) then return {-2} end
  local avail = tonumber(f[1]) - tonumber(f[2] or '0') - tonumber(f[3] or '0')
  local take = cost
  if mode == '1' then
    if avail <= 0 then short = true elseif avail < cost then take = avail end
  elseif avail < cost then
    short = true
  end
  if avail - cost < cost then low = 1 end
  takes[i + 1] = take
end
if short then
  if ARGV[6] == '' then return {-3} end
  local sv = redis.call('HMGET', h, 'safe', 'safe_spent')
  if not sv[1] or sv[1] == '' then return {-3} end
  if tonumber(sv[1]) - tonumber(sv[2] or '0') < tonumber(ARGV[6]) then return {-3} end
  redis.call('HINCRBY', h, 'safe_spent', ARGV[6])
  for i = 0, n - 1 do takes[i + 1] = tonumber(ARGV[9 + i * 3]) end
  low = 1
end
local rec = {}
local out = {1, low}
for i = 0, n - 1 do
  local id = ARGV[8 + i * 3]
  local c = string.format('%d', takes[i + 1])
  redis.call('HINCRBY', h, 'o:' .. id, c)
  rec[i + 1] = id .. '=' .. c
  out[#out + 1] = c
end
redis.call('HSET', h, 'used', string.format('%d', now))
redis.call('HSET', KEYS[2], ARGV[3], 'o|' .. ARGV[4] .. '|' .. table.concat(rec, ';'))
return out
`)

// releaseScript: KEYS hold, tickets; ARGV ticket.
var releaseScript = redis.NewScript(luaCosts + `
local v = redis.call('HGET', KEYS[2], ARGV[1])
if not v or string.sub(v, 1, 1) ~= 'o' then return 0 end
for id, c in string.gmatch(costs(v), '([^=;]+)=(%-?%d+)') do redis.call('HINCRBY', KEYS[1], 'o:' .. id, neg(c)) end
redis.call('HDEL', KEYS[2], ARGV[1])
return 1
`)

// captureScript: KEYS hold, tickets; ARGV ticket, n, actual costs.
var captureScript = redis.NewScript(luaCosts + `
local v = redis.call('HGET', KEYS[2], ARGV[1])
if not v or string.sub(v, 1, 1) ~= 'o' then return 0 end
for id, c in string.gmatch(costs(v), '([^=;]+)=(%-?%d+)') do redis.call('HINCRBY', KEYS[1], 'o:' .. id, neg(c)) end
for id, c in string.gmatch(ARGV[3], '([^=;]+)=(%-?%d+)') do redis.call('HINCRBY', KEYS[1], 'u:' .. id, c) end
redis.call('HSET', KEYS[2], ARGV[1], 'c|' .. ARGV[2] .. '|' .. ARGV[3])
return 1
`)

// reclaimScript: KEYS hold, tickets; ARGV open tickets of dead processes.
var reclaimScript = redis.NewScript(luaCosts + `
for _, ticket in ipairs(ARGV) do
  local v = redis.call('HGET', KEYS[2], ticket)
  if v and string.sub(v, 1, 1) == 'o' then
    for id, c in string.gmatch(costs(v), '([^=;]+)=(%-?%d+)') do redis.call('HINCRBY', KEYS[1], 'o:' .. id, neg(c)) end
    redis.call('HDEL', KEYS[2], ticket)
  end
end
return 1
`)

// settleScript: KEYS hold, tickets; ARGV lease, mode, pricing, safe, then
// parts (count, then id, held, end ms), settled tickets (count, then ids),
// event types (count, then types). Mode 1 applies a sync's hold (a hold of
// another lease starts over); 2 creates an empty hold when none is there; 0
// and 3 only settle (0 also takes the cost from held: the server consumed
// it). A settled ticket's cost leaves what it counts in (open, or unsettled
// once captured); an added event type bumps ver.
var settleScript = redis.NewScript(luaCosts + `
local h, t = KEYS[1], KEYS[2]
local lease, mode = ARGV[1], ARGV[2]
local i = 5
local parts, order = {}, {}
for j = 1, tonumber(ARGV[i]) do
  parts[ARGV[i + 1]] = {ARGV[i + 2], ARGV[i + 3]}
  order[#order + 1] = ARGV[i + 1]
  i = i + 3
end
i = i + 1
local cur = redis.call('HGET', h, 'lease')
if (mode == '1' or mode == '2') and cur ~= lease then
  redis.call('DEL', h, t)
  if mode == '2' then
    redis.call('HSET', h, 'lease', lease, 'p', ARGV[3], 'safe', '', 'safe_spent', 0)
    redis.call('HINCRBY', h, 'ver', 1)
  end
  cur = lease
end
local nsettled = tonumber(ARGV[i])
for j = 1, nsettled do
  local ticket = ARGV[i + j]
  local v = redis.call('HGET', t, ticket)
  if v and cur == lease then
    local field = 'u:'
    if string.sub(v, 1, 1) == 'o' then field = 'o:' end
    for id, c in string.gmatch(costs(v), '([^=;]+)=(%-?%d+)') do
      redis.call('HINCRBY', h, field .. id, neg(c))
      if mode == '0' then redis.call('HINCRBY', h, 'h:' .. id, neg(c)) end
    end
  end
  if v then redis.call('HDEL', t, ticket) end
end
i = i + nsettled + 1
local bump = false
for j = 1, tonumber(ARGV[i] or '0') do
  if redis.call('HSETNX', h, 'ev:' .. ARGV[i + j], 1) == 1 then bump = true end
end
if mode == '1' then
  redis.call('HSET', h, 'lease', lease, 'p', ARGV[3], 'safe', ARGV[4], 'safe_spent', 0)
  for _, f in ipairs(redis.call('HKEYS', h)) do
    if string.sub(f, 1, 2) == 'h:' then
      local id = string.sub(f, 3)
      if not parts[id] then
        local ou = redis.call('HMGET', h, 'o:' .. id, 'u:' .. id)
        if tonumber(ou[1] or '0') == 0 and tonumber(ou[2] or '0') == 0 then
          redis.call('HDEL', h, f, 'o:' .. id, 'u:' .. id, 'e:' .. id)
        else
          redis.call('HSET', h, f, 0)
        end
      end
    end
  end
  for _, id in ipairs(order) do
    redis.call('HSET', h, 'h:' .. id, parts[id][1], 'e:' .. id, parts[id][2])
  end
  bump = true
end
if bump and cur == lease then redis.call('HINCRBY', h, 'ver', 1) end
return 1
`)

// usageScript: KEYS lease, stream, leases, unsent; ARGV lease, item, hold,
// ticket, unsent costs. Numbers the item in its lease.
var usageScript = redis.NewScript(`
local n = redis.call('HINCRBY', KEYS[1], 'n:' .. ARGV[1], 1)
redis.call('XADD', KEYS[2], '*', 'n', n, 'item', ARGV[2], 'hold', ARGV[3], 'ticket', ARGV[4], 'unsent', ARGV[5])
redis.call('SADD', KEYS[3], ARGV[1])
for k, c in string.gmatch(ARGV[5], '([^=;]+)=(%-?%d+)') do redis.call('HINCRBY', KEYS[4], k, c) end
if ARGV[5] ~= '' then redis.call('PEXPIRE', KEYS[4], 86400000) end
return n
`)

// wantScript: KEYS wants, kick, gen; ARGV attrs, want. Answers the sync
// generation the want waits past.
var wantScript = redis.NewScript(`
redis.call('HSET', KEYS[1], ARGV[1], ARGV[2])
redis.call('LPUSH', KEYS[2], '1')
redis.call('LTRIM', KEYS[2], 0, 0)
return tonumber(redis.call('GET', KEYS[3]) or '0')
`)

var renewScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then return redis.call('PEXPIRE', KEYS[1], ARGV[2]) end
return 0
`)

var unlockScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then return redis.call('DEL', KEYS[1]) end
return 0
`)

// emptyLeaseScript: KEYS leases, stream; ARGV lease. Forgets a lease whose
// usage all settled.
var emptyLeaseScript = redis.NewScript(`
if redis.call('XLEN', KEYS[2]) == 0 then
  redis.call('SREM', KEYS[1], ARGV[1])
  redis.call('DEL', KEYS[2])
end
return 1
`)
