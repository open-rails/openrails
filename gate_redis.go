package openrails

import (
	"cmp"
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
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/modules/limits"
)

// A shared gate keeps one lease for the merchant's processes in Redis. The
// group's keys share one hash slot ({...:<ns>}); each hold's keys share
// their own ({...:<ns>:<hold>}), so a decision is one script on one slot.
//
//	G:lease    hash  id, valid (unix ms the lease lives until, by the
//	                 syncer's clock less a skew margin; 0 not yet answered,
//	                 1 dead), n:<lease> highest number issued
//	G:u:<id>   stream of a lease's unsettled usage: n, item, hold, ticket,
//	           unsent, costs
//	G:leases   set   leases with a usage stream
//	G:close    set   earlier leases to close
//	G:k:<key>  a usage write (a ticket's, or a Record's source and source_id)
//	           -> lease|n, kept for gateWriteKeyTTL
//	G:lock     the syncing process, renewed while it syncs
//	G:kick     list  wakes the syncing process for an early sync
//	G:wants    hash  want key -> want for the next sync
//	G:answers:<gen>  hash  want key -> that sync's answer (expires)
//	G:sets     hash  attrs -> limit set
//	G:gen      completed sync attempts
//	G:state    hash  ok, err (unix ms), kind, cause, refused
//	G:holds    hash  hold -> {customer, set}
//	G:applied  hash  hold -> hold version applied since the last answer
//	G:returns  set   holds to return
//	G:procs    hash  process -> heartbeat (unix ms)
//	G:limits:<customer>  display lines
//	G:unsent:<customer>  hash  limit|unit -> unsent cost
//	H          hash  lease, ver, p (the hold set), safe, safe_spent, used,
//	                 ev:<event type>, h:<part> held, o:<part> taken by open
//	                 tickets, u:<part> captured and unsettled, e:<part> end
//	H:t        hash  ticket -> o|<process>|<part>=<cost>;... or c|<n>|...
const (
	gateKeyPrefix = "openrails:gate:v2:"
	// gateLockTTL is how long the syncing process keeps the group's lock
	// without renewing it; past it another process syncs.
	gateLockTTL = 3 * limits.SyncInterval
	// gateProcTTL is how long a process may stay silent before its open
	// tickets are reclaimed: well past a lease's life and a sync, so only a
	// dead process's are. Processes beat on their own cadence.
	gateProcTTL = 3 * (limits.LeaseLifetime + syncTimeout)
	// gateClockSkew is taken off the lease's validity, so a process whose
	// clock lags the syncing one's stops deciding from it before the server
	// lets it die.
	gateClockSkew = 2 * time.Second
	// gateRedisDown is how long decisions use the process's own lease after
	// Redis failed.
	gateRedisDown = limits.SyncInterval
	// gateRedisSettled is how long Redis must answer before the process's
	// own lease, once idle, is closed.
	gateRedisSettled = 3 * limits.SyncInterval
	// gateReclaimEvery is how many syncs pass between reclaims.
	gateReclaimEvery = 10
	gateDisplayTTL   = time.Minute
	gatePoll         = 5 * time.Millisecond
	// gateAnswerScan bounds the sync answers a waiter reads at once; one
	// that skipped some sends its want again.
	gateAnswerScan = 32
	// gateWantResend is how many syncs a want waits for its answer before
	// it is sent again.
	gateWantResend = 3
	// gateAwaitStall is how long a want waits for any sync of the group
	// before it is answered as OpenRails unreachable.
	gateAwaitStall = gateLockTTL + 2*syncTimeout
	// gateWriteAttempts bounds the retries of a usage write whose outcome
	// is unknown.
	gateWriteAttempts = 3
	// gateWriteKeyTTL is how long a usage write's key is kept: a capture or
	// Record retried within it posts once.
	gateWriteKeyTTL = 24 * time.Hour
	maxGateOrphans  = 10_000
	// maxGateMaybe bounds the write keys a process remembers as maybe
	// written; past it the oldest is forgotten.
	maxGateMaybe = 100_000
)

type sharedGate struct {
	c    *Client
	rdb  redis.UniversalClient
	proc string

	fbMu sync.Mutex
	fb   *localGate

	nsMu sync.Mutex
	ns   string

	downUntil atomic.Int64
	upSince   atomic.Int64

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
	orphans []orphan
	// maybe are write keys whose write may have reached Redis: they are
	// retried there, never reported on the process's own lease.
	maybe map[string]time.Time

	waiting atomic.Int32

	ctx    context.Context
	cancel context.CancelFunc
	stop   chan struct{}
	wg     sync.WaitGroup
}

type sharedPricing struct {
	lease   string
	ver     string
	pricing *holdPricing
}

// orphan is a ticket's take left in Redis by a capture or release that
// could not reach it, released once it can.
type orphan struct{ ns, hold, ticket string }

func newSharedGate(c *Client, rdb redis.UniversalClient) *sharedGate {
	ctx, cancel := context.WithCancel(context.Background())
	s := &sharedGate{c: c, rdb: rdb, proc: uuid.NewString(),
		sets: map[string]string{}, prices: map[string]*sharedPricing{}, maybe: map[string]time.Time{}, genCh: make(chan struct{}),
		ctx: ctx, cancel: cancel, stop: make(chan struct{})}
	s.fb = s.newFallback()
	return s
}

// newFallback is the process's own lease for while Redis is unreachable;
// usage captured on it after it retired goes to the group.
func (s *sharedGate) newFallback() *localGate {
	fb := newLocalGate(s.c, nil, false)
	fb.redirect = s.record
	return fb
}

func (s *sharedGate) fallback() *localGate {
	s.fbMu.Lock()
	defer s.fbMu.Unlock()
	return s.fb
}

// namespace is the group's key namespace: the server and merchant. A handle
// that has not yet learned its merchant has none, and decides from its own
// lease meanwhile.
func (s *sharedGate) namespace() (string, bool) {
	s.nsMu.Lock()
	defer s.nsMu.Unlock()
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

func (s *sharedGate) holdKeys(ns, hold string) []string {
	return []string{s.h(ns, hold), s.h(ns, hold) + ":t"}
}

func (s *sharedGate) down() bool { return time.Now().UnixNano() < s.downUntil.Load() }

func (s *sharedGate) markDown() {
	s.downUntil.Store(time.Now().Add(gateRedisDown).UnixNano())
	s.upSince.Store(0)
}

// redisFailed reports an error of Redis itself (not a miss), marking Redis
// down unless the caller gave up.
func (s *sharedGate) redisFailed(err error) bool {
	if err == nil || errors.Is(err, redis.Nil) {
		return false
	}
	if !errors.Is(err, context.Canceled) {
		s.markDown()
	}
	return true
}

var (
	// errRedis is Redis failing a decision: the process decides from its
	// own lease instead.
	errRedis = errors.New("openrails: gate: Redis unavailable")
	// errNotWritten is a usage write known not to have reached Redis.
	errNotWritten = errors.New("openrails: gate: usage not written to Redis")
)

func (s *sharedGate) start() {
	s.wg.Add(3)
	go s.run()
	go s.beat()
	go s.pollGen()
}

func (s *sharedGate) admit(ctx context.Context, gate *Gate, p billing.AdmitParams) (*Ticket, error) {
	ns, ok := s.namespace()
	if !ok || s.down() {
		return s.fallback().admit(ctx, gate, p)
	}
	t, err := s.admitShared(ctx, gate, ns, p)
	if errors.Is(err, errRedis) {
		return s.fallback().admit(ctx, gate, p)
	}
	return t, err
}

func (s *sharedGate) admitShared(ctx context.Context, gate *Gate, ns string, p billing.AdmitParams) (*Ticket, error) {
	r := admitRequest(p)
	attrs := attrsKey(p)
	key := attrs
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
		a, err := s.await(ctx, ns, key, attrs, p)
		if err != nil {
			return nil, err
		}
		switch a.step(ctx, p) {
		case coldSafe:
			return s.safe(ctx, gate, ns, p, r, attrs, a.unavailable)
		case coldFail:
			return nil, a.err
		case coldOwnWant:
			key = ownWantKey(attrs, p)
		case coldRefused:
			return refusedTicket(gate, p, a.refusal), nil
		case coldAnswered:
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
	s.cacheSet(attrs, set)
	return set, true, nil
}

func (s *sharedGate) cacheSet(attrs, set string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.sets) >= maxGateLimitSets {
		s.sets = map[string]string{}
	}
	s.sets[attrs] = set
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
	var set holdSet
	if err := json.Unmarshal([]byte(fields["p"]), &set); err != nil {
		return nil, fmt.Errorf("openrails: gate: hold in Redis: %w", err)
	}
	events := map[string]bool{}
	for f := range fields {
		if e, ok := strings.CutPrefix(f, "ev:"); ok {
			events[e] = true
		}
	}
	sp = &sharedPricing{lease: fields["lease"], ver: fields["ver"], pricing: newHoldPricing(set.merged(), events)}
	s.mu.Lock()
	if len(s.prices) >= maxGateHolds {
		s.prices = map[string]*sharedPricing{}
	}
	s.prices[hold] = sp
	s.mu.Unlock()
	return sp, nil
}

func (s *sharedGate) leaseState() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lease, s.lease != "" && time.Now().UnixMilli() < s.valid
}

func (s *sharedGate) setLease(lease string, valid int64) {
	s.mu.Lock()
	s.lease, s.valid = lease, valid
	s.mu.Unlock()
}

// decide is one decision on the group's hold: one script that takes the
// cost from every limit or from none. A hold without parts is cold.
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
		if sp == nil || sp.lease != holdLease || holdLease == "" || len(sp.pricing.parts) == 0 {
			return nil, decideCold, nil
		}
		takes, money, err := sp.pricing.cost(time.Now(), r, withSafe)
		if errors.Is(err, errUnpriced) || err == nil && len(takes) == 0 {
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
			mode := 0
			if t.overrun == catalog.OverrunAdmitWhilePositive {
				mode = 1
			}
			if !t.money {
				mode += 2
			}
			args = append(args, t.part, strconv.FormatInt(t.cost, 10), strconv.Itoa(mode))
		}
		res, err := takeScript.Run(ctx, s.rdb, s.holdKeys(ns, hold), args...).Slice()
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
				go s.prefetch(ctx, ns, attrsKey(p), p)
			}
			return &Ticket{gate: gate, params: p, limitSet: set, shared: &sharedTicket{s: s, ns: ns, hold: hold, id: ticket, takes: takes, pricing: sp.pricing}}, decideAllowed, nil
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

func decodeTakes(s string) []take {
	var out []take
	for _, kv := range strings.Split(s, ";") {
		part, v, ok := strings.Cut(kv, "=")
		if c, err := strconv.ParseInt(v, 10, 64); ok && err == nil {
			out = append(out, take{part: part, cost: c})
		}
	}
	return out
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

// capture numbers the usage in the group's lease stream once per ticket,
// then turns the ticket's take into its cost. A write whose outcome is
// unknown fails the capture and marks the ticket maybe written: capturing
// again retries it in Redis, never elsewhere. Only a write known not to have
// happened (Redis unreachable before the ticket's first write) is reported
// on the process's own lease with no hold, and the ticket's take is released
// once Redis answers.
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
	n, err := s.number(ctx, st.ns, item, st.hold, st.id, st.id, actual)
	if errors.Is(err, errNotWritten) {
		s.orphan(st.ns, st.hold, st.id)
		item.LimitSet = nil
		fb := s.fallback()
		fb.ensureStarted()
		return fb.enqueue(ctx, item, nil, nil, nil)
	}
	if err != nil {
		return notCaptured{err}
	}
	// Failing, the ticket's take counts in place of its cost until its
	// usage settles.
	_ = s.retry(ctx, func(c context.Context) error {
		return captureScript.Run(c, s.rdb, s.holdKeys(st.ns, st.hold), st.id, n, encodeTakes(actual)).Err()
	})
	return nil
}

// release gives the ticket's take back; one Redis cannot take now is
// released once it can.
func (st *sharedTicket) release(ctx context.Context) {
	s := st.s
	err := releaseScript.Run(ctx, s.rdb, s.holdKeys(st.ns, st.hold), st.id).Err()
	if s.redisFailed(err) {
		s.orphan(st.ns, st.hold, st.id)
	}
}

func (s *sharedGate) orphan(ns, hold, ticket string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.orphans) < maxGateOrphans {
		s.orphans = append(s.orphans, orphan{ns, hold, ticket})
	}
}

func (s *sharedGate) releaseOrphans(ctx context.Context) {
	s.mu.Lock()
	list := s.orphans
	s.orphans = nil
	s.mu.Unlock()
	for i, o := range list {
		if err := releaseScript.Run(ctx, s.rdb, s.holdKeys(o.ns, o.hold), o.ticket).Err(); s.redisFailed(err) {
			s.mu.Lock()
			s.orphans = append(list[i:], s.orphans...)
			s.mu.Unlock()
			return
		}
	}
}

// retry runs a Redis write that is safe to repeat until it succeeds, a few
// times within ctx (bounded when ctx has none).
func (s *sharedGate) retry(ctx context.Context, write func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), syncTimeout)
	defer cancel()
	var err error
	for attempt := 0; attempt < gateWriteAttempts; attempt++ {
		if err = write(ctx); err == nil || errors.Is(err, redis.Nil) {
			return nil
		}
		s.redisFailed(err)
		if !sleepCtx(ctx, time.Duration(attempt+1)*10*time.Millisecond) {
			break
		}
	}
	return err
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// number writes an item to the group's current lease stream once per write
// key, which Redis keeps for gateWriteKeyTTL: a retry of a write whose
// outcome is unknown finds it. errNotWritten is a write known not to have
// happened, never answered for a key an earlier write may have reached.
func (s *sharedGate) number(ctx context.Context, ns string, item billing.LeaseUsage, hold, ticket, key string, costs []take) (int64, error) {
	maybe := s.mayBeWritten(key)
	if s.down() && !maybe {
		return 0, errNotWritten
	}
	body, err := json.Marshal(item)
	if err != nil {
		return 0, err
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, syncTimeout)
		defer cancel()
	}
	lease, _ := s.leaseState()
	unknown := 0
	for hops := 0; hops < 5; hops++ {
		if lease == "" {
			// Only the script can tell whether a write that may have
			// happened did.
			cur, err := s.rdb.HGet(ctx, s.g(ns, "lease"), "id").Result()
			if (s.redisFailed(err) || cur == "") && unknown == 0 && !maybe {
				return 0, errNotWritten
			}
			lease = cur
		}
		keys := []string{s.g(ns, "lease"), s.g(ns, "u:"+lease), s.g(ns, "leases"), s.g(ns, "k:"+key), s.g(ns, "unsent:"+item.CustomerID.String())}
		res, err := usageScript.Run(ctx, s.rdb, keys, lease, body, hold, ticket, unsentArg(item.Currency, costs), encodeTakes(costs), gateWriteKeyTTL.Milliseconds()).Slice()
		if err == nil && len(res) == 2 {
			code, _ := res[0].(int64)
			ref := fmt.Sprint(res[1])
			if code == 1 {
				s.written(key)
				_, n, _ := strings.Cut(ref, "|")
				return strconv.ParseInt(n, 10, 64)
			}
			// The group's lease is another (or none); the key was not
			// found, so nothing was written.
			if ref == "" {
				s.written(key)
				return 0, errNotWritten
			}
			lease = ref
			continue
		}
		if err == nil {
			err = fmt.Errorf("unexpected reply %v", res)
		}
		s.redisFailed(err)
		if unknown++; unknown >= gateWriteAttempts || !sleepCtx(ctx, time.Duration(unknown)*10*time.Millisecond) {
			return 0, s.mayBe(key, err)
		}
	}
	err = errors.New("openrails: gate: the group's lease kept changing under a usage write")
	if unknown == 0 {
		s.written(key)
		return 0, err
	}
	return 0, s.mayBe(key, err)
}

// mayBe marks a write key maybe written and answers the write's failure: a
// retry goes to Redis only.
func (s *sharedGate) mayBe(key string, cause error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.maybe) >= maxGateMaybe {
		now := time.Now()
		var oldest string
		for k, at := range s.maybe {
			if now.Sub(at) >= gateWriteKeyTTL {
				delete(s.maybe, k)
			} else if oldest == "" || at.Before(s.maybe[oldest]) {
				oldest = k
			}
		}
		if len(s.maybe) >= maxGateMaybe {
			delete(s.maybe, oldest)
		}
	}
	s.maybe[key] = time.Now()
	return fmt.Errorf("openrails: gate: usage may have reached Redis; retry to settle it once: %w", cause)
}

// mayBeWritten reports a write key an earlier write may have reached.
func (s *sharedGate) mayBeWritten(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	at, ok := s.maybe[key]
	if ok && time.Since(at) >= gateWriteKeyTTL {
		delete(s.maybe, key)
		ok = false
	}
	return ok
}

// written forgets a write key once Redis has answered whether it was
// written.
func (s *sharedGate) written(key string) {
	s.mu.Lock()
	delete(s.maybe, key)
	s.mu.Unlock()
}

// record writes usage with no admission once per source and source_id: a
// Record repeated within gateWriteKeyTTL finds the first write.
func (s *sharedGate) record(ctx context.Context, item billing.LeaseUsage) error {
	ns, ok := s.namespace()
	if !ok {
		return s.fallback().record(ctx, item)
	}
	_, err := s.number(ctx, ns, item, "", "", recordKey(item), nil)
	if errors.Is(err, errNotWritten) {
		return s.fallback().record(ctx, item)
	}
	return err
}

func recordKey(item billing.LeaseUsage) string {
	sum := sha256.Sum256([]byte(item.Source + "\x00" + item.SourceID))
	return "r:" + hex.EncodeToString(sum[:16])
}

// storedWant is a want in Redis; storedAnswer a sync's answer to it.
type storedWant struct {
	Want      billing.LeaseWant `json:"want"`
	EventType string            `json:"event_type"`
	Attrs     string            `json:"attrs"`
}

type storedAnswer struct {
	Gen         int64                `json:"gen"`
	LimitSet    string               `json:"limit_set,omitempty"`
	Refusal     *billing.Refusal     `json:"refusal,omitempty"`
	Sent        *billing.LeaseWant   `json:"sent,omitempty"`
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
		return wantAnswer{limitSet: a.LimitSet, refusal: a.Refusal, sent: a.Sent}
	}
	return wantAnswer{limitSet: a.LimitSet}
}

func storedFailure(gen int64, status int, details billing.ErrorDetails) storedAnswer {
	if overloaded(status) {
		return storedAnswer{Gen: gen, Unavailable: (&billing.StatusError{Status: status, ErrorDetails: details}).Error()}
	}
	return storedAnswer{Gen: gen, Error: &billing.StatusError{Status: status, ErrorDetails: details}}
}

// register adds a want to the group's next sync and wakes the syncing
// process: the sync generation it waits past.
func (s *sharedGate) register(ctx context.Context, ns, key, attrs string, p billing.AdmitParams) (int64, error) {
	body, err := json.Marshal(storedWant{Want: leaseWant(p), EventType: eventTypeOf(p), Attrs: attrs})
	if err != nil {
		return 0, err
	}
	gen, err := wantScript.Run(ctx, s.rdb, []string{s.g(ns, "wants"), s.g(ns, "kick"), s.g(ns, "gen")}, key, body).Int64()
	if s.redisFailed(err) {
		return 0, errRedis
	}
	return gen, err
}

func (s *sharedGate) prefetch(ctx context.Context, ns, attrs string, p billing.AdmitParams) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), syncTimeout)
	defer cancel()
	_, _ = s.register(ctx, ns, attrs, attrs, p)
}

// await waits for the first sync past the want's registration that answers
// it. A want whose answer does not come within a few syncs (its sync was
// lost, or the answer is among skipped ones) is sent again; with no sync of
// the group at all it is answered as OpenRails unreachable.
func (s *sharedGate) await(ctx context.Context, ns, key, attrs string, p billing.AdmitParams) (wantAnswer, error) {
	gen0, err := s.register(ctx, ns, key, attrs, p)
	if err != nil {
		return wantAnswer{}, err
	}
	s.waiting.Add(1)
	defer s.waiting.Add(-1)
	seen, sent := gen0, gen0
	stall := time.NewTimer(gateAwaitStall)
	defer stall.Stop()
	for {
		ch, gen := s.watch()
		if gen > seen {
			from := max(seen+1, gen-gateAnswerScan+1)
			cmds := make([]*redis.StringCmd, 0, gen-from+1)
			_, err := s.rdb.Pipelined(ctx, func(pipe redis.Pipeliner) error {
				for g := from; g <= gen; g++ {
					cmds = append(cmds, pipe.HGet(ctx, s.g(ns, "answers:"+strconv.FormatInt(g, 10)), key))
				}
				return nil
			})
			if s.redisFailed(err) {
				return wantAnswer{}, errRedis
			}
			for _, cmd := range cmds {
				var a storedAnswer
				if raw, err := cmd.Result(); err == nil && json.Unmarshal([]byte(raw), &a) == nil && a.Gen > gen0 {
					if a.LimitSet != "" {
						s.cacheSet(attrs, a.LimitSet)
					}
					if err := s.refreshLease(ctx, ns); err != nil {
						return wantAnswer{}, err
					}
					return a.answer(), nil
				}
			}
			if from > seen+1 || gen-sent >= gateWantResend {
				if _, err := s.register(ctx, ns, key, attrs, p); err != nil {
					return wantAnswer{}, err
				}
				sent = gen
			}
			seen = gen
			stall.Reset(gateAwaitStall)
		}
		select {
		case <-ctx.Done():
			return wantAnswer{}, ctx.Err()
		case <-stall.C:
			return wantAnswer{unavailable: fmt.Errorf("%w: no process of the gate's group synced", billing.ErrUnreachable)}, nil
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
		lease, _ := v[0].(string)
		valid, _ := strconv.ParseInt(fmt.Sprint(v[1]), 10, 64)
		s.setLease(lease, valid)
	}
	return nil
}

// watch is the generation this process last saw and a channel closed when
// it advances; pollGen follows it while anyone waits.
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

// run takes the group's lock and, while it holds it, syncs the group.
func (s *sharedGate) run() {
	defer s.wg.Done()
	ctx := s.ctx
	holder := false
	var last time.Time
	for {
		select {
		case <-s.stop:
			if holder {
				if ns, ok := s.namespace(); ok {
					c, done := context.WithTimeout(context.WithoutCancel(ctx), syncTimeout)
					_ = unlockScript.Run(c, s.rdb, []string{s.g(ns, "lock")}, s.proc).Err()
					done()
				}
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
		holder, err = s.takeLock(c, ns, holder)
		done()
		if s.redisFailed(err) || !holder {
			holder = false
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

// takeLock takes or renews the group's lock.
func (s *sharedGate) takeLock(ctx context.Context, ns string, holder bool) (bool, error) {
	ttl := gateLockTTL.Milliseconds()
	if holder {
		kept, err := renewScript.Run(ctx, s.rdb, []string{s.g(ns, "lock")}, s.proc, ttl).Int64()
		if err != nil {
			return false, err
		}
		if kept == 1 {
			return true, nil
		}
	}
	return s.rdb.SetNX(ctx, s.g(ns, "lock"), s.proc, gateLockTTL).Result()
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
func (s *sharedGate) pollGen() {
	defer s.wg.Done()
	ctx := s.ctx
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

// beat records the process on its own cadence, whatever it syncs, and reads
// the group's lease and the last sync's state; it releases orphaned takes
// and closes the process's own lease once Redis has answered a while.
func (s *sharedGate) beat() {
	defer s.wg.Done()
	t := time.NewTicker(limits.SyncInterval)
	defer t.Stop()
	for {
		s.beatOnce()
		select {
		case <-s.stop:
			return
		case <-t.C:
		}
	}
}

func (s *sharedGate) beatOnce() {
	ns, ok := s.namespace()
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, syncTimeout)
	defer cancel()
	now := time.Now().UnixMilli()
	var lease, state *redis.SliceCmd
	_, err := s.rdb.Pipelined(ctx, func(p redis.Pipeliner) error {
		p.HSet(ctx, s.g(ns, "procs"), s.proc, now)
		lease = p.HMGet(ctx, s.g(ns, "lease"), "id", "valid")
		state = p.HMGet(ctx, s.g(ns, "state"), "ok", "err", "kind", "cause")
		return nil
	})
	if s.redisFailed(err) {
		return
	}
	s.upSince.CompareAndSwap(0, now)
	if v := lease.Val(); len(v) == 2 {
		id, _ := v[0].(string)
		valid, _ := strconv.ParseInt(fmt.Sprint(v[1]), 10, 64)
		s.setLease(id, valid)
	}
	if v := state.Val(); len(v) == 4 {
		okAt, _ := strconv.ParseInt(fmt.Sprint(v[0]), 10, 64)
		errAt, _ := strconv.ParseInt(fmt.Sprint(v[1]), 10, 64)
		s.mu.Lock()
		if errAt > okAt && v[2] == "unavailable" {
			s.failure, s.failAt = fmt.Errorf("%w: %v", billing.ErrUnreachable, v[3]), time.UnixMilli(errAt)
		} else {
			s.failure = nil
		}
		s.mu.Unlock()
	}
	s.releaseOrphans(ctx)
	s.retireFallback()
}

// retireFallback closes the process's own lease once Redis has answered
// for a while and no ticket of it is open; a fresh one stands by.
func (s *sharedGate) retireFallback() {
	up := s.upSince.Load()
	if up == 0 || time.Since(time.UnixMilli(up)) < gateRedisSettled {
		return
	}
	s.fbMu.Lock()
	old := s.fb
	if !old.started.Load() || !old.idle() {
		s.fbMu.Unlock()
		return
	}
	s.fb = s.newFallback()
	s.fbMu.Unlock()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ctx, cancel := context.WithTimeout(context.WithoutCancel(s.ctx), gateFlushBound)
		defer cancel()
		_ = old.close(ctx)
	}()
}

// usageEntry is one stream entry of a lease's usage.
type usageEntry struct {
	id     string
	item   billing.LeaseUsage
	hold   string
	ticket string
	unsent string
	costs  []take
}

// readUsage is the head of a lease's stream, the highest number issued on
// the lease, and the ids of entries that cannot be read.
func (s *sharedGate) readUsage(ctx context.Context, ns, lease string) ([]usageEntry, int64, []string, error) {
	var msgs *redis.XMessageSliceCmd
	var issued *redis.StringCmd
	_, err := s.rdb.Pipelined(ctx, func(p redis.Pipeliner) error {
		msgs = p.XRangeN(ctx, s.g(ns, "u:"+lease), "-", "+", billing.MaxSyncUsageItems)
		issued = p.HGet(ctx, s.g(ns, "lease"), "n:"+lease)
		return nil
	})
	if err != nil && !errors.Is(err, redis.Nil) {
		return nil, 0, nil, err
	}
	n, _ := strconv.ParseInt(issued.Val(), 10, 64)
	out := make([]usageEntry, 0, len(msgs.Val()))
	var bad []string
	for _, m := range msgs.Val() {
		e := usageEntry{id: m.ID}
		body, _ := m.Values["item"].(string)
		var err error
		if err = json.Unmarshal([]byte(body), &e.item); err == nil {
			e.item.N, err = strconv.ParseInt(fmt.Sprint(m.Values["n"]), 10, 64)
		}
		if err != nil {
			bad = append(bad, m.ID)
			continue
		}
		e.hold, _ = m.Values["hold"].(string)
		e.ticket, _ = m.Values["ticket"].(string)
		e.unsent, _ = m.Values["unsent"].(string)
		costs, _ := m.Values["costs"].(string)
		e.costs = decodeTakes(costs)
		out = append(out, e)
	}
	return out, n, bad, nil
}

type holdRef struct {
	Customer billing.CustomerID `json:"customer"`
	Set      string             `json:"set"`
}

// groupHolds is the group's holds as one sync reads them: their refs, each
// hold hash, and the open tickets' commitments of the lease's holds.
type groupHolds struct {
	refs      map[string]holdRef
	fields    map[string]map[string]string
	committed []billing.HoldCommitment
}

type sentWant struct {
	key  string
	want storedWant
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
		old := lease
		lease = newLeaseID().String()
		if _, err := s.rdb.TxPipelined(ctx, func(p redis.Pipeliner) error {
			p.HSet(ctx, s.g(ns, "lease"), "id", lease, "valid", 0)
			p.Del(ctx, s.g(ns, "applied"))
			if old != "" {
				// Closed, so its holds free at once.
				p.SAdd(ctx, s.g(ns, "close"), old)
			}
			return nil
		}); err != nil {
			return err
		}
		valid = 0
	}
	leaseID, err := billing.ParseLeaseID(lease)
	if err != nil {
		return err
	}
	holds, err := s.readHolds(ctx, ns, lease)
	if err != nil {
		return err
	}
	if err := s.evict(ctx, ns, lease, holds); err != nil {
		return err
	}
	entries, issued, bad, err := s.readUsage(ctx, ns, lease)
	if err != nil {
		return err
	}
	rawWants, err := s.rdb.HGetAll(ctx, s.g(ns, "wants")).Result()
	if err != nil {
		return err
	}
	rawApplied, err := s.rdb.HGetAll(ctx, s.g(ns, "applied")).Result()
	if err != nil {
		return err
	}
	retIDs, err := s.rdb.SRandMemberN(ctx, s.g(ns, "returns"), billing.MaxSyncReturns).Result()
	if err != nil {
		return err
	}
	returns := []billing.HoldReturn{}
	returned := map[string]bool{}
	for _, id := range retIDs {
		if ref, ok := holds.refs[id]; ok {
			returns = append(returns, billing.HoldReturn{CustomerID: ref.Customer, LimitSet: ref.Set})
		}
		returned[id] = true
	}
	applied := []billing.HoldApplied{}
	for id, v := range rawApplied {
		ref, ok := holds.refs[id]
		version, err := strconv.ParseInt(v, 10, 64)
		if ok && err == nil && len(applied) < billing.MaxSyncApplied {
			applied = append(applied, billing.HoldApplied{CustomerID: ref.Customer, LimitSet: ref.Set, Version: version})
		}
	}
	var sent []sentWant
	for key, raw := range rawWants {
		if len(sent) == billing.MaxSyncWants {
			break
		}
		var w storedWant
		if json.Unmarshal([]byte(raw), &w) != nil {
			continue
		}
		if len(returned) > 0 {
			if set, ok, _ := s.setFor(ctx, ns, w.Attrs); ok && returned[holdID(w.Want.CustomerID, set)] {
				continue
			}
		}
		sent = append(sent, sentWant{key, w})
	}
	params := billing.SyncLeasesParams{LeaseID: leaseID, Usage: make([]billing.LeaseUsage, len(entries)), Wants: make([]billing.LeaseWant, len(sent)),
		Returns: returns, Committed: holds.committed, Applied: applied}
	for i, e := range entries {
		params.Usage[i] = e.item
	}
	for i, w := range sent {
		params.Wants[i] = w.want.Want
	}
	if len(sent) > 0 {
		keys := make([]string, len(sent))
		for i, w := range sent {
			keys[i] = w.key
		}
		if err := s.rdb.HDel(ctx, s.g(ns, "wants"), keys...).Err(); err != nil {
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
	if len(bad) > 0 {
		pipe.XDel(ctx, s.g(ns, "u:"+lease), bad...)
		pipe.HSet(ctx, s.g(ns, "state"), "refused", fmt.Sprintf("%d unreadable usage entries of lease %s dropped", len(bad), lease))
	}
	answers := map[string]any{}
	var gap string
	newValid := valid
	if syncErr != nil {
		fault := classify(syncErr)
		var se *billing.StatusError
		errors.As(syncErr, &se)
		for _, w := range sent {
			a := storedAnswer{Gen: gen}
			switch fault {
			case faultLeaseNotHeld:
				a.Retry = true
			case faultUnreachable:
				a.Unavailable = syncErr.Error()
			default:
				a.Error = se
			}
			answers[w.key] = mustJSON(a)
		}
		switch fault {
		case faultLeaseNotHeld:
			newValid = 1
		case faultUnreachable:
			pipe.HSet(ctx, s.g(ns, "state"), "err", time.Now().UnixMilli(), "kind", "unavailable", "cause", syncErr.Error())
		default:
			pipe.HSet(ctx, s.g(ns, "state"), "err", time.Now().UnixMilli(), "kind", "refused", "cause", syncErr.Error())
		}
	} else {
		live := res.Lease.Live
		pipe.HSet(ctx, s.g(ns, "state"), "ok", time.Now().UnixMilli())
		newValid = sentAt.Add(limits.LeaseLifetime - gateClockSkew).UnixMilli()
		// A lease the server reports dead, or whose numbering collided with
		// numbers it settled (a Redis that lost writes), is replaced.
		if !live || res.Usage.ThroughN > issued || collided(res.Usage) {
			newValid = 1
		}
		for _, a := range applied {
			pipe.HDel(ctx, s.g(ns, "applied"), holdID(a.CustomerID, a.LimitSet))
		}
		gap = s.applyAnswer(ctx, pipe, ns, lease, entries, res, returned, live, holds)
		for i, wr := range res.Wants {
			a := storedAnswer{Gen: gen}
			switch {
			case wr.Error != nil && wr.Error.Code == billing.CodeLeaseNotHeld:
				a.Retry = true
				newValid = 1
			case wr.Error != nil:
				a = storedFailure(gen, wr.Status, *wr.Error)
			case wr.LimitSet == nil:
				a.Unavailable = "a want was answered with no limit set"
			default:
				pipe.HSet(ctx, s.g(ns, "sets"), sent[i].want.Attrs, *wr.LimitSet)
				a.LimitSet = *wr.LimitSet
				if wr.Refusal != nil {
					a.Refusal, a.Sent = wr.Refusal, &sent[i].want.Want
				} else if !live || !s.ensureEvents(ctx, pipe, ns, lease, sent[i].want, *wr.LimitSet, res) {
					a.LimitSet, a.Retry = "", true
				}
			}
			answers[sent[i].key] = mustJSON(a)
		}
	}
	pipe.HSet(ctx, s.g(ns, "lease"), "valid", newValid)
	if len(answers) > 0 {
		key := s.g(ns, "answers:"+strconv.FormatInt(gen, 10))
		pipe.HSet(ctx, key, answers)
		pipe.Expire(ctx, key, gateDisplayTTL)
	}
	pipe.Set(ctx, s.g(ns, "gen"), gen, 0)
	if _, err := pipe.Exec(ctx); err != nil {
		return err
	}
	s.setLease(lease, newValid)
	s.observe(gen)
	if gap != "" {
		if err := s.moveGap(ctx, ns, lease, gap); err != nil {
			return err
		}
	}
	s.mu.Lock()
	s.syncs++
	reclaim := s.syncs%gateReclaimEvery == 0
	s.mu.Unlock()
	if reclaim {
		if err := s.reclaim(ctx, ns, lease, holds); err != nil {
			return err
		}
		if n, err := s.rdb.HLen(ctx, s.g(ns, "sets")).Result(); err == nil && n > maxGateLimitSets {
			_ = s.rdb.Del(ctx, s.g(ns, "sets")).Err()
		}
	}
	if err := s.resendEarlier(ctx, ns, lease); err != nil {
		return err
	}
	return s.closeEarlier(ctx, ns, lease)
}

// closeEarlier closes the group's abandoned leases that have no usage left
// to send (a usage sync closes the others), one empty closing sync each, so
// their holds free before the leases would expire.
func (s *sharedGate) closeEarlier(ctx context.Context, ns, current string) error {
	leases, err := s.rdb.SRandMemberN(ctx, s.g(ns, "close"), maxGateCloses).Result()
	if err != nil {
		return err
	}
	for _, lease := range leases {
		id, perr := billing.ParseLeaseID(lease)
		if perr == nil && lease != current {
			pending, err := s.rdb.XLen(ctx, s.g(ns, "u:"+lease)).Result()
			if err != nil {
				return err
			}
			if pending > 0 {
				continue
			}
			call, cancel := context.WithTimeout(ctx, syncTimeout)
			_, syncErr := s.c.SyncLeases(call, billing.SyncLeasesParams{LeaseID: id, Usage: []billing.LeaseUsage{}, Wants: []billing.LeaseWant{},
				Returns: []billing.HoldReturn{}, Committed: []billing.HoldCommitment{}, Applied: []billing.HoldApplied{}, Close: true})
			cancel()
			if syncErr != nil && classify(syncErr) == faultUnreachable {
				return nil
			}
		}
		// Closed, or refused for good: either way not tried again.
		if err := s.rdb.SRem(ctx, s.g(ns, "close"), lease).Err(); err != nil {
			return err
		}
	}
	return nil
}

// collided reports a refusal that shows the lease's numbers were issued
// twice: an item the gate never sent before settled with other terms.
func collided(r billing.LeaseUsageResult) bool {
	for _, f := range r.Refused {
		if f.N <= r.ThroughN && f.Error != nil && f.Error.Code == billing.CodeIdempotencyKeyReused {
			return true
		}
	}
	return false
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// applyAnswer settles a sync's usage and, for a live lease, applies its
// holds and display lines; it answers the stream entry from which a gap
// the server refused begins, to move to the current lease.
func (s *sharedGate) applyAnswer(ctx context.Context, pipe redis.Pipeliner, ns, lease string, entries []usageEntry, res *billing.SyncLeasesResult, returned map[string]bool, live bool, holds *groupHolds) string {
	through := res.Usage.ThroughN
	refused := map[int64]billing.LeaseUsageRefusal{}
	gapN := int64(0)
	for _, f := range res.Usage.Refused {
		if f.N > through {
			if f.Error != nil && f.Error.Code == billing.CodeUsageOutOfOrder && (gapN == 0 || f.N < gapN) {
				gapN = f.N
			}
			continue
		}
		refused[f.N] = f
	}
	type settled struct {
		tickets []string
		costs   [][]take
	}
	consumed := map[string]*settled{}
	dropped := map[string][]string{}
	var ids []string
	gap := ""
	for _, e := range entries {
		if e.item.N > through {
			if gapN != 0 && e.item.N >= gapN && gap == "" {
				gap = e.id
			}
			continue
		}
		ids = append(ids, e.id)
		f, isRefused := refused[e.item.N]
		if isRefused {
			pipe.HSet(ctx, s.g(ns, "state"), "refused", fmt.Sprintf("usage %d (%s %s) refused with status %d", f.N, e.item.Source, e.item.SourceID, f.Status))
		}
		if e.hold != "" && e.ticket != "" {
			if isRefused {
				dropped[e.hold] = append(dropped[e.hold], e.ticket)
			} else {
				c := consumed[e.hold]
				if c == nil {
					c = &settled{}
					consumed[e.hold] = c
				}
				c.tickets = append(c.tickets, e.ticket)
				c.costs = append(c.costs, e.costs)
			}
		}
		s.unsentLess(ctx, pipe, ns, e)
	}
	if len(ids) > 0 {
		pipe.XDel(ctx, s.g(ns, "u:"+lease), ids...)
	}
	for hold := range returned {
		pipe.Del(ctx, s.holdKeys(ns, hold)...)
		pipe.HDel(ctx, s.g(ns, "holds"), hold)
		pipe.HDel(ctx, s.g(ns, "applied"), hold)
		pipe.SRem(ctx, s.g(ns, "returns"), hold)
	}
	now := time.Now()
	if live {
		var order []string
		byHold := map[string][]billing.Hold{}
		for _, b := range res.Holds {
			hold := holdID(b.CustomerID, b.LimitSet)
			if byHold[hold] == nil {
				order = append(order, hold)
			}
			byHold[hold] = append(byHold[hold], b)
		}
		for _, hold := range order {
			if returned[hold] {
				// Its return released it.
				continue
			}
			b := byHold[hold]
			set := holds.set(hold, lease)
			set.apply(b, now)
			var tickets []string
			if c := consumed[hold]; c != nil {
				tickets = c.tickets
			}
			settleScript.Eval(ctx, pipe, s.holdKeys(ns, hold), settleArgs(lease, "a", &set, tickets, nil)...)
			pipe.HSet(ctx, s.g(ns, "holds"), hold, mustJSON(holdRef{Customer: b[0].CustomerID, Set: b[0].LimitSet}))
			if set.Version > 0 {
				pipe.HSet(ctx, s.g(ns, "applied"), hold, set.Version)
			}
			holds.fields[hold] = map[string]string{"lease": lease, "p": mustJSON(set)}
			delete(consumed, hold)
		}
	}
	for hold, c := range consumed {
		if f := holds.fields[hold]; live && !returned[hold] && f != nil && f["lease"] == lease && f["p"] != "" {
			// The server answers every hold whose usage it settled; one it
			// did not loses the settled cost.
			set := holds.set(hold, lease)
			for _, list := range c.costs {
				for _, t := range list {
					set.consume(t.part, t.cost)
				}
			}
			settleScript.Eval(ctx, pipe, s.holdKeys(ns, hold), settleArgs(lease, "w", &set, c.tickets, nil)...)
			continue
		}
		settleScript.Eval(ctx, pipe, s.holdKeys(ns, hold), settleArgs(lease, "d", nil, c.tickets, nil)...)
	}
	for hold, tickets := range dropped {
		settleScript.Eval(ctx, pipe, s.holdKeys(ns, hold), settleArgs(lease, "d", nil, tickets, nil)...)
	}
	display := map[billing.CustomerID][]billing.CustomerLimit{}
	for _, line := range res.Limits {
		display[line.CustomerID] = append(display[line.CustomerID], line)
	}
	for customer, lines := range display {
		pipe.Set(ctx, s.g(ns, "limits:"+customer.String()), mustJSON(lines), gateDisplayTTL)
	}
	return gap
}

// set is a hold's hold set as Redis holds it for the lease, or an empty one.
func (gh *groupHolds) set(hold, lease string) holdSet {
	var set holdSet
	if f := gh.fields[hold]; f != nil && f["lease"] == lease && f["p"] != "" {
		_ = json.Unmarshal([]byte(f["p"]), &set)
	}
	return set
}

// settleArgs are settleScript's arguments: with set, the hold's state to
// write.
func settleArgs(lease, mode string, set *holdSet, tickets, events []string) []any {
	args := []any{lease, mode, "", ""}
	var parts []billing.HoldLimit
	if set != nil {
		merged := set.merged()
		args[2] = mustJSON(set)
		if merged.Safe != nil {
			args[3] = strconv.FormatInt(*merged.Safe, 10)
		}
		parts = merged.Limits
	}
	args = append(args, len(parts))
	for _, l := range parts {
		end := ""
		if l.WindowEndsAt != nil {
			end = strconv.FormatInt(l.WindowEndsAt.UnixMilli(), 10)
		}
		args = append(args, partID(l.Limit, l.WindowStartsAt), strconv.FormatInt(l.Held, 10), end)
	}
	args = append(args, len(tickets))
	for _, t := range tickets {
		args = append(args, t)
	}
	args = append(args, len(events))
	for _, e := range events {
		args = append(args, e)
	}
	return args
}

// ensureEvents marks the want's event type as one the server priced on
// the hold an answered want names, which must be the lease's: a gate never
// makes a hold of its own. It reports whether that hold exists.
func (s *sharedGate) ensureEvents(ctx context.Context, pipe redis.Pipeliner, ns, lease string, w storedWant, set string, res *billing.SyncLeasesResult) bool {
	hold := holdID(w.Want.CustomerID, set)
	known := slices.ContainsFunc(res.Holds, func(b billing.Hold) bool { return holdID(b.CustomerID, b.LimitSet) == hold })
	if !known {
		f, err := s.rdb.HGet(ctx, s.h(ns, hold), "lease").Result()
		known = err == nil && f == lease
	}
	if known && w.EventType != "" {
		settleScript.Eval(ctx, pipe, s.holdKeys(ns, hold), settleArgs(lease, "e", nil, nil, []string{w.EventType})...)
	}
	return known
}

func (s *sharedGate) unsentLess(ctx context.Context, pipe redis.Pipeliner, ns string, e usageEntry) {
	if e.unsent == "" {
		return
	}
	key := s.g(ns, "unsent:"+e.item.CustomerID.String())
	for _, kv := range strings.Split(e.unsent, ";") {
		k, v, ok := strings.Cut(kv, "=")
		if c, err := strconv.ParseInt(v, 10, 64); ok && err == nil && c != 0 {
			pipe.HIncrBy(ctx, key, k, -c)
		}
	}
}

// moveGap numbers a lease's entries from the gap the server refused on on
// the group's current lease, after replacing the lease when it is the gap's.
func (s *sharedGate) moveGap(ctx context.Context, ns, from, startID string) error {
	cur, err := s.rdb.HGet(ctx, s.g(ns, "lease"), "id").Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return err
	}
	if cur == "" || cur == from {
		replaced := cur
		cur = newLeaseID().String()
		if _, err := s.rdb.TxPipelined(ctx, func(p redis.Pipeliner) error {
			p.HSet(ctx, s.g(ns, "lease"), "id", cur, "valid", 0)
			p.Del(ctx, s.g(ns, "applied"))
			if replaced != "" {
				p.SAdd(ctx, s.g(ns, "close"), replaced)
			}
			return nil
		}); err != nil {
			return err
		}
		s.setLease(cur, 0)
	}
	keys := []string{s.g(ns, "lease"), s.g(ns, "u:"+from), s.g(ns, "u:"+cur), s.g(ns, "leases")}
	for {
		moved, err := moveScript.Run(ctx, s.rdb, keys, cur, startID).Int64()
		if err != nil || moved <= 0 {
			return err
		}
	}
}

// resendEarlier sends the usage of the group's earlier leases, each sync
// closing its lease.
func (s *sharedGate) resendEarlier(ctx context.Context, ns, current string) error {
	leases, err := s.rdb.SMembers(ctx, s.g(ns, "leases")).Result()
	if err != nil {
		return err
	}
	for _, lease := range leases {
		if lease == current {
			continue
		}
		entries, _, bad, err := s.readUsage(ctx, ns, lease)
		if err != nil {
			return err
		}
		if len(bad) > 0 {
			if err := s.rdb.XDel(ctx, s.g(ns, "u:"+lease), bad...).Err(); err != nil {
				return err
			}
		}
		if len(entries) == 0 {
			if err := emptyLeaseScript.Run(ctx, s.rdb, []string{s.g(ns, "leases"), s.g(ns, "u:"+lease), s.g(ns, "lease")}, lease).Err(); err != nil {
				return err
			}
			continue
		}
		id, err := billing.ParseLeaseID(lease)
		if err != nil {
			continue
		}
		params := billing.SyncLeasesParams{LeaseID: id, Usage: make([]billing.LeaseUsage, len(entries)), Wants: []billing.LeaseWant{}, Returns: []billing.HoldReturn{},
			Committed: []billing.HoldCommitment{}, Applied: []billing.HoldApplied{}, Close: true}
		for i, e := range entries {
			params.Usage[i] = e.item
		}
		call, cancel := context.WithTimeout(ctx, syncTimeout)
		res, syncErr := s.c.SyncLeases(call, params)
		cancel()
		var se *billing.StatusError
		switch {
		case syncErr == nil:
		case classify(syncErr) == faultUnreachable:
			return nil
		case errors.As(syncErr, &se) && se.Code == billing.CodeLeaseNotFound:
			// A forgotten lease: its items go on the current one, where any
			// too old to bill are refused alone.
			if err := s.moveGap(ctx, ns, lease, entries[0].id); err != nil {
				return err
			}
			continue
		default:
			// Kept, and sent again.
			continue
		}
		pipe := s.rdb.Pipeline()
		gap := s.applyAnswer(ctx, pipe, ns, lease, entries, &billing.SyncLeasesResult{Usage: res.Usage}, nil, false, &groupHolds{fields: map[string]map[string]string{}})
		pipe.SRem(ctx, s.g(ns, "close"), lease)
		if _, err := pipe.Exec(ctx); err != nil {
			return err
		}
		if gap != "" {
			if err := s.moveGap(ctx, ns, lease, gap); err != nil {
				return err
			}
		}
	}
	return nil
}

// readHolds reads the group's holds: their refs, each hold hash and what
// the lease's open tickets took of them, as the sync is built.
func (s *sharedGate) readHolds(ctx context.Context, ns, lease string) (*groupHolds, error) {
	raw, err := s.rdb.HGetAll(ctx, s.g(ns, "holds")).Result()
	if err != nil {
		return nil, err
	}
	out := &groupHolds{refs: map[string]holdRef{}, fields: map[string]map[string]string{}, committed: []billing.HoldCommitment{}}
	ids := make([]string, 0, len(raw))
	for id, v := range raw {
		var ref holdRef
		if json.Unmarshal([]byte(v), &ref) == nil {
			out.refs[id] = ref
			ids = append(ids, id)
		}
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
	for i, id := range ids {
		f := fields[i].Val()
		out.fields[id] = f
		if f["lease"] != lease {
			continue
		}
		ref := out.refs[id]
		c := billing.HoldCommitment{CustomerID: ref.Customer, LimitSet: ref.Set}
		for k, v := range f {
			part, ok := strings.CutPrefix(k, "o:")
			if n, _ := strconv.ParseInt(v, 10, 64); ok && n > 0 {
				c.Limits = append(c.Limits, billing.LimitCommitment{Limit: partLimit(part), WindowStartsAt: partStart(part), Committed: n})
			}
		}
		if len(c.Limits) > 0 && len(out.committed) < billing.MaxSyncCommitted {
			out.committed = append(out.committed, c)
		}
	}
	return out, nil
}

// evict queues the least recently used idle holds past the bound for
// return: one script finds each idle and marks it gone, so no decision
// takes from a hold being returned.
func (s *sharedGate) evict(ctx context.Context, ns, lease string, holds *groupHolds) error {
	if len(holds.refs) <= maxGateHolds {
		return nil
	}
	type candidate struct {
		id   string
		used string
	}
	var idle []candidate
	var gone []any
	for id, f := range holds.fields {
		switch f["lease"] {
		case lease:
			idle = append(idle, candidate{id, f["used"]})
		case "":
			gone = append(gone, id)
		}
	}
	slices.SortFunc(idle, func(a, b candidate) int {
		x, _ := strconv.ParseInt(a.used, 10, 64)
		y, _ := strconv.ParseInt(b.used, 10, 64)
		return cmp.Compare(x, y)
	})
	over := len(holds.refs) - maxGateHolds - len(gone)
	for _, c := range idle {
		if over <= 0 {
			break
		}
		marked, err := evictScript.Run(ctx, s.rdb, s.holdKeys(ns, c.id), lease, c.used).Int64()
		if err != nil {
			return err
		}
		if marked == 1 {
			gone = append(gone, c.id)
			over--
		}
	}
	if len(gone) == 0 {
		return nil
	}
	return s.rdb.SAdd(ctx, s.g(ns, "returns"), gone...).Err()
}

// reclaim gives back what dead processes' open tickets took, never this
// process's own, and forgets idle holds of earlier leases.
func (s *sharedGate) reclaim(ctx context.Context, ns, lease string, holds *groupHolds) error {
	procs, err := s.rdb.HGetAll(ctx, s.g(ns, "procs")).Result()
	if err != nil {
		return err
	}
	cutoff := time.Now().Add(-gateProcTTL).UnixMilli()
	dead := map[string]bool{}
	for proc, at := range procs {
		if ms, _ := strconv.ParseInt(at, 10, 64); ms < cutoff && proc != s.proc {
			dead[proc] = true
		}
	}
	ids := make([]string, 0, len(holds.refs))
	for id := range holds.refs {
		ids = append(ids, id)
	}
	if len(dead) > 0 {
		tickets := make([]*redis.MapStringStringCmd, len(ids))
		if _, err := s.rdb.Pipelined(ctx, func(p redis.Pipeliner) error {
			for i, id := range ids {
				tickets[i] = p.HGetAll(ctx, s.h(ns, id)+":t")
			}
			return nil
		}); err != nil && !errors.Is(err, redis.Nil) {
			return err
		}
		for i, id := range ids {
			var lost []any
			for ticket, v := range tickets[i].Val() {
				if fields := strings.SplitN(v, "|", 3); len(fields) == 3 && fields[0] == "o" && dead[fields[1]] {
					lost = append(lost, ticket)
				}
			}
			if len(lost) > 0 {
				if err := reclaimScript.Run(ctx, s.rdb, s.holdKeys(ns, id), lost...).Err(); err != nil {
					return err
				}
			}
		}
		stale := make([]string, 0, len(dead))
		for proc := range dead {
			stale = append(stale, proc)
		}
		if err := s.rdb.HDel(ctx, s.g(ns, "procs"), stale...).Err(); err != nil {
			return err
		}
	}
	for _, id := range ids {
		if l := holds.fields[id]["lease"]; l == lease || l == "" {
			continue
		}
		forgot, err := forgetScript.Run(ctx, s.rdb, s.holdKeys(ns, id), lease).Int64()
		if err != nil {
			return err
		}
		if forgot == 1 {
			if err := s.rdb.HDel(ctx, s.g(ns, "holds"), id).Err(); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *sharedGate) limits(ctx context.Context, customer billing.CustomerID) ([]billing.CustomerLimit, error) {
	ns, ok := s.namespace()
	if !ok || s.down() {
		return s.fallback().limits(ctx, customer)
	}
	raw, err := s.rdb.Get(ctx, s.g(ns, "limits:"+customer.String())).Result()
	if s.redisFailed(err) {
		return s.fallback().limits(ctx, customer)
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
// call, within ctx (bounded when it has no deadline). Usage in a Redis this
// process cannot reach is not waited for: it is lost with Redis, never
// billed.
func (s *sharedGate) flush(ctx context.Context) error {
	ns, ok := s.namespace()
	if !ok || s.down() {
		return nil
	}
	ctx, cancel := boundContext(ctx)
	defer cancel()
	leases, err := s.rdb.SMembers(ctx, s.g(ns, "leases")).Result()
	if s.redisFailed(err) {
		return nil
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
				return nil
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
		state, err := s.rdb.HMGet(ctx, s.g(ns, "state"), "ok", "err", "kind", "cause").Result()
		if err == nil && len(state) == 4 {
			okAt, _ := strconv.ParseInt(fmt.Sprint(state[0]), 10, 64)
			errAt, _ := strconv.ParseInt(fmt.Sprint(state[1]), 10, 64)
			if errAt > started && errAt > okAt {
				if state[2] == "unavailable" {
					return unavailable(fmt.Errorf("%w: %v", billing.ErrUnreachable, state[3]))
				}
				return fmt.Errorf("openrails: gate: sync refused: %v", state[3])
			}
		}
		if time.Since(kicked) >= limits.SyncInterval {
			_ = s.rdb.LPush(ctx, s.g(ns, "kick"), "1").Err()
			kicked = time.Now()
		}
		select {
		case <-ctx.Done():
			return waitErr(ctx)
		case <-t.C:
		}
	}
}

// close flushes and leaves the group: its lease lives while another process
// syncs it, and the last process to leave closes it. The process's own
// lease closes too.
func (s *sharedGate) close(ctx context.Context) error {
	err := s.flush(ctx)
	close(s.stop)
	s.cancel()
	stopped := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-ctx.Done():
		return errors.Join(err, waitErr(ctx))
	}
	if ns, ok := s.namespace(); ok {
		others, lerr := leaveScript.Run(ctx, s.rdb, []string{s.g(ns, "procs")}, s.proc, time.Now().Add(-gateProcTTL).UnixMilli()).Int64()
		if lerr == nil && others == 0 {
			err = errors.Join(err, s.closeGroup(ctx, ns))
		}
	}
	return errors.Join(err, s.fallback().close(ctx))
}

// closeGroup closes the group's lease with the usage left in its stream,
// under the group's lock; a process that joins later starts a new lease. A
// lease another process holds the lock of, or that already died, is left to
// it.
func (s *sharedGate) closeGroup(ctx context.Context, ns string) error {
	locked, err := s.rdb.SetNX(ctx, s.g(ns, "lock"), s.proc, gateLockTTL).Result()
	if err != nil || !locked {
		return nil
	}
	defer func() {
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), syncTimeout)
		_ = unlockScript.Run(c, s.rdb, []string{s.g(ns, "lock")}, s.proc).Err()
		cancel()
	}()
	info, err := s.rdb.HMGet(ctx, s.g(ns, "lease"), "id", "valid").Result()
	if err != nil {
		return nil
	}
	lease, _ := info[0].(string)
	valid, _ := strconv.ParseInt(fmt.Sprint(info[1]), 10, 64)
	id, perr := billing.ParseLeaseID(lease)
	if perr != nil || valid <= 1 || time.Now().UnixMilli() >= valid {
		return nil
	}
	for batch := 0; batch < 10; batch++ {
		entries, _, _, err := s.readUsage(ctx, ns, lease)
		if err != nil {
			return nil
		}
		params := billing.SyncLeasesParams{LeaseID: id, Usage: make([]billing.LeaseUsage, len(entries)), Wants: []billing.LeaseWant{}, Returns: []billing.HoldReturn{},
			Committed: []billing.HoldCommitment{}, Applied: []billing.HoldApplied{}, Close: true}
		for i, e := range entries {
			params.Usage[i] = e.item
		}
		call, cancel := context.WithTimeout(ctx, syncTimeout)
		res, err := s.c.SyncLeases(call, params)
		cancel()
		if err != nil {
			if classify(err) == faultUnreachable {
				return unavailable(err)
			}
			return err
		}
		pipe := s.rdb.Pipeline()
		s.applyAnswer(ctx, pipe, ns, lease, entries, &billing.SyncLeasesResult{Usage: res.Usage}, nil, false, &groupHolds{fields: map[string]map[string]string{}})
		pipe.HSet(ctx, s.g(ns, "lease"), "valid", 1)
		pipe.Del(ctx, s.g(ns, "returns"), s.g(ns, "applied"))
		if _, err := pipe.Exec(ctx); err != nil {
			return err
		}
		if len(entries) < billing.MaxSyncUsageItems {
			return nil
		}
	}
	return nil
}

// Scripts. Costs and held units are integers below 2^53, sent as decimal
// strings; a script reads and checks everything it needs before its first
// write, so it applies whole or not at all.
const luaCosts = `
local function costs(v) return string.match(v, '^[oc]|[^|]*|(.*)$') or '' end
local function parse(s)
  local out = {}
  for id, c in string.gmatch(s, '([^=;]+)=([^;]*)') do
    local v = tonumber(c)
    if not v or v ~= math.floor(v) then return nil end
    out[#out + 1] = {id, v}
  end
  return out
end
local function add(h, prefix, list, sign)
  for _, e in ipairs(list) do
    if e[2] ~= 0 then redis.call('HINCRBY', h, prefix .. e[1], string.format('%d', sign * e[2])) end
  end
end
`

// takeScript: KEYS hold, tickets; ARGV lease, ver, ticket, process, live,
// safe cost (empty: none), n, then n of (part, cost, mode 0 whole estimate | 1
// while positive, plus 2 for a unit limit). Short of a live hold, the safe
// amount pays the money cost in full; units are taken only from a live hold
// that fits them. A zero take writes nothing. Answers {1, low, takes...}, {0}
// not this lease's hold (or none to take from), {-1} stale pricing, {-2} a
// part without a current window, {-3} short.
var takeScript = redis.NewScript(`
local h = KEYS[1]
local v = redis.call('HMGET', h, 'lease', 'ver')
if v[1] ~= ARGV[1] then return {0} end
if v[2] ~= ARGV[2] then return {-1} end
local n = tonumber(ARGV[7])
if not n or n < 1 then return {0} end
local t = redis.call('TIME')
local now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
local takes = {}
local live = ARGV[5] == '1'
local short = not live
local unitShort = false
local low = 0
for i = 0, n - 1 do
  local id, cost, mode = ARGV[8 + i * 3], tonumber(ARGV[9 + i * 3]), ARGV[10 + i * 3]
  if not cost or cost < 0 then return {-2} end
  local f = redis.call('HMGET', h, 'h:' .. id, 'o:' .. id, 'u:' .. id, 'e:' .. id)
  local held, open, unsent = tonumber(f[1]), tonumber(f[2] or '0'), tonumber(f[3] or '0')
  if not held or not open or not unsent then return {-2} end
  if f[4] and f[4] ~= '' and now >= (tonumber(f[4]) or 0) then return {-2} end
  local avail = held - open - unsent
  local take = cost
  local fits = live
  if mode == '1' or mode == '3' then
    if avail <= 0 then fits = false elseif avail < cost then take = avail end
  elseif avail < cost then
    fits = false
  end
  if not fits then
    short = true
    if (mode == '2' or mode == '3') and cost > 0 then unitShort = true end
  end
  if avail - cost < cost then low = 1 end
  takes[i + 1] = take
end
local safe = nil
if short then
  if ARGV[6] == '' or unitShort then return {-3} end
  safe = tonumber(ARGV[6])
  local sv = redis.call('HMGET', h, 'safe', 'safe_spent')
  local cap, spent = tonumber(sv[1] or ''), tonumber(sv[2] or '0')
  if not safe or not cap or not spent or cap - spent < safe then return {-3} end
  for i = 0, n - 1 do
    local mode = ARGV[10 + i * 3]
    if mode == '0' or mode == '1' then takes[i + 1] = tonumber(ARGV[9 + i * 3]) end
  end
  low = 1
end
if safe and safe ~= 0 then redis.call('HINCRBY', h, 'safe_spent', string.format('%d', safe)) end
local rec = {}
local out = {1, low}
for i = 0, n - 1 do
  local id = ARGV[8 + i * 3]
  local c = string.format('%d', takes[i + 1])
  if takes[i + 1] ~= 0 then redis.call('HINCRBY', h, 'o:' .. id, c) end
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
local list = parse(costs(v))
if not list then return redis.error_reply('release: bad ticket') end
add(KEYS[1], 'o:', list, -1)
redis.call('HDEL', KEYS[2], ARGV[1])
return 1
`)

// captureScript: KEYS hold, tickets; ARGV ticket, n, actual costs. Turns an
// open ticket's take into its usage's cost; a ticket already captured, or
// gone (reclaimed, or settled), is left as it is.
var captureScript = redis.NewScript(luaCosts + `
local v = redis.call('HGET', KEYS[2], ARGV[1])
if not v or string.sub(v, 1, 1) ~= 'o' then return 0 end
local taken, actual = parse(costs(v)), parse(ARGV[3])
if not taken or not actual then return redis.error_reply('capture: bad costs') end
add(KEYS[1], 'o:', taken, -1)
add(KEYS[1], 'u:', actual, 1)
redis.call('HSET', KEYS[2], ARGV[1], 'c|' .. ARGV[2] .. '|' .. ARGV[3])
return 1
`)

// reclaimScript: KEYS hold, tickets; ARGV open tickets of dead processes.
var reclaimScript = redis.NewScript(luaCosts + `
local lost = {}
for _, ticket in ipairs(ARGV) do
  local v = redis.call('HGET', KEYS[2], ticket)
  if v and string.sub(v, 1, 1) == 'o' then
    local list = parse(costs(v))
    if not list then return redis.error_reply('reclaim: bad ticket') end
    lost[#lost + 1] = {ticket, list}
  end
end
for _, l in ipairs(lost) do
  add(KEYS[1], 'o:', l[2], -1)
  redis.call('HDEL', KEYS[2], l[1])
end
return #lost
`)

// settleScript: KEYS hold, tickets; ARGV lease, mode, hold set, safe, then
// parts (count, then id, held, end ms), settled tickets (count, then ids),
// event types (count, then types). A settled ticket's cost leaves what it
// counts in (open, or unsettled once captured) when the hold is the lease's.
// Mode a writes an answered hold (its safe amount spent anew), w writes the
// hold's state after usage the answer left out, d only settles tickets, e
// only adds event types the server priced to the lease's hold. Writing a
// hold of another lease (or none) starts it over. Every write but e bumps
// ver; e bumps it for a new event type.
var settleScript = redis.NewScript(luaCosts + `
local h, t = KEYS[1], KEYS[2]
local lease, mode = ARGV[1], ARGV[2]
local i = 5
local np = tonumber(ARGV[i])
if not np then return redis.error_reply('settle: parts') end
local parts, order = {}, {}
for j = 1, np do
  if not tonumber(ARGV[i + 2]) then return redis.error_reply('settle: held') end
  parts[ARGV[i + 1]] = {ARGV[i + 2], ARGV[i + 3]}
  order[#order + 1] = ARGV[i + 1]
  i = i + 3
end
i = i + 1
local nt = tonumber(ARGV[i])
if not nt then return redis.error_reply('settle: tickets') end
local tickets = {}
for j = 1, nt do tickets[j] = ARGV[i + j] end
i = i + nt + 1
local ne = tonumber(ARGV[i] or '0')
if not ne then return redis.error_reply('settle: events') end
local events = {}
for j = 1, ne do events[j] = ARGV[i + j] end
local cur = redis.call('HGET', h, 'lease')
if mode == 'e' and cur ~= lease then return 0 end
local write = mode == 'a' or mode == 'w'
local reset = write and cur ~= lease
local settle = {}
if not reset then
  for _, ticket in ipairs(tickets) do
    local v = redis.call('HGET', t, ticket)
    if v then
      local list = parse(costs(v))
      if not list then return redis.error_reply('settle: bad ticket') end
      local field = 'u:'
      if string.sub(v, 1, 1) == 'o' then field = 'o:' end
      settle[#settle + 1] = {ticket, field, list}
    end
  end
end
local stale = {}
if write and not reset then
  for _, f in ipairs(redis.call('HKEYS', h)) do
    local kind, id = string.sub(f, 1, 2), string.sub(f, 3)
    if (kind == 'h:' or kind == 'o:' or kind == 'u:') and not parts[id] then stale[id] = true end
  end
end
if reset then redis.call('DEL', h, t) end
for _, s in ipairs(settle) do
  if cur == lease then add(h, s[2], s[3], -1) end
  redis.call('HDEL', t, s[1])
end
local bump = false
for _, e in ipairs(events) do
  if redis.call('HSETNX', h, 'ev:' .. e, 1) == 1 then bump = true end
end
if write then
  if mode == 'a' or reset then
    redis.call('HSET', h, 'lease', lease, 'p', ARGV[3], 'safe', ARGV[4], 'safe_spent', 0)
  else
    redis.call('HSET', h, 'lease', lease, 'p', ARGV[3], 'safe', ARGV[4])
  end
  for id in pairs(stale) do
    local ou = redis.call('HMGET', h, 'o:' .. id, 'u:' .. id)
    if (tonumber(ou[1] or '0') or 1) == 0 and (tonumber(ou[2] or '0') or 1) == 0 then
      redis.call('HDEL', h, 'h:' .. id, 'o:' .. id, 'u:' .. id, 'e:' .. id)
    else
      redis.call('HSET', h, 'h:' .. id, 0)
    end
  end
  for _, id in ipairs(order) do
    redis.call('HSET', h, 'h:' .. id, parts[id][1], 'e:' .. id, parts[id][2])
  end
  bump = true
end
if bump then redis.call('HINCRBY', h, 'ver', 1) end
return 1
`)

// usageScript: KEYS lease, stream, leases, write key, unsent; ARGV lease,
// item, hold, ticket, unsent costs, part costs, write key TTL (ms). Numbers
// the item in the group's current lease once per write key: {1, lease|n}
// written (now or by an earlier try), {0, current lease} not written, the
// lease being another. The write key outlives the item's settlement.
var usageScript = redis.NewScript(`
local done = redis.call('GET', KEYS[4])
if done then return {1, done} end
local cur = redis.call('HGET', KEYS[1], 'id')
if cur ~= ARGV[1] then return {0, cur or ''} end
local ttl = tonumber(ARGV[7])
if not ttl or ttl < 1 then return redis.error_reply('usage: bad key ttl') end
local unsent = {}
for k, c in string.gmatch(ARGV[5], '([^=;]+)=([^;]*)') do
  local v = tonumber(c)
  if not v or v ~= math.floor(v) then return redis.error_reply('usage: bad unsent cost') end
  if v ~= 0 then unsent[#unsent + 1] = {k, string.format('%d', v)} end
end
local n = redis.call('HINCRBY', KEYS[1], 'n:' .. ARGV[1], 1)
local ref = ARGV[1] .. '|' .. string.format('%d', n)
redis.call('XADD', KEYS[2], '*', 'n', string.format('%d', n), 'item', ARGV[2], 'hold', ARGV[3], 'ticket', ARGV[4], 'unsent', ARGV[5], 'costs', ARGV[6])
redis.call('SADD', KEYS[3], ARGV[1])
redis.call('SET', KEYS[4], ref, 'PX', string.format('%d', ttl))
for _, kc in ipairs(unsent) do redis.call('HINCRBY', KEYS[5], kc[1], kc[2]) end
if #unsent > 0 then redis.call('PEXPIRE', KEYS[5], 86400000) end
return {1, ref}
`)

// moveScript: KEYS lease, from stream, to stream, leases; ARGV to lease
// (the group's current), first entry id. Numbers up to 1000 entries from
// that id on again on the current lease; answers how many moved, -1 when
// the lease is not the group's. A moved item's write key still says it was
// written.
var moveScript = redis.NewScript(`
if redis.call('HGET', KEYS[1], 'id') ~= ARGV[1] then return -1 end
local entries = redis.call('XRANGE', KEYS[2], ARGV[2], '+', 'COUNT', 1000)
for _, e in ipairs(entries) do
  local n = string.format('%d', redis.call('HINCRBY', KEYS[1], 'n:' .. ARGV[1], 1))
  local fields = {}
  for j = 1, #e[2], 2 do
    local f, v = e[2][j], e[2][j + 1]
    if f == 'n' then v = n end
    fields[#fields + 1] = f
    fields[#fields + 1] = v
  end
  redis.call('XADD', KEYS[3], '*', unpack(fields))
  redis.call('XDEL', KEYS[2], e[1])
end
if #entries > 0 then redis.call('SADD', KEYS[4], ARGV[1]) end
return #entries
`)

// evictScript: KEYS hold, tickets; ARGV lease, used as read. Marks an idle
// hold of the lease gone, unless a decision or ticket touched it since.
var evictScript = redis.NewScript(`
local v = redis.call('HMGET', KEYS[1], 'lease', 'used')
if v[1] ~= ARGV[1] or (v[2] or '') ~= ARGV[2] then return 0 end
if redis.call('HLEN', KEYS[2]) > 0 then return 0 end
redis.call('HSET', KEYS[1], 'lease', '')
return 1
`)

// forgetScript: KEYS hold, tickets; ARGV current lease. Deletes a hold of
// an earlier lease that no ticket counts on.
var forgetScript = redis.NewScript(`
local cur = redis.call('HGET', KEYS[1], 'lease')
if cur == ARGV[1] or cur == '' then return 0 end
if redis.call('HLEN', KEYS[2]) > 0 then return 0 end
redis.call('DEL', KEYS[1], KEYS[2])
return 1
`)

// wantScript: KEYS wants, kick, gen; ARGV want key, want. Answers the sync
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

// leaveScript: KEYS procs; ARGV process, cutoff (unix ms). Takes the
// process out of the group and answers how many others beat since cutoff.
var leaveScript = redis.NewScript(`
redis.call('HDEL', KEYS[1], ARGV[1])
local n = 0
local all = redis.call('HGETALL', KEYS[1])
for i = 2, #all, 2 do
  if tonumber(all[i]) and tonumber(all[i]) >= tonumber(ARGV[2]) then n = n + 1 end
end
return n
`)

// emptyLeaseScript: KEYS leases, stream, lease; ARGV lease. Forgets an
// earlier lease whose usage all settled, with its numbering.
var emptyLeaseScript = redis.NewScript(`
if redis.call('XLEN', KEYS[2]) == 0 and redis.call('HGET', KEYS[3], 'id') ~= ARGV[1] then
  redis.call('SREM', KEYS[1], ARGV[1])
  redis.call('DEL', KEYS[2])
  redis.call('HDEL', KEYS[3], 'n:' .. ARGV[1])
end
return 1
`)
