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
	// marginIntervals is how many sync intervals of what others add to a
	// limit the near-zero margin covers: as long as an admission may stay
	// unseen by this gate.
	marginIntervals = 4
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

	mu     sync.Mutex
	lines  map[string]*lineState
	pinned []pinnedTake
	// probing is a stale view deciding its one request until a sync answers.
	probing bool
	used    time.Time
}

// lineState is one limit of a view: the line as last answered, its windows
// and what the gate has in flight in each.
type lineState struct {
	line  billing.ViewLimit
	money bool
	// asOf is when the sync that answered the line was sent, by the gate's
	// clock.
	asOf time.Time
	// cur is the line's window as answered (no bounds: one that never ends);
	// next the window after it, once the gate decided in it, or a session the
	// gate opened.
	cur, next *span
	// others are the most what others added to the limit in each of the last
	// marginIntervals intervals; seen is a previous answer of the same window.
	others []int64
	seen   bool
	prev   int64
}

// span is one window of a line and what the gate has in it: open tickets'
// takes, captured usage not yet settled, and server-decided requests the
// view does not show yet. base and avail bound offline spending: what the
// gate had in it when the view was answered, and what was left for every
// gate's new requests, of which each gate spends its share.
type span struct {
	start, end   *time.Time
	remaining    int64
	open, unsent int64
	pinned       int64
	base, avail  int64
	local        bool
}

func (s *span) inFlight() int64 { return s.open + s.unsent + s.pinned }

// take is what a decision took from one window of one limit.
type take struct {
	line *lineState
	span *span
	cost int64
}

// pinnedTake is a server-decided request's take, counted until a view from
// a sync built after it answers.
type pinnedTake struct {
	span  *span
	cost  int64
	build int64
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
		s.avail = max(s.remaining, 0)
		l.next = s
	case opensAtUse(l.line.Reset):
		remaining := l.line.Remaining
		if l.line.Next != nil {
			remaining = l.line.Next.Remaining
		}
		start := now.UTC().Truncate(time.Second)
		end := start.Add(time.Duration(l.line.Reset.WindowSeconds) * time.Second)
		l.next = &span{start: &start, end: &end, remaining: remaining, local: true, avail: max(remaining, 0)}
	default:
		return nil, false
	}
	return l.next, true
}

// room is what the window leaves for a request at now: the answered
// remaining, less what the gate has in flight in it, drained by running
// charges or refilled by a rate limit since the answer.
func (l *lineState) room(s *span, now time.Time, creditLine int64) int64 {
	r := s.remaining
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

// margin is what others may add to the limit before this gate sees it.
func (l *lineState) margin(cost int64) int64 {
	var most int64
	for _, o := range l.others {
		most = max(most, o)
	}
	if !l.seen && l.money && l.line.Recent > 0 {
		most = max(most, l.line.Recent*int64(limits.SyncInterval)/int64(time.Minute))
	}
	return max(cost, satMul(most, marginIntervals))
}

func satMul(a, n int64) int64 {
	if a > maxGateAmount/n {
		return maxGateAmount
	}
	return a * n
}

// verdict is what a view decides of a request.
type verdict uint8

const (
	verdictLocal verdict = iota
	verdictNear
	verdictRefuse
	verdictCold
	verdictWait
	verdictShort
)

// decision is a view's verdict on one request and, admitted, its takes.
type decision struct {
	verdict verdict
	refusal *billing.Refusal
	takes   []take
}

// decide takes a request's costs from every limit its set claims, or from
// none. Each limit is refused if the request does not fit it at all, near
// if less than the margin would remain, and local otherwise; the first
// refusal in the set's order wins and any near sends the request to the
// server. Offline, a limit fits only within the gate's share of it. A stale
// view decides one request (probe) and holds the rest for a sync.
func (v *custView) decide(set billing.LimitSet, costs []int64, now time.Time, offline bool, gates int) decision {
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
		takes[i] = take{line: l, span: s, cost: c}
		r := l.room(s, now, set.CreditLine)
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
			spent := satAdd(s.inFlight(), -s.base)
			avail := s.avail
			if l.line.Limit == catalog.BalanceLimit {
				avail = satAdd(avail, set.CreditLine)
			}
			share := avail / int64(max(gates, 1))
			if awp && spent >= share || !awp && satAdd(spent, c) > share {
				out.verdict = verdictShort
			}
			continue
		}
		if out.verdict == verdictLocal && satAdd(r, -c) < l.margin(c) {
			out.verdict = verdictNear
		}
	}
	switch out.verdict {
	case verdictNear:
		// Pinned at once, so the gate's other requests see it while the
		// server decides.
		out.takes = takes
		for _, t := range takes {
			t.span.pinned = satAdd(t.span.pinned, t.cost)
		}
		return out
	case verdictLocal:
	default:
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

// unpin gives back a server-decided request's takes the server refused.
func (v *custView) unpin(takes []take) {
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, t := range takes {
		t.span.pinned = satAdd(t.span.pinned, -t.cost)
	}
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

// pin keeps a server-admitted request's takes, already pinned, until a view
// from a sync built after it answers.
func (v *custView) pin(takes []take, build int64) {
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, t := range takes {
		v.pinned = append(v.pinned, pinnedTake{span: t.span, cost: t.cost, build: build})
	}
}

// apply takes in a sync's answer for the view: its view, sent at sent by the
// gate's clock as sync number build, among gates live leases. A line keeps
// its window (and what the gate has in it) while the answer names the same
// one; a tile or session the gate already moved into becomes the answered
// window. What others added since the last answer of the same window feeds
// the margin.
func (v *custView) apply(view billing.View, sent time.Time, build int64, gates int) {
	kept := v.pinned[:0]
	for _, p := range v.pinned {
		if p.build < build {
			p.span.pinned = satAdd(p.span.pinned, -p.cost)
			continue
		}
		kept = append(kept, p)
	}
	v.pinned = kept
	v.probing = false
	for _, nl := range view.Limits {
		l := v.lines[nl.Limit]
		if l == nil {
			l = &lineState{money: nl.Unit == v.key.currency}
			v.lines[nl.Limit] = l
		}
		same := l.cur != nil && sameTime(l.cur.start, nl.WindowStartsAt)
		if l.seen && same {
			delta := l.prev - nl.Remaining - nl.Settled
			if nl.Limit == catalog.BalanceLimit {
				delta -= drained(nl.DrainPerHour, sent.Sub(l.asOf))
			}
			l.others = append(l.others, max(delta, 0))
			if len(l.others) > marginIntervals {
				l.others = l.others[1:]
			}
		}
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
		l.cur, l.line, l.asOf, l.prev, l.seen = cur, nl, sent, nl.Remaining, true
		for _, s := range []*span{cur, l.next} {
			if s != nil {
				s.base, s.avail = s.inFlight(), max(l.room(s, sent, 0), 0)
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
			held = satAdd(held, s.open+s.pinned)
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
