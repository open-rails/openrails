package main

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
)

// summarize renders summary.md from the raw results.
func summarize(r *Results) string {
	var b strings.Builder
	p := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	p("# OpenRails load qualification")
	p("")
	p("Run %s to %s on %s (%s CPUs, %s). A GitHub-hosted runner is shared hardware: treat latency and saturation as noisy first numbers, not a baseline.",
		r.Started.Format("2006-01-02 15:04Z"), r.Finished.Format("15:04Z"), r.Host["cpu"], r.Host["cpus"], strings.TrimSpace(strings.TrimPrefix(r.Host["memory"], "MemTotal:")))
	p("")
	var settings []string
	for _, k := range sortedKeys(r.Postgres) {
		settings = append(settings, k+"="+r.Postgres[k])
	}
	p("PostgreSQL: %s.", strings.Join(settings, ", "))
	p("")
	plan := r.Plan
	p("Workload: %d gate processes per world, each with one gate per merchant and every customer spread over all of them. Hot merchant at %.0f events/s from %d customers (%.1f requests/s each), ramped through %v with its active customers growing at that rate up to %d; %d other merchants at %.0f events/s each from all their customers; requests of %d to %d units at %d micros a unit; tickets open %.0f ms on average, %.0f%% for 2 to 6 s; %d draining customers a merchant funded %d micros at %.0f requests/s.",
		plan.Processes, plan.Hot.Rate, plan.active(plan.Hot, plan.Hot.Rate), plan.CustomerRate, plan.Ramp, plan.Hot.Customers, len(plan.Normal), firstRate(plan), plan.Units[0], plan.Units[1], plan.UnitPrice, plan.WorkMeanMS, 100*plan.LongShare, plan.DrainCustomers, plan.DrainFunding, plan.DrainRate)
	p("")
	if len(r.Errors) > 0 {
		p("Run errors: %s", strings.Join(r.Errors, "; "))
		p("")
	}

	p("## Checks")
	p("")
	p("| world | check | ok | detail |")
	p("|---|---|---|---|")
	for _, name := range []string{"http", "embedded"} {
		if w := r.Worlds[name]; w != nil {
			for _, c := range w.Checks {
				p("| %s | %s | %s | %s |", name, c.Name, okMark(c.OK), c.Detail)
			}
		}
	}
	if r.Posting != nil {
		for _, c := range r.Posting.Checks {
			p("| posting | %s | %s | %s |", c.Name, okMark(c.OK), c.Detail)
		}
	}
	if r.Storage != nil {
		for _, w := range r.Storage.Worlds {
			for _, c := range w.Checks {
				p("| storage/%s | %s | %s | %s |", w.Name, c.Name, okMark(c.OK), c.Detail)
			}
		}
	}
	p("")

	for _, name := range []string{"http", "embedded"} {
		w := r.Worlds[name]
		if w == nil {
			continue
		}
		p("## %s world (%s)", name, w.Mode)
		p("")
		if len(w.Errors) > 0 {
			p("Errors: %s", strings.Join(w.Errors, "; "))
			p("")
		}
		if len(w.ProcErrors) > 0 {
			p("Gate process errors (first %d): %s", min(5, len(w.ProcErrors)), strings.Join(w.ProcErrors[:min(5, len(w.ProcErrors))], "; "))
			p("")
		}
		p("### Decisions in the steady phase, by path and outcome")
		p("")
		decisionTable(p, w, "steady")
		p("")
		p("Miss rate (decisions decided on the server, cold or held for a sync), steady phase: %s", missRates(w, "steady"))
		p("")
		p("### Syncs (client-observed, including ClickHouse's async-insert wait)")
		p("")
		p("| phase | merchant | syncs | p50 ms | p99 ms | max ms | failed | lost replies |")
		p("|---|---|---|---|---|---|---|---|")
		for _, k := range sortedKeys(w.Syncs) {
			h := w.Syncs[k]
			phase, class, _ := strings.Cut(k, "|")
			p("| %s | %s | %d | %s | %s | %s | %d | %d |", phase, class, h.N, ms(h.Q(0.5)), ms(h.Q(0.99)), ms(h.Max), w.Counters["sync_failures|"+k], w.Counters["lost_replies|"+k])
		}
		p("")
		queryTable(p, w.Queries)
		if len(w.Steps) > 0 {
			p("### Ramp of the hot merchant")
			p("")
			p("| hot events/s | hot customers | offered | decided/s | warm p50 ms | warm p99 ms | misses | miss p50 ms | miss p99 ms | sync p99 ms | syncs/s | backlog start → end | machine CPU | largest consumers (cores) | saturated |")
			p("|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|")
			for _, s := range w.Steps {
				p("| %d | %d | %.0f | %.0f | %s | %s | %.1f%% | %s | %s | %s | %.0f | %d → %d | %.0f%% | %s | %s |", s.Rate, s.Customers, s.Offered, s.Achieved, ms(s.HotP50US), ms(s.HotP99US), 100*s.MissRate, ms(s.MissP50US), ms(s.MissP99US), ms(s.SyncP99US), s.SyncsPerSecond,
					s.BacklogStart, s.BacklogEnd, 100*s.MachineBusy, topCores(s.Cores, 4), strings.Join(s.Reasons, "; "))
			}
			p("")
			p("Saturation: %s. Back at the base rates, the settlement backlog settled in %.0f s (-1: not within four minutes).", w.Saturation, w.CooldownSeconds)
			p("")
		}
		if o := w.Overshoot; o != nil {
			p("### Overshoot")
			p("")
			p("%d draining customers, %d drained. Overshoot max %s, median %s (drained); bound max %s; max overshoot/bound %.2f; %d over the bound. %.1f%% of their admissions were decided on the server.",
				o.Customers, o.Drained, usd(o.Max), usd(o.Median), usd(o.MaxBound), o.MaxRatio, o.Violations, 100*o.ServerShare)
			p("")
		}
		if w.TopUps != nil && w.TopUps.N > 0 {
			p("Top-ups under load (each posts the customer's usage first): %d, p50 %s ms, max %s ms.", w.TopUps.N, ms(w.TopUps.Q(0.5)), ms(w.TopUps.Max))
			p("")
		}
		if w.BillingPass != nil {
			p("Billing pass after the lots lapsed (attempt %d): %s in %.1f s %s.", w.BillingPass.Attempts, w.BillingPass.State, w.BillingPass.Seconds, w.BillingPass.Error)
			p("")
		}
		if w.Outage != nil || len(w.Crash) > 0 || w.Counters != nil && hasPhase(w, "lost-replies") {
			p("### Faults")
			p("")
			lost := int64(0)
			for k, v := range w.Counters {
				if strings.HasPrefix(k, "lost_replies|") {
					lost += v
				}
			}
			if hasPhase(w, "lost-replies") {
				p("- Lost replies: %d sync replies dropped after the server answered; the stored-once checks above cover their resends.", lost)
			}
			for _, c := range w.Crash {
				p("- Crash (%s): %d captured, %d stored once, %d lost, %d stored twice (%s).", c.Kind, c.Captured, c.Stored, c.Lost, c.Twice, c.Expect)
			}
			if o := w.Outage; o != nil {
				p("- ClickHouse outage: down %.0f s, answering again %.1f s after restart, backlog of %d events settled %.1f s after restart. %s", o.Seconds, o.RestartSeconds, o.BacklogPeak, o.DrainSeconds, o.Error)
				p("")
				p("Decisions during the outage:")
				p("")
				decisionTable(p, w, "ch-outage")
			}
			p("")
		}
		var waits int64
		blockers, waiters := map[string]bool{}, map[string]bool{}
		for _, s := range w.Samples {
			waits = max(waits, s.PGLockWaits)
			for _, b := range s.PGBlockers {
				if len(blockers) < 5 {
					blockers[b] = true
				}
			}
			for _, b := range s.PGWaiters {
				if len(waiters) < 5 {
					waiters[b] = true
				}
			}
		}
		p("PostgreSQL: at most %d sessions waiting on a lock in a sample; blockers seen: %s; waiting for: %s", waits, strings.Join(sortedKeys(blockers), " | "), strings.Join(sortedKeys(waiters), " | "))
		p("")
		rowsAndDisk(p, w.Rows, w.Storage)
	}
	if pr := r.Posting; pr != nil {
		p("## Posting")
		p("")
		if len(pr.Errors) > 0 {
			p("Errors: %s", strings.Join(pr.Errors, "; "))
			p("")
		}
		p("- Ingest as a batch reporter (one lease): events in time order across %d customers %.0f events/s; grouped by customer %.0f events/s; sync p50 %s ms, p99 %s ms.", pr.Customers, pr.IngestRate, pr.GroupedRate, ms(pr.Syncs.Q(0.5)), ms(pr.Syncs.Q(0.99)))
		p("- Heavy customer: %d events in its month (copied in ClickHouse in %.1f s).", pr.HeavyRows, pr.CloneSeconds)
		p("- Posting before a top-up: light customers p50 %s ms, max %s ms; heavy customer %.0f ms.", ms(pr.TopUpLight.Q(0.5)), ms(pr.TopUpLight.Max), pr.TopUpHeavyMS)
		if pr.DailyPosting != nil {
			p("- Daily posting at the month's end of %d customers including the heavy one: %s in %.1f s, %d postings. %s", pr.Customers+2, pr.DailyPosting.State, pr.DailyPosting.Seconds, pr.DailyPostings, pr.DailyPosting.Error)
		}
		p("")
		queryTable(p, pr.Queries)
		rowsAndDisk(p, pr.Rows, pr.Storage)
	}
	if st := r.Storage; st != nil {
		storageSection(p, st)
	}
	return b.String()
}

// storageSection is what each storage database holds per usage event.
func storageSection(p func(string, ...any), st *StorageResult) {
	p("## Storage per usage event")
	p("")
	p("Workload: %s. Each database is measured once every TTL is applied and its parts are merged (`MATERIALIZE TTL`, `OPTIMIZE FINAL`).", st.Shape)
	p("")
	for _, w := range st.Worlds {
		p("### %s: usage from %s to %s", w.Name, w.From.Format(time.DateOnly), w.To.Format(time.DateOnly))
		p("")
		if len(w.Errors) > 0 {
			p("Errors: %s", strings.Join(w.Errors, "; "))
			p("")
		}
		p("%d events stored, ingested at %.0f events/s (sync p50 %s ms, p99 %s ms).", w.Events, w.IngestRate, ms(w.Syncs.Q(0.5)), ms(w.Syncs.Q(0.99)))
		p("")
		perEvent(p, w.Tables, uint64(max(w.Events, 1)))
		if h := w.History; h != nil {
			p("usage_history: %d rows; one a UTC day would be %d rows.", h.Rows, h.DayRows)
			p("")
		}
		if len(w.Columns) > 0 {
			p("| usage_events column | compressed | uncompressed | compressed B/event |")
			p("|---|---|---|---|")
			for _, c := range w.Columns {
				p("| %s | %s | %s | %.2f |", c.Column, mb(c.Compressed), mb(c.Uncompressed), float64(c.Compressed)/float64(max(w.Events, 1)))
			}
			p("")
		}
	}
}

// perEvent is each table's active parts and their bytes per stored event.
func perEvent(p func(string, ...any), storage []TableStorage, events uint64) {
	p("| ClickHouse table | rows | parts | compressed | uncompressed | on disk | B/event compressed | B/event uncompressed | B/event on disk |")
	p("|---|---|---|---|---|---|---|---|---|")
	sorted := slices.Clone(storage)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Bytes > sorted[j].Bytes })
	var all TableStorage
	for _, t := range sorted {
		all.Compressed, all.Uncompressed, all.Bytes = all.Compressed+t.Compressed, all.Uncompressed+t.Uncompressed, all.Bytes+t.Bytes
		p("| %s | %d | %d | %s | %s | %s | %.1f | %.1f | %.1f |", t.Table, t.Rows, t.Parts, mb(t.Compressed), mb(t.Uncompressed), mb(t.Bytes),
			float64(t.Compressed)/float64(events), float64(t.Uncompressed)/float64(events), float64(t.Bytes)/float64(events))
	}
	p("| all | | | %s | %s | %s | %.1f | %.1f | %.1f |", mb(all.Compressed), mb(all.Uncompressed), mb(all.Bytes),
		float64(all.Compressed)/float64(events), float64(all.Uncompressed)/float64(events), float64(all.Bytes)/float64(events))
	p("")
}

func firstRate(p Plan) float64 {
	if len(p.Normal) == 0 {
		return 0
	}
	return p.Normal[0].Rate
}

func okMark(ok bool) string {
	if ok {
		return "yes"
	}
	return "**NO**"
}

func ms(us int64) string { return fmt.Sprintf("%.2f", float64(us)/1000) }

func usd(micros int64) string { return fmt.Sprintf("$%.4f", float64(micros)/1e6) }

func mb(b uint64) string { return fmt.Sprintf("%.1f MB", float64(b)/(1<<20)) }

func hasPhase(w *WorldResult, name string) bool {
	for _, s := range w.Phases {
		if s.Name == name {
			return true
		}
	}
	return false
}

// decisionTable groups a phase's decisions by merchant class, path and
// outcome.
func decisionTable(p func(string, ...any), w *WorldResult, phase string) {
	groups := map[string]*Hist{}
	totals := map[string]uint64{}
	for k, h := range w.Decisions {
		parts := strings.SplitN(k, "|", 4)
		if len(parts) != 4 || parts[0] != phase {
			continue
		}
		key := parts[1] + "|" + parts[2] + "|" + parts[3]
		if groups[key] == nil {
			groups[key] = newHist()
		}
		groups[key].Merge(h)
		totals[parts[1]] += h.N
	}
	p("| merchant | path | outcome | decisions | share | p50 ms | p99 ms | p99.9 ms | max ms |")
	p("|---|---|---|---|---|---|---|---|---|")
	keys := sortedKeys(groups)
	for _, k := range keys {
		h := groups[k]
		parts := strings.Split(k, "|")
		share := 0.0
		if t := totals[parts[0]]; t > 0 {
			share = 100 * float64(h.N) / float64(t)
		}
		p("| %s | %s | %s | %d | %.2f%% | %s | %s | %s | %s |", parts[0], parts[1], parts[2], h.N, share, ms(h.Q(0.5)), ms(h.Q(0.99)), ms(h.Q(0.999)), ms(h.Max))
	}
}

// missRates is, per merchant class, the share of a phase's decisions that
// went to the server, were cold, or were held for a sync.
func missRates(w *WorldResult, phase string) string {
	total, miss := map[string]uint64{}, map[string]uint64{}
	for k, h := range w.Decisions {
		parts := strings.SplitN(k, "|", 4)
		if len(parts) != 4 || parts[0] != phase {
			continue
		}
		total[parts[1]] += h.N
		if parts[2] == "server" || parts[2] == "cold" || parts[2] == "held" {
			miss[parts[1]] += h.N
		}
	}
	var out []string
	for _, class := range sortedKeys(total) {
		out = append(out, fmt.Sprintf("%s %.2f%% (%d)", class, 100*float64(miss[class])/float64(max(total[class], 1)), miss[class]))
	}
	return strings.Join(out, "; ")
}

func queryTable(p func(string, ...any), q map[string]QueryStats) {
	if len(q) == 0 {
		return
	}
	p("ClickHouse's own query times (system.query_log):")
	p("")
	p("| query | count | p50 ms | p99 ms | max ms | avg rows read | max memory |")
	p("|---|---|---|---|---|---|---|")
	for _, k := range sortedKeys(q) {
		s := q[k]
		p("| %s | %d | %.0f | %.0f | %d | %.0f | %s |", k, s.Count, s.P50MS, s.P99MS, s.MaxMS, s.AvgRows, mb(s.MaxMemory))
	}
	p("")
}

func rowsAndDisk(p func(string, ...any), rows *RowStats, storage []TableStorage) {
	if rows != nil {
		var types []string
		for _, k := range sortedKeys(rows.ByType) {
			types = append(types, fmt.Sprintf("%s %d", k, rows.ByType[k]))
		}
		p("Rows: %d usage events; %d ledger transfers over %d customers (%.2f a customer, at most %d); by type: %s. %d usage receipts, %d running totals, %d customers with pending usage.",
			rows.UsageEvents, rows.LedgerTransfers, rows.Customers, rows.TransfersPerCustomer, rows.PerCustomerMax, strings.Join(types, ", "), rows.UsageReceipts, rows.UsageTotals, rows.Pending)
		p("")
	}
	if len(storage) == 0 {
		return
	}
	var events uint64
	for _, t := range storage {
		if t.Table == "usage_events" {
			events = t.Rows
		}
	}
	perEvent(p, storage, max(events, 1))
}

func topCores(cores map[string]float64, n int) string {
	type kv struct {
		k string
		v float64
	}
	var all []kv
	for k, v := range cores {
		all = append(all, kv{k, v})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].v > all[j].v })
	var out []string
	for _, e := range all[:min(n, len(all))] {
		out = append(out, fmt.Sprintf("%s %.2f", e.k, e.v))
	}
	return strings.Join(out, ", ")
}
