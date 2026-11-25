package openrails

import (
	"strings"
	"sync"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/modules/limits"
)

// View bounds. None is a setting: the bound holds for any value, and only
// speed depends on them.
const (
	// viewFresh is how long a line decides freely after the sync that
	// answered it was sent; past it, its customer's requests are decided one
	// at a time until a sync answers again.
	viewFresh = 2 * limits.SyncInterval
	// viewKeep is how long a view no request uses is kept.
	viewKeep = 10 * time.Minute
	// maxGateViews bounds the views a gate keeps; past it the least recently
	// used go.
	maxGateViews = 10_000
	// maxGateSets bounds the cached attributes-to-limit-set map.
	maxGateSets = 100_000
)

type viewKey struct {
	customer billing.CustomerID
	currency string
}

// custView is the gate's view of one customer in one currency and what it
// has in flight against it. Its mutex serializes the decisions on it: each
// takes from every limit it claims or from none.
type custView struct {
	key viewKey

	mu    sync.Mutex
	lines map[string]*lineState
	// probing is a stale view deciding its one request until a sync answers.
	probing bool
	used    time.Time
}

// lineState is one limit of a view: the line as last answered, its windows
// and what the gate has in flight in each. key and money never change, so
// pricing reads them without the view's lock.
type lineState struct {
	key   string
	line  billing.ViewLimit
	money bool
	// asOf is when the sync that answered the line was sent, by the gate's
	// clock.
	asOf time.Time
	// cur is the line's window as answered (no bounds: one that never ends);
	// next the window after it, once the gate decided in it, or a session the
	// gate opened.
	cur, next *span
}

// span is one window of a line and what the gate has in it: open tickets'
// takes and captured usage not yet settled. base and avail bound offline
// spending: what the gate had in it when the view was answered, and what
// was left for every gate's new requests (below zero when nothing was), of
// which each gate spends its share.
type span struct {
	start, end   *time.Time
	remaining    int64
	open, unsent int64
	base, avail  int64
	local        bool
}

func (s *span) inFlight() int64 { return s.open + s.unsent }

// take is what a decision took from one window of one limit; limit and
// unit name it without the view's lock.
type take struct {
	line        *lineState
	span        *span
	cost        int64
	limit, unit string
}

func takeOf(l *lineState, s *span, cost int64) take {
	return take{line: l, span: s, cost: cost, limit: l.key, unit: l.line.Unit}
}

func newCustView(k viewKey) *custView {
	return &custView{key: k, lines: map[string]*lineState{}}
}

// opensAtUse reports a limit whose window a use opens: a session, or a
// first-use fixed window.
func opensAtUse(r *catalog.LimitReset) bool {
	return r != nil && (r.Kind == catalog.ResetSession || r.Kind == catalog.ResetFixed && r.Anchor == catalog.AnchorFirstUse)
}

func before(now time.Time, end *time.Time) bool { return end == nil || now.Before(*end) }

// spanAt is the line's window a request at now counts in: the answered one
// while it lasts, else the next (a tile the view named, or a session opened
// at this use). ok is false when the gate does not know it.
func (l *lineState) spanAt(now time.Time) (*span, bool) {
	open := l.cur != nil && (l.cur.end != nil || !opensAtUse(l.line.Reset))
	if open && before(now, l.cur.end) {
		return l.cur, true
	}
	if l.next != nil && (l.next.start == nil || !now.Before(*l.next.start)) && before(now, l.next.end) {
		return l.next, true
	}
	switch {
	case l.line.Next != nil && l.line.Next.StartsAt != nil:
		s := &span{start: l.line.Next.StartsAt, end: l.line.Next.EndsAt, remaining: l.line.Next.Remaining}
		if now.Before(*s.start) || !before(now, s.end) {
			return nil, false
		}
		s.avail = s.remaining
		l.next = s
	case opensAtUse(l.line.Reset):
		remaining := l.line.Remaining
		if l.line.Next != nil {
			remaining = l.line.Next.Remaining
		}
		start := now.UTC().Truncate(time.Second)
		end := start.Add(time.Duration(l.line.Reset.WindowSeconds) * time.Second)
		l.next = &span{start: &start, end: &end, remaining: remaining, local: true, avail: remaining}
	default:
		return nil, false
	}
	return l.next, true
}

// room is what the window leaves for a request at now: the answered
// remaining, less what the gate has in flight in it, drained by running
// charges or refilled by a rate limit since the answer. capacity is what the
// request's policy gives a spend window, where it differs from the line's.
func (l *lineState) room(s *span, now time.Time, creditLine int64, capacity *int64) int64 {
	r := s.remaining
	if capacity != nil {
		r = satAdd(r, *capacity-l.line.Capacity)
	}
	if s == l.cur && l.line.Reset != nil && l.line.Reset.Kind == catalog.ResetRate {
		level := limits.RateLevel(l.line.Used, l.line.Capacity, l.line.Reset.WindowSeconds, l.asOf, now)
		r = l.line.Capacity - level - l.line.Held
	}
	if l.line.Limit == catalog.BalanceLimit {
		r = satAdd(r, -drained(l.line.DrainPerHour, now.Sub(l.asOf)))
		r = satAdd(r, creditLine)
	}
	return satAdd(r, -s.inFlight())
}

// capacityOf is the capacity a set's policy gives a limit, nil for one it
// does not name.
func capacityOf(set billing.LimitSet, limit string) *int64 {
	for _, c := range set.Capacities {
		if c.Limit == limit {
			return &c.Capacity
		}
	}
	return nil
}

func drained(perHour int64, d time.Duration) int64 {
	if perHour <= 0 || d <= 0 {
		return 0
	}
	n, err := ceilMulDiv(perHour, int64(d), int64(time.Hour))
	if err != nil {
		return maxGateAmount
	}
	return n
}

// share is this gate's part of what was left for every gate's new requests
// when the view was answered: under the request's policy (its credit line,
// its cap on a spend window), less what running charges drained since.
func (l *lineState) share(s *span, set billing.LimitSet, now time.Time, gates int) int64 {
	pool := s.avail
	if c := capacityOf(set, l.key); c != nil {
		pool = satAdd(pool, *c-l.line.Capacity)
	}
	if l.key == catalog.BalanceLimit {
		pool = satAdd(pool, set.CreditLine)
		pool = satAdd(pool, -drained(l.line.DrainPerHour, now.Sub(l.asOf)))
	}
	return max(pool, 0) / int64(max(gates, 1))
}

// verdict is what a view decides of a request.
type verdict uint8

const (
	verdictLocal verdict = iota
	verdictRefuse
	verdictCold
	verdictWait
	verdictShort
)

// outage is what a gate knows of OpenRails being unreachable: since when
// (zero while it is reachable), and how many gates were live at its last
// answer.
type outage struct {
	since time.Time
	gates int
}

func (o outage) on() bool { return !o.since.IsZero() }

// decision is a view's verdict on one request and, admitted, its takes.
type decision struct {
	verdict verdict
	refusal *billing.Refusal
	takes   []take
}

// decide takes a request's costs from every limit its set claims, or from
// none, against the view less what the gate has in flight: a limit refuses
// a request that does not fit it, and the first refusal in the set's order
// wins. Nothing is decided on the server, so what other gates admit unseen
// may take a limit past its line, by about what they admit in four sync
// intervals. Offline, a limit fits only within the gate's share of it, of a
// view answered about when the outage began. A stale view decides one
// request (probe) and holds the rest for a sync.
func (v *custView) decide(set billing.LimitSet, costs []int64, now time.Time, down outage) decision {
	offline := down.on()
	v.mu.Lock()
	defer v.mu.Unlock()
	v.used = now
	lines := make([]*lineState, len(set.Limits))
	stale := false
	for i, key := range set.Limits {
		if lines[i] = v.lines[key]; lines[i] == nil {
			return decision{verdict: verdictCold}
		}
		stale = stale || now.Sub(lines[i].asOf) > viewFresh
	}
	if stale && !offline && v.probing {
		return decision{verdict: verdictWait}
	}
	out := decision{verdict: verdictLocal}
	takes := make([]take, len(lines))
	for i, l := range lines {
		s, ok := l.spanAt(now)
		if !ok {
			return decision{verdict: verdictCold}
		}
		c := costs[i]
		awp := l.line.Overrun == catalog.OverrunAdmitWhilePositive
		takes[i] = takeOf(l, s, c)
		r := l.room(s, now, set.CreditLine, capacityOf(set, l.key))
		if awp && r <= 0 || !awp && r < c {
			if out.verdict != verdictRefuse {
				out = decision{verdict: verdictRefuse, refusal: refusalOf(set, l, s, now)}
			}
			continue
		}
		if out.verdict == verdictRefuse {
			continue
		}
		if offline {
			// A view older than the outage does not know what was left.
			if l.asOf.Before(down.since.Add(-viewFresh)) {
				out.verdict = verdictShort
				continue
			}
			spent := satAdd(s.inFlight(), -s.base)
			share := l.share(s, set, now, down.gates)
			if awp && spent >= share || !awp && satAdd(spent, c) > share {
				out.verdict = verdictShort
			}
			continue
		}
	}
	if out.verdict != verdictLocal {
		return out
	}
	if stale && !offline {
		v.probing = true
	}
	out.takes = takes
	for _, t := range out.takes {
		t.span.open = satAdd(t.span.open, t.cost)
	}
	return out
}

// refusalOf is the refusal a limit answers: balance's deny code, a spend
// window's budget_exceeded, or limit_reached with when it resets.
func refusalOf(set billing.LimitSet, l *lineState, s *span, now time.Time) *billing.Refusal {
	switch {
	case l.line.Limit == catalog.BalanceLimit:
		return &billing.Refusal{DenyCode: set.BalanceDenyCode}
	case strings.HasPrefix(l.line.Limit, limits.SpendKeyPrefix):
		out := &billing.Refusal{DenyCode: billing.DenyBudgetExceeded}
		if s.end != nil {
			retry := ceilSeconds(s.end.Sub(now))
			out.RetryAfterSeconds = &retry
		}
		return out
	}
	key := l.line.Limit
	out := &billing.Refusal{DenyCode: billing.DenyLimitReached, Limit: &key, ResetsAt: l.line.ResetsAt}
	if s.end != nil {
		out.ResetsAt = s.end
	}
	if out.ResetsAt != nil {
		retry := ceilSeconds(out.ResetsAt.Sub(now))
		out.RetryAfterSeconds = &retry
	}
	return out
}

func ceilSeconds(d time.Duration) int64 {
	if d <= 0 {
		return 0
	}
	s := int64(d / time.Second)
	if d%time.Second > 0 {
		s++
	}
	return s
}

// apply takes in a sync's answer for the view: its view, sent at sent by the
// gate's clock. A line keeps its window (and what the gate has in it) while
// the answer names the same one; a tile or session the gate already moved
// into becomes the answered window.
func (v *custView) apply(view billing.View, sent time.Time) {
	v.probing = false
	if v.used.IsZero() {
		v.used = sent
	}
	for _, nl := range view.Limits {
		l := v.lines[nl.Limit]
		if l == nil {
			l = &lineState{key: nl.Limit, money: nl.Unit == v.key.currency}
			v.lines[nl.Limit] = l
		}
		same := l.cur != nil && sameTime(l.cur.start, nl.WindowStartsAt)
		var cur *span
		switch {
		case same:
			cur = l.cur
		case l.next != nil && nl.WindowStartsAt != nil && (sameTime(l.next.start, nl.WindowStartsAt) || l.next.local):
			cur, l.next = l.next, nil
		default:
			cur = &span{}
		}
		cur.start, cur.end, cur.remaining, cur.local = nl.WindowStartsAt, nl.WindowEndsAt, nl.Remaining, false
		if nl.WindowStartsAt == nil && opensAtUse(nl.Reset) && l.next != nil && l.next.local {
			// A session the gate opened that no settlement has opened yet.
			l.next.remaining = nl.Remaining
		}
		l.cur, l.line, l.asOf = cur, nl, sent
		for _, s := range []*span{cur, l.next} {
			if s != nil {
				s.base, s.avail = s.inFlight(), l.room(s, sent, 0, nil)
			}
		}
	}
}

func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

// display is the customer's lines in this view, with what the gate has in
// flight added: spend windows are not shown.
func (v *custView) display() []billing.CustomerLimit {
	v.mu.Lock()
	defer v.mu.Unlock()
	now := time.Now()
	out := []billing.CustomerLimit{}
	for _, l := range v.lines {
		if strings.HasPrefix(l.line.Limit, limits.SpendKeyPrefix) || l.line.Limit == catalog.BalanceLimit {
			continue
		}
		remaining, held := l.line.Remaining, l.line.Held
		if s, ok := l.spanAt(now); ok {
			remaining = satAdd(s.remaining, -s.inFlight())
			held = satAdd(held, s.open)
		}
		resets := l.line.WindowEndsAt
		if resets == nil {
			resets = l.line.ResetsAt
		}
		out = append(out, billing.CustomerLimit{CustomerID: v.key.customer, Limit: l.line.Limit, Unit: l.line.Unit,
			Capacity: l.line.Capacity, Used: satAdd(satAdd(l.line.Capacity, -held), -remaining), Held: held,
			Remaining: remaining, WindowStartsAt: l.line.WindowStartsAt, ResetsAt: resets})
	}
	return out
}
