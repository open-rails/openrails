package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
)

const crashSlug = "crash"

// runHTTP is the hosted shape: the standalone server, gate processes over
// HTTP, every merchant at its rate, the hot one ramped, then the faults.
func (e *env) runHTTP(ctx context.Context) *WorldResult {
	plan := e.plan
	w := &WorldResult{Name: "http", Mode: "remote"}
	const schema = "load_http"
	if err := e.resetWorld(ctx, schema); err != nil {
		w.fail("reset: %v", err)
		return w
	}
	addr, err := freeAddr()
	if err != nil {
		w.fail("port: %v", err)
		return w
	}
	slugs := []string{crashSlug}
	for _, m := range plan.merchants() {
		slugs = append(slugs, m.Slug)
	}
	readyPath := filepath.Join(e.out, "http-ready.json")
	spec := serveSpec{DSN: plan.DSN, Schema: schema, RedisURL: plan.RedisURL, ClickHouse: plan.ClickHouse, CHDatabase: schema, Addr: addr, PoolConns: 30, Merchants: slugs, Ready: readyPath}
	specPath := filepath.Join(e.out, "http-server.json")
	if err := writeJSON(specPath, spec); err != nil {
		w.fail("server spec: %v", err)
		return w
	}
	srv, err := e.start("http-server", "serve", specPath)
	if err != nil {
		w.fail("server: %v", err)
		return w
	}
	defer srv.terminate(40 * time.Second)
	var ready serveReady
	deadline := time.Now().Add(150 * time.Second)
	for readJSON(readyPath, &ready) != nil {
		if !srv.running() || time.Now().After(deadline) || ctx.Err() != nil {
			w.fail("server did not become ready (see http-server.log): %v", srv.err)
			return w
		}
		time.Sleep(200 * time.Millisecond)
	}
	creds := map[billing.MerchantID]openrails.RemoteCredential{}
	for _, m := range ready.Merchants {
		creds[m.ID] = openrails.RemoteCredential{APIKey: m.Key}
	}
	remote, err := openrails.NewRemote(ready.URL, openrails.RemoteOptions{Credentials: creds, Timeout: 2 * time.Minute})
	if err != nil {
		w.fail("remote: %v", err)
		return w
	}
	defer remote.Close(context.Background())
	admin := map[string]*openrails.Client{}
	for slug, m := range ready.Merchants {
		admin[slug], _ = remote.Merchant(m.ID)
	}

	logf("[http] seeding")
	seeds := map[string]*seeded{}
	sizes := map[string][2]int{crashSlug: {plan.CrashCustomers, 0}}
	for _, m := range plan.merchants() {
		sizes[m.Slug] = [2]int{m.Customers, plan.DrainCustomers}
	}
	for slug, size := range sizes {
		s := &seeded{slug: slug, class: classOf(plan, slug), id: ready.Merchants[slug].ID, key: ready.Merchants[slug].Key}
		if err := seed(ctx, admin[slug], plan, s, size[0], size[1]); err != nil {
			w.fail("seed %s: %v", slug, err)
			return w
		}
		seeds[slug] = s
	}
	// Lots that lapse mid-run, granted before any usage, so the lapse posts
	// all of the customer's usage before it.
	expiring := e.grantExpiring(ctx, w, admin, seeds, 75*time.Second)

	f := &fleet{e: e, world: w, ms: plan.merchants()}
	defer f.killRunning()
	var specs []gateSpec
	for p := range plan.Processes {
		gs := e.gateSpec(p, "remote", fmt.Sprintf("h%d", p), schema, seeds, plan.merchants())
		gs.URL = ready.URL
		gs.Results = filepath.Join(e.out, fmt.Sprintf("http-gates-%d.result.json", p))
		if err := f.add(gs, fmt.Sprintf("http-gates-%d", p)); err != nil {
			w.fail("gate process %d: %v", p, err)
			return w
		}
		specs = append(specs, gs)
	}
	stopSampling := e.sample(ctx, w, schema, f.currentPhase, func() map[string]int {
		pids := f.pids()
		if srv.running() {
			pids["server"] = srv.pid()
		}
		return pids
	})

	f.rates(plan.Hot.Rate)
	f.setPhase("warm")
	sleepCtx(ctx, time.Duration(plan.WarmSeconds)*time.Second)

	f.setPhase("steady")
	steadyStart := time.Now()
	f.send(control{Op: "drain", Rate: plan.DrainRate / float64(plan.Processes)})
	topUps := make(chan *Hist, 1)
	go func() { topUps <- e.topUps(ctx, w, admin, seeds, steadyStart.Add(20*time.Second)) }()
	passDone := make(chan *PassResult, 1)
	go func() {
		at := time.Now()
		if len(expiring) > 0 {
			at = expiring[0].ExpiresAt.Add(15 * time.Second)
		}
		sleepCtx(ctx, time.Until(at))
		passDone <- e.billingPass(ctx, schema)
	}()
	sleepCtx(ctx, time.Duration(plan.SteadySeconds)*time.Second)
	f.send(control{Op: "drain", Rate: 0})
	w.TopUps = <-topUps

	hotID := seeds[plan.Hot.Slug].id
	for _, rate := range plan.Ramp {
		if ctx.Err() != nil {
			break
		}
		st := e.rampStep(ctx, f, w, schema, hotID, rate)
		w.Steps = append(w.Steps, st)
		if st.Saturated {
			w.Saturation = fmt.Sprintf("saturated at %d events/s: %v", rate, st.Reasons)
			break
		}
	}
	if w.Saturation == "" && len(plan.Ramp) > 0 {
		w.Saturation = fmt.Sprintf("not saturated up to %d events/s", plan.Ramp[len(plan.Ramp)-1])
	}
	f.rates(plan.Hot.Rate)

	f.setPhase("lost-replies")
	f.send(control{Op: "drop", Rate: plan.LostReplyShare})
	sleepCtx(ctx, time.Duration(plan.FaultSeconds)*time.Second)
	f.send(control{Op: "drop", Rate: 0})

	f.setPhase("crash")
	crashSpecs := e.crash(ctx, f, w, seeds[crashSlug], ready.URL, schema)

	f.setPhase("ch-outage")
	var mains []billing.MerchantID
	for _, m := range plan.merchants() {
		mains = append(mains, seeds[m.Slug].id)
	}
	w.Outage = e.outage(ctx, f, schema, mains)
	f.setPhase("recovery")
	sleepCtx(ctx, time.Duration(plan.RecoverySeconds)*time.Second)
	f.endPhase()

	logf("[http] stopping gate processes")
	f.stopAll()
	stopSampling()
	w.BillingPass = <-passDone
	w.collect(append(specs, crashSpecs...))
	w.Crash = e.checkCrash(ctx, w, schema, seeds[crashSlug].id, crashSpecs)
	e.checkWorld(ctx, w, schema, seeds, admin)
	e.checkExpiring(ctx, w, schema, admin, seeds, expiring)
	return w
}

func classOf(plan Plan, slug string) string {
	switch slug {
	case plan.Hot.Slug:
		return "hot"
	case crashSlug:
		return "crash"
	}
	return "normal"
}

// gateSpec is one main gate process serving every merchant of ms.
func (e *env) gateSpec(proc int, mode, prefix, schema string, seeds map[string]*seeded, ms []MerchantPlan) gateSpec {
	plan := e.plan
	gs := gateSpec{Proc: proc, Mode: mode, Prefix: prefix, DSN: plan.DSN, Schema: schema, RedisURL: plan.RedisURL, ClickHouse: plan.ClickHouse, CHDatabase: schema,
		Units: plan.Units, UnitPrice: plan.UnitPrice, WorkMeanMS: plan.WorkMeanMS, LongShare: plan.LongShare, ReleaseShare: plan.ReleaseShare}
	for _, m := range ms {
		s := seeds[m.Slug]
		gs.Merchants = append(gs.Merchants, gateMerchant{Slug: s.slug, ID: s.id, Key: s.key, Class: s.class, Customers: s.customers, Drains: s.drains})
	}
	return gs
}

// grantExpiring gives the first Expiring customers of the first normal
// merchant a $5 lot that lapses after lead.
func (e *env) grantExpiring(ctx context.Context, w *WorldResult, admin map[string]*openrails.Client, seeds map[string]*seeded, lead time.Duration) []billing.CreditGrant {
	if e.plan.Expiring == 0 || len(e.plan.Normal) == 0 {
		return nil
	}
	s := seeds[e.plan.Normal[0].Slug]
	at := time.Now().Add(lead).UTC().Truncate(time.Second)
	var grants []billing.CreateCreditGrantParams
	for _, c := range s.customers[:min(e.plan.Expiring, len(s.customers))] {
		grants = append(grants, billing.CreateCreditGrantParams{CustomerID: c, Currency: "USD", Amount: 5_000_000, Source: "load", SourceID: "expiring-" + c.String(), ExpiresAt: &at})
	}
	out, err := admin[s.slug].CreateCreditGrants(ctx, grants)
	if err != nil {
		w.fail("expiring lots: %v", err)
		return nil
	}
	return out
}

// topUps grants $1 to TopUps customers of the second normal merchant, one at
// a time from at: each posts the customer's usage first.
func (e *env) topUps(ctx context.Context, w *WorldResult, admin map[string]*openrails.Client, seeds map[string]*seeded, at time.Time) *Hist {
	h := newHist()
	if e.plan.TopUps == 0 || len(e.plan.Normal) < 2 {
		return h
	}
	sleepCtx(ctx, time.Until(at))
	s := seeds[e.plan.Normal[1].Slug]
	for i, c := range s.customers[:min(e.plan.TopUps, len(s.customers))] {
		start := time.Now()
		_, err := admin[s.slug].CreateCreditGrants(ctx, []billing.CreateCreditGrantParams{{CustomerID: c, Currency: "USD", Amount: 1_000_000, Source: "load", SourceID: fmt.Sprintf("top-up-%d", i)}})
		if err != nil {
			w.fail("top-up %s: %v", c, err)
			continue
		}
		h.Add(time.Since(start))
	}
	return h
}

type registeredJob struct {
	KindName string `json:"-"`
}

func (j registeredJob) Kind() string { return j.KindName }

// billingPass queues the server's hourly billing pass now (the month-end
// close and lot expiry run in it) and waits for it.
func (e *env) billingPass(ctx context.Context, schema string) *PassResult {
	out := &PassResult{EnqueuedAt: time.Now().UTC()}
	client, err := river.NewClient(riverpgxv5.New(e.pool), &river.Config{Schema: schema})
	if err != nil {
		out.Error = err.Error()
		return out
	}
	job, err := client.Insert(ctx, registeredJob{KindName: "openrails.billing_pass"}, &river.InsertOpts{Queue: openrails.QueueBilling, MaxAttempts: 1})
	if err != nil {
		out.Error = err.Error()
		return out
	}
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		row, err := client.JobGet(ctx, job.Job.ID)
		if err == nil && row.FinalizedAt != nil {
			out.State = string(row.State)
			out.Seconds = row.FinalizedAt.Sub(job.Job.CreatedAt).Seconds()
			if row.State != rivertype.JobStateCompleted && len(row.Errors) > 0 {
				out.Error = row.Errors[len(row.Errors)-1].Error
			}
			return out
		}
		time.Sleep(time.Second)
	}
	out.State = "unfinished"
	return out
}

// crash runs two processes of the crash merchant, one with a file outbox
// and one without, kills both mid-traffic and restarts them.
func (e *env) crash(ctx context.Context, f *fleet, w *WorldResult, s *seeded, url, schema string) []gateSpec {
	plan := e.plan
	var specs []gateSpec
	mk := func(n int, outbox bool) gateSpec {
		kind := "plain"
		if outbox {
			kind = "outbox"
		}
		gs := e.gateSpec(100+n, "remote", fmt.Sprintf("c%d", n), schema, map[string]*seeded{crashSlug: s}, []MerchantPlan{{Slug: crashSlug}})
		gs.URL = url
		gs.CaptureLog = filepath.Join(e.out, "crash-"+kind+".captures")
		if outbox {
			gs.Outbox = filepath.Join(e.out, "crash-outbox.jsonl")
		}
		gs.Results = filepath.Join(e.out, fmt.Sprintf("crash-%d.result.json", n))
		return gs
	}
	var first []*child
	for i, outbox := range []bool{true, false} {
		gs := mk(i, outbox)
		if err := f.add(gs, fmt.Sprintf("crash-%d", i)); err != nil {
			w.fail("crash process: %v", err)
			return specs
		}
		first = append(first, f.all()[len(f.all())-1])
	}
	for _, c := range first {
		c.send(control{Op: "rates", Rates: map[string]float64{crashSlug: plan.CrashRate}})
	}
	sleepCtx(ctx, 20*time.Second)
	for _, c := range first {
		c.kill()
	}
	logf("[http] killed both crash processes")
	sleepCtx(ctx, 3*time.Second)
	for i, outbox := range []bool{true, false} {
		gs := mk(2+i, outbox)
		if err := f.add(gs, fmt.Sprintf("crash-%d", 2+i)); err != nil {
			w.fail("crash restart: %v", err)
			continue
		}
		f.all()[len(f.all())-1].send(control{Op: "rates", Rates: map[string]float64{crashSlug: plan.CrashRate}})
		specs = append(specs, gs)
	}
	sleepCtx(ctx, 25*time.Second)
	for _, c := range f.all() {
		if c.running() && (c.name == "crash-2" || c.name == "crash-3") {
			if err := c.stop(2 * time.Minute); err != nil {
				w.fail("%s: %v", c.name, err)
			}
		}
	}
	return specs
}

// OutageResult is the ClickHouse outage: how decisions went while it was
// down and how long the backlog took to settle once it was back.
type OutageResult struct {
	Seconds        float64 `json:"seconds"`
	RestartSeconds float64 `json:"restart_seconds"`
	DrainSeconds   float64 `json:"drain_seconds"`
	BacklogPeak    int64   `json:"backlog_peak"`
	Error          string  `json:"error,omitempty"`
}

func (e *env) outage(ctx context.Context, f *fleet, schema string, mains []billing.MerchantID) *OutageResult {
	out := &OutageResult{}
	name := e.plan.ClickHouseContainer
	if name == "" {
		out.Error = "no clickhouse_container: outage skipped"
		return out
	}
	start := time.Now()
	if err := docker(ctx, "kill", name); err != nil {
		out.Error = err.Error()
		return out
	}
	logf("[http] ClickHouse killed")
	sleepCtx(ctx, time.Duration(e.plan.OutageSeconds)*time.Second)
	if err := docker(ctx, "start", name); err != nil {
		out.Error = err.Error()
		return out
	}
	restart := time.Now()
	for time.Since(restart) < 2*time.Minute {
		pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := e.ch.Ping(pctx)
		cancel()
		if err == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	out.Seconds = restart.Sub(start).Seconds()
	out.RestartSeconds = time.Since(restart).Seconds()
	logf("[http] ClickHouse back after %.1fs", out.RestartSeconds)
	// Settled once the main merchants' stored events reach what the gates
	// captured before the restart; the crash merchant lost usage by design.
	target := int64(0)
	for slug, v := range f.totals().Captured {
		if slug != crashSlug {
			target += v
		}
	}
	for time.Since(restart) < 2*time.Minute && ctx.Err() == nil {
		stored, err := e.countEvents(ctx, schema, mains...)
		if err == nil {
			out.BacklogPeak = max(out.BacklogPeak, target-stored)
			if stored >= target {
				out.DrainSeconds = time.Since(restart).Seconds()
				return out
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	out.Error = "backlog not drained within 2 minutes"
	return out
}

// countEvents counts a database's raw usage events, of merchants when named.
func (e *env) countEvents(ctx context.Context, schema string, merchants ...billing.MerchantID) (int64, error) {
	q := "SELECT count() FROM `" + schema + "`.usage_events"
	if len(merchants) > 0 {
		ids := make([]string, len(merchants))
		for i, m := range merchants {
			ids[i] = uuidLit(m.UUID())
		}
		q += " WHERE merchant_id IN (" + strings.Join(ids, ",") + ")"
	}
	var n uint64
	err := e.ch.QueryRow(ctx, q).Scan(&n)
	return int64(n), err
}

func hostInfo() map[string]string {
	out := map[string]string{}
	if data, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		n := 0
		for _, line := range splitLines(string(data)) {
			if len(line) > 10 && line[:10] == "model name" {
				n++
				out["cpu"] = line[13:]
			}
		}
		out["cpus"] = fmt.Sprint(n)
	}
	if data, err := os.ReadFile("/proc/meminfo"); err == nil {
		out["memory"] = splitLines(string(data))[0]
	}
	out["runner"] = os.Getenv("RUNNER_NAME")
	out["runner_os"] = os.Getenv("ImageOS")
	return out
}
