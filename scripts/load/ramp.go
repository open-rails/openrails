package main

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/open-rails/openrails/billing"
)

// Step is one rate of the hot merchant's ramp, measured over its last three
// quarters.
type Step struct {
	Rate           int                `json:"rate"`
	Customers      int                `json:"hot_customers"`
	Seconds        float64            `json:"seconds"`
	Offered        float64            `json:"offered"`
	Achieved       float64            `json:"achieved"`
	HotP50US       int64              `json:"hot_p50_us_median"`
	HotP99US       int64              `json:"hot_p99_us_median"`
	SyncP99US      int64              `json:"sync_p99_us_median"`
	SyncsPerSecond float64            `json:"syncs_per_second"`
	BacklogStart   int64              `json:"backlog_start"`
	BacklogEnd     int64              `json:"backlog_end"`
	Shed           int64              `json:"shed"`
	Lagged         int64              `json:"lagged"`
	Unavailable    int64              `json:"unavailable"`
	Errors         int64              `json:"errors"`
	MachineBusy    float64            `json:"machine_busy"`
	Cores          map[string]float64 `json:"cores"`
	Saturated      bool               `json:"saturated"`
	Reasons        []string           `json:"reasons"`
}

func (e *env) rampStep(ctx context.Context, f *fleet, w *WorldResult, schema string, hot billing.MerchantID, rate int) Step {
	plan := e.plan
	f.setPhase(fmt.Sprintf("ramp-%d", rate))
	f.rates(float64(rate))
	st := Step{Rate: rate, Customers: plan.active(plan.Hot, float64(rate)), Offered: f.offered(float64(rate))}
	secs := max(plan.StepSeconds, 8)
	sleepCtx(ctx, time.Duration(secs/4)*time.Second)
	backlog := func() int64 {
		stored, err := e.countEvents(ctx, schema, hot)
		if err != nil {
			return -1
		}
		return f.totals().Captured[plan.Hot.Slug] - stored
	}
	t0, b0, start := f.totals(), backlog(), time.Now()
	var p50s, p99s, s99s []int64
	for range secs - secs/4 {
		sleepCtx(ctx, time.Second)
		var p50 int64
		for _, c := range f.all() {
			p50 = max(p50, c.live().HotP50)
		}
		l := f.totals()
		p50s, p99s, s99s = append(p50s, p50), append(p99s, l.HotP99), append(s99s, l.SyncP99)
	}
	t1, b1 := f.totals(), backlog()
	el := time.Since(start).Seconds()
	st.Seconds = el
	st.Achieved = float64(t1.Decisions-t0.Decisions) / el
	st.SyncsPerSecond = float64(t1.Syncs-t0.Syncs) / el
	st.HotP50US, st.HotP99US, st.SyncP99US = median(p50s), median(p99s), median(s99s)
	st.BacklogStart, st.BacklogEnd = b0, b1
	st.Shed, st.Lagged = t1.Shed-t0.Shed, t1.Lagged-t0.Lagged
	st.Unavailable, st.Errors = t1.Unavailable-t0.Unavailable, t1.Errors-t0.Errors
	st.MachineBusy, st.Cores = w.cpuBetween(start, time.Now())
	decided := float64(t1.Decisions - t0.Decisions)
	if st.Achieved < 0.9*st.Offered {
		st.Reasons = append(st.Reasons, fmt.Sprintf("decided %.0f/s of %.0f/s offered", st.Achieved, st.Offered))
	}
	if b1 >= 0 && b0 >= 0 && b1-b0 > 2*int64(rate) && b1 > 3*int64(rate) {
		st.Reasons = append(st.Reasons, fmt.Sprintf("settlement backlog grew from %d to %d events", b0, b1))
	}
	if st.HotP99US > 100_000 {
		st.Reasons = append(st.Reasons, fmt.Sprintf("hot decision p99 %.1f ms", float64(st.HotP99US)/1000))
	}
	if float64(st.Shed+st.Lagged) > 0.005*st.Offered*el {
		st.Reasons = append(st.Reasons, fmt.Sprintf("driver could not keep up: %d shed, %d late", st.Shed, st.Lagged))
	}
	if decided > 0 && float64(st.Unavailable+st.Errors) > 0.01*decided {
		st.Reasons = append(st.Reasons, fmt.Sprintf("%d unavailable and %d errors", st.Unavailable, st.Errors))
	}
	st.Saturated = len(st.Reasons) > 0
	logf("[%s] step %d/s from %d customers: decided %.0f/s, hot p99 %.2f ms, sync p99 %.1f ms, backlog %d→%d, machine %.0f%% %v",
		w.Name, rate, st.Customers, st.Achieved, float64(st.HotP99US)/1000, float64(st.SyncP99US)/1000, b0, b1, 100*st.MachineBusy, st.Reasons)
	return st
}

func median(v []int64) int64 {
	if len(v) == 0 {
		return 0
	}
	s := slices.Clone(v)
	slices.Sort(s)
	return s[len(s)/2]
}
