package main

import (
	"container/heap"
	"fmt"
	"context"
	"math"
	"math/rand/v2"
	"slices"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Monthly odds per live plan. Churn lands near 5% a month: cancels at period
// end not resumed, failed renewals not recovered, merchant cancels,
// chargebacks and expired cards.
const (
	pCancelEnd  = 0.028
	pResume     = 0.20
	pFail       = 0.045
	pRecover    = 0.55
	pAwaiting   = 0.006
	pUnverified = 0.003
	pCard       = 0.006
	pCardFixed  = 0.70
	pMerchant   = 0.002
	pChargeback = 0.001
	pUpgrade    = 0.015
	pDowngrade  = 0.008
	pSchedDown  = 0.005
	pAddon      = 0.004
	pDelinquent = 0.002
	pReturn     = 0.25
	day         = 24 * time.Hour
)

const (
	qArrive    = `SELECT sim.arrive($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`
	qRejoin    = `SELECT sim.rejoin($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`
	qSet2      = `SELECT sim.set2($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`
	qSubscribe = `SELECT sim.subscribe($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`
	qCardRenew = `SELECT sim.card_renew($1, $2, $3, $4, $5, $6, $7, $8, $9)`
	qDelinq    = `SELECT sim.delinquency($1, $2, $3, $4, $5, $6)`
	qSchedule  = `SELECT sim.schedule_change($1, $2, $3, $4, $5, $6, $7)`
	qApply     = `SELECT sim.apply_change($1, $2, $3, $4, $5, $6, $7)`
	qEffect    = `SELECT sim.effect_due($1, $2, $3, $4)`
	qDrain     = `SELECT billing.effect_due_mrr_risk($1, 1000, NULL)`
)

type event struct {
	at  time.Time
	seq uint64
	fn  func(time.Time)
}

type events []*event

func (h events) Len() int { return len(h) }
func (h events) Less(i, j int) bool {
	if h[i].at.Equal(h[j].at) {
		return h[i].seq < h[j].seq
	}
	return h[i].at.Before(h[j].at)
}
func (h events) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *events) Push(x any)   { *h = append(*h, x.(*event)) }
func (h *events) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

type sub struct {
	id     uuid.UUID
	cust   *cust
	tier   int
	status string
	anchor time.Time
	k      int // the period is [anchor + k months, anchor + k+1 months)
	risk   bool
	doomed bool
	gen    int
	change *change
	done   bool
}

type change struct {
	id   uuid.UUID
	tier int
}

type cust struct {
	id      uuid.UUID
	cur     int
	pm      uuid.UUID
	expYear int
	core    *sub
	addon   *sub
	delinq  bool
}

func (s *sub) ps() time.Time { return s.anchor.AddDate(0, s.k, 0) }
func (s *sub) pe() time.Time { return s.anchor.AddDate(0, s.k+1, 0) }

type kindStats struct {
	N, Failed int
	total     time.Duration
	recent    []time.Duration
}

type sim struct {
	ctx           context.Context
	c             config
	w             *world
	conn          *pgx.Conn
	rng           *rand.Rand
	h             events
	seq           uint64
	live          int
	start, end    time.Time
	stats         map[string]*kindStats
	errs          []string
	nerr          int
	began         time.Time
	stopped       bool
	customers     int
	reactivations int
	executed      int
}

func newSim(c config, w *world, conn *pgx.Conn) *sim {
	return &sim{ctx: context.Background(), c: c, w: w, conn: conn, rng: rand.New(rand.NewPCG(uint64(c.seed), 1418)), stats: map[string]*kindStats{}}
}

func (s *sim) at(t time.Time, fn func(time.Time)) {
	s.seq++
	heap.Push(&s.h, &event{at: t, seq: s.seq, fn: fn})
}

func (s *sim) between(a, b time.Duration) time.Duration {
	if b <= a {
		return a
	}
	return a + time.Duration(s.rng.Int64N(int64(b-a)))
}

func (s *sim) poisson(lambda float64) int {
	if lambda <= 0 {
		return 0
	}
	if lambda > 50 {
		return max(0, int(math.Round(lambda+math.Sqrt(lambda)*s.rng.NormFloat64())))
	}
	l, k, p := math.Exp(-lambda), 0, 1.0
	for {
		p *= s.rng.Float64()
		if p <= l {
			return k
		}
		k++
	}
}

func (s *sim) exec(kind, sql string, args ...any) bool {
	t0 := time.Now()
	_, err := s.conn.Exec(s.ctx, sql, args...)
	d := time.Since(t0)
	st := s.stats[kind]
	if st == nil {
		st = &kindStats{}
		s.stats[kind] = st
	}
	st.N++
	st.total += d
	if len(st.recent) < 2000 {
		st.recent = append(st.recent, d)
	} else {
		st.recent[st.N%2000] = d
	}
	s.executed++
	if err != nil {
		st.Failed++
		s.nerr++
		if len(s.errs) < 40 {
			s.errs = append(s.errs, kind+": "+err.Error())
		}
		return false
	}
	return true
}

func (s *sim) price(cur, tier int) uuid.UUID { return s.w.prices[cur][tier] }

// addonOf is a customer's live add-on, its id and price, or NULLs.
func (s *sim) addonOf(c *cust) (*sub, any, any) {
	if c.addon == nil || c.addon.done {
		return nil, nil, nil
	}
	return c.addon, c.addon.id, s.price(c.cur, 3)
}

func (s *sim) run(ctx context.Context, from, to time.Time) (time.Time, error) {
	s.ctx, s.start, s.end, s.began = ctx, from, to, time.Now()
	s.at(from, s.tick)
	nextLog := from.AddDate(0, 1, 0)
	end := to
	for s.h.Len() > 0 {
		e := heap.Pop(&s.h).(*event)
		if e.at.After(to) {
			break
		}
		if ctx.Err() != nil {
			return e.at, ctx.Err()
		}
		if s.c.genBudget > 0 && time.Since(s.began) > s.c.genBudget {
			s.stopped, end = true, e.at
			logf("generation budget %s spent: history stops at %s", s.c.genBudget, end.Format(time.RFC3339))
			break
		}
		if e.at.After(nextLog) {
			logf("simulated %s: %d live customers, %d statements, %d failed, %.0f/s",
				e.at.Format("2006-01-02"), s.live, s.executed, s.nerr, float64(s.executed)/time.Since(s.began).Seconds())
			nextLog = nextLog.AddDate(0, 1, 0)
		}
		e.fn(e.at)
		if s.executed >= 500 && s.nerr*2 > s.executed {
			return e.at, fmt.Errorf("most statements fail (%d of %d): %v", s.nerr, s.executed, s.errs)
		}
	}
	if s.executed > 0 && s.nerr*10 > s.executed {
		return end, fmt.Errorf("%d of %d statements failed: %v", s.nerr, s.executed, s.errs)
	}
	return end, nil
}

// tick brings each day's new customers: the first month ramps the book to
// its size, then arrivals replace churn and close the gap to the target.
func (s *sim) tick(t time.Time) {
	n := float64(s.c.customers)
	var lambda float64
	if t.Before(s.start.AddDate(0, 0, 30)) {
		lambda = n / 30
	} else {
		lambda = max(0, 0.05*n/30.4+0.05*(n-float64(s.live)))
	}
	for range s.poisson(lambda) {
		s.at(t.Add(s.between(0, day)), s.arrive)
	}
	s.at(t.Add(day), s.tick)
}

func (s *sim) pickTier() int {
	u := s.rng.Float64()
	switch {
	case u < 0.6:
		return 0
	case u < 0.9:
		return 1
	}
	return 2
}

func (s *sim) arrive(t time.Time) {
	c := &cust{id: uuid.New(), pm: uuid.New(), expYear: s.end.Year() + 6}
	if s.rng.Float64() < 0.2 {
		c.cur = 1
	}
	core := &sub{id: uuid.New(), cust: c, tier: s.pickTier(), status: "active", anchor: t}
	var add *sub
	var addID, addPrice, addProduct any
	if s.rng.Float64() < 0.08 {
		add = &sub{id: uuid.New(), cust: c, tier: 3, status: "active", anchor: t}
		addID, addPrice, addProduct = add.id, s.price(c.cur, 3), s.w.addon
	}
	if !s.exec("arrive", qArrive, s.w.merchant, c.id, s.w.psp, c.pm, c.expYear, core.id, s.price(c.cur, core.tier), s.w.core,
		addID, addPrice, addProduct, core.ps(), core.pe(), t) {
		return
	}
	c.core, c.addon = core, add
	s.live++
	s.customers++
	s.drawPeriod(core)
	s.at(core.pe(), s.renewal(core))
}

func (s *sim) rejoin(c *cust) func(time.Time) {
	return func(t time.Time) {
		if c.core != nil && !c.core.done {
			return
		}
		core := &sub{id: uuid.New(), cust: c, tier: s.pickTier(), status: "active", anchor: t}
		var add *sub
		var addID, addPrice, addProduct any
		if s.rng.Float64() < 0.05 {
			add = &sub{id: uuid.New(), cust: c, tier: 3, status: "active", anchor: t}
			addID, addPrice, addProduct = add.id, s.price(c.cur, 3), s.w.addon
		}
		if !s.exec("reactivate", qRejoin, s.w.merchant, c.id, s.w.psp, c.pm, core.id, s.price(c.cur, core.tier), s.w.core,
			addID, addPrice, addProduct, core.ps(), core.pe(), t) {
			return
		}
		c.core, c.addon = core, add
		s.live++
		s.reactivations++
		s.drawPeriod(core)
		s.at(core.pe(), s.renewal(core))
	}
}

// renewal starts the next period: a scheduled change bills, the renewal may
// fail or find the card expiring, and the period's own events are drawn.
func (s *sim) renewal(sb *sub) func(time.Time) {
	return func(t time.Time) {
		if sb.done || sb.doomed || sb.status != "active" {
			return
		}
		sb.k++
		c := sb.cust
		if sb.change != nil {
			ch := sb.change
			sb.change = nil
			if !sb.risk && s.exec("apply_change", qApply, s.w.merchant, ch.id, sb.id, s.price(c.cur, ch.tier), sb.ps(), sb.pe(), t) {
				sb.tier = ch.tier
			}
		}
		if !sb.risk && !c.delinq {
			u := s.rng.Float64()
			switch {
			case u < pFail:
				s.at(t.Add(s.between(0, 2*time.Hour)), s.unhealthy(sb, "past_due"))
			case u < pFail+pAwaiting:
				s.at(t.Add(s.between(0, 2*time.Hour)), s.unhealthy(sb, "awaiting_method"))
			case u < pFail+pAwaiting+pUnverified:
				s.at(t.Add(s.between(0, 2*time.Hour)), s.unhealthy(sb, "unverified"))
			case u < pFail+pAwaiting+pUnverified+pCard:
				s.cardExpiring(sb, t)
			}
		}
		s.drawPeriod(sb)
		s.at(sb.pe(), s.renewal(sb))
	}
}

// drawPeriod draws the events of sb's current period.
func (s *sim) drawPeriod(sb *sub) {
	r, r2 := sb.ps(), sb.pe()
	span := r2.Sub(r)
	in := func(lo time.Duration) time.Time { return r.Add(s.between(lo, span-lo)) }
	if s.rng.Float64() < pCancelEnd {
		s.at(in(day), s.cancelEnd(sb))
	}
	if s.rng.Float64() < pMerchant {
		s.at(in(time.Hour), s.end1(sb, "merchant", "merchant_cancel"))
	}
	if s.rng.Float64() < pChargeback {
		s.at(in(time.Hour), s.end1(sb, "chargeback", "chargeback"))
	}
	if s.rng.Float64() < pUpgrade {
		s.at(in(time.Hour), s.reprice(sb, +1))
	}
	if s.rng.Float64() < pDowngrade {
		s.at(in(time.Hour), s.reprice(sb, -1))
	}
	if s.rng.Float64() < pSchedDown {
		s.at(in(day), s.schedDown(sb))
	}
	if s.rng.Float64() < pAddon {
		s.at(in(time.Hour), s.addAddon(sb))
	}
	if s.rng.Float64() < pDelinquent {
		s.at(in(day), s.delinquent(sb))
	}
}

func (s *sim) healthy(sb *sub) bool {
	return !sb.done && sb.status == "active" && !sb.risk && !sb.doomed && !sb.cust.delinq && sb.cust.core == sb
}

func (s *sim) churned(c *cust, t time.Time) {
	c.core.done = true
	if c.addon != nil {
		c.addon.done = true
	}
	s.live--
	if s.rng.Float64() < pReturn {
		s.at(t.Add(s.between(30*day, 365*day)), s.rejoin(c))
	}
}

// terminate cancels the plan and its add-on at once.
func (s *sim) terminate(sb *sub, t time.Time, cancelType, kind string) {
	c := sb.cust
	_, addID, addPrice := s.addonOf(c)
	if s.exec(kind, qSet2, s.w.merchant, sb.id, s.price(c.cur, sb.tier), addID, addPrice, t, "canceled", sb.ps(), sb.pe(), cancelType, t) {
		s.churned(c, t)
	}
}

func (s *sim) end1(sb *sub, cancelType, kind string) func(time.Time) {
	return func(t time.Time) {
		if s.healthy(sb) {
			s.terminate(sb, t, cancelType, kind)
		}
	}
}

func (s *sim) cancelEnd(sb *sub) func(time.Time) {
	return func(t time.Time) {
		if !s.healthy(sb) {
			return
		}
		c := sb.cust
		end := sb.pe()
		add, addID, addPrice := s.addonOf(c)
		if !s.exec("cancel_at_end", qSet2, s.w.merchant, sb.id, s.price(c.cur, sb.tier), addID, addPrice, t, "canceled", sb.ps(), end, "user", end) {
			return
		}
		sb.gen++
		gen := sb.gen
		sb.status = "canceled"
		if add != nil {
			add.status = "canceled"
		}
		if s.rng.Float64() < pResume {
			s.at(t.Add(s.between(0, end.Sub(t)*9/10)), s.resume(sb, gen))
		} else {
			s.at(end.Add(s.between(time.Minute, time.Hour)), s.effect(sb, gen))
		}
	}
}

func (s *sim) resume(sb *sub, gen int) func(time.Time) {
	return func(t time.Time) {
		if sb.done || sb.gen != gen || sb.status != "canceled" {
			return
		}
		c := sb.cust
		add, addID, addPrice := s.addonOf(c)
		if !s.exec("resume", qSet2, s.w.merchant, sb.id, s.price(c.cur, sb.tier), addID, addPrice, t, "active", sb.ps(), sb.pe(), nil, nil) {
			return
		}
		sb.status = "active"
		if add != nil {
			add.status = "active"
		}
	}
}

// effect is openrails.mrr_due reaching a canceled plan's end.
func (s *sim) effect(sb *sub, gen int) func(time.Time) {
	return func(t time.Time) {
		if sb.done || sb.gen != gen || sb.status != "canceled" {
			return
		}
		_, addID, _ := s.addonOf(sb.cust)
		if s.exec("mrr_due", qEffect, s.w.merchant, sb.id, addID, t) {
			s.churned(sb.cust, t)
		}
	}
}

// unhealthy moves a renewal into past_due, awaiting_method or unverified;
// it recovers or, but for unverified, expires.
func (s *sim) unhealthy(sb *sub, status string) func(time.Time) {
	return func(t time.Time) {
		if !s.healthy(sb) {
			return
		}
		c := sb.cust
		prev := sb.anchor.AddDate(0, sb.k-1, 0)
		if !s.exec(status, qSet2, s.w.merchant, sb.id, s.price(c.cur, sb.tier), nil, nil, t, status, prev, sb.ps(), nil, nil) {
			return
		}
		sb.status, sb.risk = status, true
		recover, within, expireAfter := pRecover, 10*day, 15*day
		switch status {
		case "awaiting_method":
			recover, within, expireAfter = 0.5, 7*day, 10*day
		case "unverified":
			recover, within = 1, 5*day
		}
		if s.rng.Float64() < recover {
			s.at(t.Add(s.between(day, within)), s.recover(sb, status))
		} else {
			s.at(sb.ps().Add(expireAfter), s.expire(sb, status))
		}
	}
}

func (s *sim) recover(sb *sub, status string) func(time.Time) {
	return func(t time.Time) {
		if sb.done || sb.status != status {
			return
		}
		c := sb.cust
		if s.exec("recover_"+status, qSet2, s.w.merchant, sb.id, s.price(c.cur, sb.tier), nil, nil, t, "active", sb.ps(), sb.pe(), nil, nil) {
			sb.status, sb.risk = "active", false
		}
	}
}

func (s *sim) expire(sb *sub, status string) func(time.Time) {
	return func(t time.Time) {
		if sb.done || sb.status != status {
			return
		}
		s.terminate(sb, t, "expired", "expire_"+status)
	}
}

// cardExpiring renews onto a card that expires within the new period: it
// is updated, or the next renewal fails and the plan expires.
func (s *sim) cardExpiring(sb *sub, t time.Time) {
	c := sb.cust
	if !s.exec("card_expiring", qCardRenew, s.w.merchant, c.pm, int(t.Month()), t.Year(), sb.id, s.price(c.cur, sb.tier), sb.ps(), sb.pe(), t) {
		return
	}
	sb.risk = true
	if s.rng.Float64() < pCardFixed {
		s.at(t.Add(s.between(2*day, 20*day)), func(u time.Time) {
			if sb.done || !sb.risk {
				return
			}
			if s.exec("card_updated", qCardRenew, s.w.merchant, c.pm, 12, c.expYear, nil, nil, nil, nil, u) && s.exec("risk_drain", qDrain, s.w.merchant) {
				sb.risk = false
			}
		})
		return
	}
	sb.doomed = true
	s.at(sb.pe().Add(s.between(0, 2*time.Hour)), func(u time.Time) {
		if !sb.done {
			s.terminate(sb, u, "expired", "expire_card")
		}
	})
}

func (s *sim) reprice(sb *sub, step int) func(time.Time) {
	return func(t time.Time) {
		to := sb.tier + step
		if !s.healthy(sb) || sb.change != nil || to < 0 || to > 2 {
			return
		}
		c := sb.cust
		kind := "upgrade"
		if step < 0 {
			kind = "downgrade"
		}
		if s.exec(kind, qSet2, s.w.merchant, sb.id, s.price(c.cur, to), nil, nil, t, "active", sb.ps(), sb.pe(), nil, nil) {
			sb.tier = to
		}
	}
}

func (s *sim) schedDown(sb *sub) func(time.Time) {
	return func(t time.Time) {
		if !s.healthy(sb) || sb.change != nil || sb.tier == 0 {
			return
		}
		c := sb.cust
		id := uuid.New()
		if s.exec("schedule_downgrade", qSchedule, s.w.merchant, id, sb.id, s.price(c.cur, sb.tier), s.price(c.cur, sb.tier-1), sb.pe(), t) {
			sb.change = &change{id: id, tier: sb.tier - 1}
		}
	}
}

func (s *sim) addAddon(sb *sub) func(time.Time) {
	return func(t time.Time) {
		c := sb.cust
		if !s.healthy(sb) || (c.addon != nil && !c.addon.done) {
			return
		}
		add := &sub{id: uuid.New(), cust: c, tier: 3, status: "active", anchor: t}
		if s.exec("add_addon", qSubscribe, s.w.merchant, c.id, add.id, s.price(c.cur, 3), s.w.addon, s.w.psp, c.pm, sb.ps(), sb.pe(), t) {
			c.addon = add
		}
	}
}

func (s *sim) delinquent(sb *sub) func(time.Time) {
	return func(t time.Time) {
		if !s.healthy(sb) {
			return
		}
		c := sb.cust
		cur := currencies[c.cur]
		if !s.exec("delinquent", qDelinq, s.w.merchant, c.id, cur, true, int64(20_000_000), t) || !s.exec("risk_drain", qDrain, s.w.merchant) {
			return
		}
		c.delinq, sb.risk = true, true
		s.at(t.Add(s.between(3*day, 15*day)), func(u time.Time) {
			if s.exec("delinquency_cleared", qDelinq, s.w.merchant, c.id, cur, false, int64(0), u) && s.exec("risk_drain", qDrain, s.w.merchant) {
				c.delinq = false
				if !sb.done {
					sb.risk = false
				}
			}
		})
	}
}

// simSummary is what the generator did.
type simSummary struct {
	From, To      time.Time
	StoppedEarly  bool
	Customers     int
	Live          int
	Reactivations int
	Statements    int
	Failed        int
	Errors        []string
	Seconds       float64
	PerSecond     float64
	Kinds         []kindRow
}

type kindRow struct {
	Kind          string
	N, Failed     int
	MeanMS, P50MS float64
	P95MS         float64
}

func (s *sim) summary(end time.Time) simSummary {
	el := time.Since(s.began).Seconds()
	out := simSummary{From: s.start, To: end, StoppedEarly: s.stopped, Customers: s.customers, Live: s.live, Reactivations: s.reactivations,
		Statements: s.executed, Failed: s.nerr, Errors: s.errs, Seconds: el, PerSecond: float64(s.executed) / el}
	for kind, st := range s.stats {
		r := slices.Clone(st.recent)
		sort.Slice(r, func(i, j int) bool { return r[i] < r[j] })
		row := kindRow{Kind: kind, N: st.N, Failed: st.Failed, MeanMS: ms(st.total) / float64(max(1, st.N))}
		if len(r) > 0 {
			row.P50MS, row.P95MS = ms(r[len(r)/2]), ms(r[len(r)*95/100])
		}
		out.Kinds = append(out.Kinds, row)
	}
	sort.Slice(out.Kinds, func(i, j int) bool { return out.Kinds[i].N > out.Kinds[j].N })
	return out
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
