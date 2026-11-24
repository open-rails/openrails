package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
)

// gateSpec is one gate process: a gate per merchant over the remote client
// (Mode remote) or over an engine of its own (Mode embedded).
type gateSpec struct {
	Proc   int    `json:"proc"`
	Mode   string `json:"mode"`
	URL    string `json:"url"`
	Prefix string `json:"prefix"`

	DSN        string `json:"dsn"`
	Schema     string `json:"schema"`
	RedisURL   string `json:"redis_url"`
	ClickHouse string `json:"clickhouse"`
	CHDatabase string `json:"ch_database"`

	Merchants    []gateMerchant `json:"merchants"`
	Units        [2]int64       `json:"units"`
	UnitPrice    int64          `json:"unit_price"`
	WorkMeanMS   float64        `json:"work_mean_ms"`
	LongShare    float64        `json:"long_share"`
	ReleaseShare float64        `json:"release_share"`

	// Outbox is a file outbox; CaptureLog lists each captured request id.
	Outbox     string `json:"outbox"`
	CaptureLog string `json:"capture_log"`
	Results    string `json:"results"`
}

type gateMerchant struct {
	Slug      string               `json:"slug"`
	ID        billing.MerchantID   `json:"id"`
	Key       string               `json:"key"`
	Class     string               `json:"class"`
	Customers []billing.CustomerID `json:"customers"`
	Drains    []billing.CustomerID `json:"drains"`
}

// control is a command from the coordinator, one JSON line on stdin.
type control struct {
	Op     string             `json:"op"`
	Phase  string             `json:"phase,omitempty"`
	Rates  map[string]float64 `json:"rates,omitempty"`
	Active map[string]int     `json:"active,omitempty"`
	Rate   float64            `json:"rate,omitempty"`
}

// live is what a process reports each second on stdout.
type live struct {
	Proc        int              `json:"proc"`
	At          time.Time        `json:"at"`
	Decisions   int64            `json:"decisions"`
	Unavailable int64            `json:"unavailable"`
	Errors      int64            `json:"errors"`
	Inflight    int64            `json:"inflight"`
	Shed        int64            `json:"shed"`
	Lagged      int64            `json:"lagged"`
	Captured    map[string]int64 `json:"captured"`
	HotP50      int64            `json:"hot_p50_us"`
	HotP99      int64            `json:"hot_p99_us"`
	SyncP99     int64            `json:"sync_p99_us"`
	Syncs       int64            `json:"syncs"`
}

// admission is one allowed decision of a draining customer.
type admission struct {
	Merchant string `json:"m"`
	Customer string `json:"c"`
	Proc     int    `json:"p"`
	At       int64  `json:"t"`
	Cost     int64  `json:"x"`
	Path     string `json:"path"`
}

// procResult is a gate process's final report.
type procResult struct {
	Proc        int                         `json:"proc"`
	Mode        string                      `json:"mode"`
	Decisions   map[string]*Hist            `json:"decisions"`
	Syncs       map[string]*Hist            `json:"syncs"`
	Counters    map[string]int64            `json:"counters"`
	Captured    map[string]map[string]int64 `json:"captured"`
	Amounts     map[string]map[string]int64 `json:"amounts"`
	Admissions  []admission                 `json:"admissions"`
	Errors      []string                    `json:"errors"`
	FlushErrors []string                    `json:"flush_errors"`
}

const maxInflight = 50_000

type probeKey struct{}

// probe records that a decision reached the server in its own context: an
// admission over HTTP, or any query of the embedded engine.
type probe struct{ server atomic.Bool }

type probeTracer struct{}

func (probeTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	if p, ok := ctx.Value(probeKey{}).(*probe); ok {
		p.server.Store(true)
	}
	return ctx
}

func (probeTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

type worker struct {
	spec  gateSpec
	ctx   context.Context
	phase atomic.Pointer[string]
	ms    []*mrun
	drop  atomic.Uint64

	decisions *hists
	syncs     *hists

	cmu      sync.Mutex
	counters map[string]int64
	errs     []string

	wmu        sync.Mutex
	window     *Hist
	syncWindow *Hist

	inflight, shed, lagged, total, unavailable, failed, syncCount atomic.Int64
	wg                                                            sync.WaitGroup
	stopping                                                      atomic.Bool

	seq atomic.Int64

	amu        sync.Mutex
	admissions []admission

	pmu      sync.Mutex
	captured map[string]map[string]int64
	amounts  map[string]map[string]int64
	capLog   *os.File
	logMu    sync.Mutex
}

type mrun struct {
	gateMerchant
	gate      *openrails.Gate
	rate      atomic.Uint64
	active    atomic.Int64
	drainRate atomic.Uint64
	seen      sync.Map
	syncs     atomic.Int64
	captured  atomic.Int64
}

func loadFloat(a *atomic.Uint64) float64     { return math.Float64frombits(a.Load()) }
func storeFloat(a *atomic.Uint64, v float64) { a.Store(math.Float64bits(v)) }

func (w *worker) phaseName() string { return *w.phase.Load() }

func (w *worker) count(key string, n int64) {
	w.cmu.Lock()
	w.counters[key] += n
	w.cmu.Unlock()
}

func (w *worker) noteErr(err error) {
	w.cmu.Lock()
	if len(w.errs) < 50 {
		w.errs = append(w.errs, err.Error())
	}
	w.cmu.Unlock()
}

// wire is a remote gate's transport: it marks a decision's probe when the
// gate admits on the server, times syncs and can lose a sync's reply after
// the server answered it.
type wire struct {
	base http.RoundTripper
	w    *worker
	m    *mrun
}

func (t *wire) RoundTrip(r *http.Request) (*http.Response, error) {
	if p, ok := r.Context().Value(probeKey{}).(*probe); ok && r.URL.Path == "/v1/app/admissions" {
		p.server.Store(true)
	}
	if r.URL.Path != "/v1/app/leases/sync" {
		return t.base.RoundTrip(r)
	}
	start := time.Now()
	resp, err := t.base.RoundTrip(r)
	lat := time.Since(start)
	key := t.w.phaseName() + "|" + t.m.Class
	t.m.syncs.Add(1)
	t.w.syncCount.Add(1)
	t.w.count("syncs|"+key, 1)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.w.count("sync_failures|"+key, 1)
		return resp, err
	}
	t.w.syncs.add(key, lat)
	if t.m.Class == "hot" {
		t.w.wmu.Lock()
		t.w.syncWindow.Add(lat)
		t.w.wmu.Unlock()
	}
	if share := loadFloat(&t.w.drop); share > 0 && rand.Float64() < share {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		t.w.count("lost_replies|"+key, 1)
		return nil, errors.New("load: lost the sync's reply")
	}
	return resp, nil
}

func gatesCmd(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: load gates SPEC.json")
	}
	var spec gateSpec
	if err := readJSON(args[0], &spec); err != nil {
		return err
	}
	w := &worker{spec: spec, ctx: context.WithoutCancel(ctx), decisions: newHists(), syncs: newHists(), counters: map[string]int64{},
		window: newHist(), syncWindow: newHist(), captured: map[string]map[string]int64{}, amounts: map[string]map[string]int64{}}
	phase := "start"
	w.phase.Store(&phase)
	if spec.CaptureLog != "" {
		f, err := os.OpenFile(spec.CaptureLog, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		defer f.Close()
		w.capLog = f
	}
	var deps openrails.GateDeps
	if spec.Outbox != "" {
		ob, err := openOutbox(spec.Outbox)
		if err != nil {
			return err
		}
		defer ob.close()
		deps.Outbox = ob
	}
	for _, gm := range spec.Merchants {
		w.ms = append(w.ms, &mrun{gateMerchant: gm})
		w.captured[gm.Slug], w.amounts[gm.Slug] = map[string]int64{}, map[string]int64{}
	}
	clients, closeAll, err := gateClients(ctx, w)
	if err != nil {
		return err
	}
	defer closeAll()
	for i, m := range w.ms {
		if m.gate, err = openrails.NewGate(clients[i], deps); err != nil {
			return fmt.Errorf("gate %s: %w", m.Slug, err)
		}
	}
	done := make(chan struct{})
	go w.report(done)
	for _, m := range w.ms {
		go w.arrivals(m, m.Customers, &m.rate, &m.active, false)
		for _, c := range m.Drains {
			go w.arrivals(m, []billing.CustomerID{c}, &m.drainRate, nil, true)
		}
	}
	w.controls(os.Stdin)
	w.stopping.Store(true)
	waited := make(chan struct{})
	go func() { w.wg.Wait(); close(waited) }()
	select {
	case <-waited:
	case <-time.After(30 * time.Second):
		w.noteErr(errors.New("requests still open 30s after stop"))
	}
	var flushErrs []string
	var fmu sync.Mutex
	var fwg sync.WaitGroup
	for _, m := range w.ms {
		fwg.Go(func() {
			note := func(what string, err error) {
				fmu.Lock()
				flushErrs = append(flushErrs, m.Slug+" "+what+": "+err.Error())
				fmu.Unlock()
			}
			fctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			if err := m.gate.Flush(fctx); err != nil {
				note("flush", err)
			}
			cancel()
			cctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			if err := m.gate.Close(cctx); err != nil {
				note("close", err)
			}
			cancel()
		})
	}
	fwg.Wait()
	close(done)
	w.pmu.Lock()
	w.cmu.Lock()
	w.amu.Lock()
	defer w.amu.Unlock()
	res := procResult{Proc: spec.Proc, Mode: spec.Mode, Decisions: w.decisions.snapshot(), Syncs: w.syncs.snapshot(), Counters: w.counters,
		Captured: w.captured, Amounts: w.amounts, Admissions: w.admissions, Errors: w.errs, FlushErrors: flushErrs}
	w.cmu.Unlock()
	w.pmu.Unlock()
	return writeJSON(spec.Results, res)
}

// gateClients are the merchant handles the gates use, and what closes them.
func gateClients(ctx context.Context, w *worker) ([]*openrails.Client, func(), error) {
	spec := w.spec
	var out []*openrails.Client
	switch spec.Mode {
	case "remote":
		var remotes []*openrails.Remote
		for i, gm := range spec.Merchants {
			base := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
				MaxIdleConns: 1024, MaxIdleConnsPerHost: 512, IdleConnTimeout: 90 * time.Second}
			r, err := openrails.NewRemote(spec.URL, openrails.RemoteOptions{HTTPClient: &http.Client{Transport: &wire{base: base, w: w, m: w.ms[i]}},
				Credentials: map[billing.MerchantID]openrails.RemoteCredential{gm.ID: {APIKey: gm.Key}}, Timeout: 15 * time.Second})
			if err != nil {
				return nil, nil, err
			}
			c, err := r.Merchant(gm.ID)
			if err != nil {
				return nil, nil, err
			}
			remotes = append(remotes, r)
			out = append(out, c)
		}
		return out, func() {
			for _, r := range remotes {
				_ = r.Close(context.Background())
			}
		}, nil
	case "embedded":
		pool, err := openPool(ctx, spec.DSN, 16, probeTracer{})
		if err != nil {
			return nil, nil, err
		}
		ch, err := openClickHouse(spec.ClickHouse)
		if err != nil {
			pool.Close()
			return nil, nil, err
		}
		rdb, err := openRedis(spec.RedisURL)
		if err != nil {
			pool.Close()
			return nil, nil, err
		}
		var slugs []string
		for _, gm := range spec.Merchants {
			slugs = append(slugs, gm.Slug)
		}
		eng, err := openrails.New(ctx, embeddedConfig(spec.Schema, spec.CHDatabase, slugs), openrails.Deps{Postgres: pool, Redis: rdb, ClickHouse: ch, FXTransport: fxWire{}, Logger: quietLogger(os.Stderr)})
		if err != nil {
			pool.Close()
			return nil, nil, err
		}
		for _, gm := range spec.Merchants {
			c, err := eng.Merchant(gm.ID)
			if err != nil {
				return nil, nil, err
			}
			out = append(out, c)
		}
		return out, func() {
			_ = eng.Close(context.Background())
			_ = ch.Close()
			if rdb != nil {
				_ = rdb.Close()
			}
			pool.Close()
		}, nil
	}
	return nil, nil, fmt.Errorf("unknown mode %q", spec.Mode)
}

// embeddedConfig is the engine every embedded process and the coordinator
// open on a world's schema, declaring the same merchants.
func embeddedConfig(schema, chdb string, slugs []string) openrails.Config {
	cfg := openrails.Config{Database: openrails.DatabaseConfig{Schema: schema, RiverSchema: schema}, ClickHouse: openrails.ClickHouseConfig{Database: chdb},
		ProviderWriteMode: openrails.ProviderWritesReadOnly, RateLimits: openrails.RateLimitsConfig{}}
	for _, s := range slugs {
		cfg.Merchants = append(cfg.Merchants, billing.MerchantDeclaration{MerchantDocument: billing.MerchantDocument{Slug: s, DisplayName: strings.ToUpper(s)}})
	}
	return cfg
}

func (w *worker) controls(in io.Reader) {
	sc := bufio.NewScanner(in)
	for sc.Scan() {
		var c control
		if json.Unmarshal(sc.Bytes(), &c) != nil {
			continue
		}
		switch c.Op {
		case "phase":
			p := c.Phase
			w.phase.Store(&p)
		case "rates":
			for _, m := range w.ms {
				if r, ok := c.Rates[m.Slug]; ok {
					storeFloat(&m.rate, r)
				}
				if n, ok := c.Active[m.Slug]; ok {
					m.active.Store(int64(n))
				}
			}
		case "drain":
			for _, m := range w.ms {
				storeFloat(&m.drainRate, c.Rate)
			}
		case "drop":
			storeFloat(&w.drop, c.Rate)
		case "stop":
			return
		}
	}
}

// arrivals starts requests for the first active customers (all, without a
// count) at the rate rate holds, as a Poisson process on an absolute
// schedule: a late wake-up catches up.
func (w *worker) arrivals(m *mrun, customers []billing.CustomerID, rate *atomic.Uint64, active *atomic.Int64, drain bool) {
	if len(customers) == 0 {
		return
	}
	next := time.Now()
	for !w.stopping.Load() {
		r := loadFloat(rate)
		if r <= 0 {
			time.Sleep(50 * time.Millisecond)
			next = time.Now()
			continue
		}
		next = next.Add(time.Duration(rand.ExpFloat64() / r * float64(time.Second)))
		if d := time.Until(next); d > 0 {
			time.Sleep(d)
		} else if d < -time.Second {
			w.lagged.Add(1)
			next = time.Now()
		}
		if w.inflight.Load() >= maxInflight {
			w.shed.Add(1)
			continue
		}
		n := len(customers)
		if active != nil {
			if a := int(active.Load()); a > 0 && a < n {
				n = a
			}
		}
		w.inflight.Add(1)
		w.wg.Add(1)
		go w.request(m, customers[rand.IntN(n)], drain)
	}
}

func (w *worker) workTime() time.Duration {
	if rand.Float64() < w.spec.LongShare {
		return 2*time.Second + time.Duration(rand.Int64N(int64(4*time.Second)))
	}
	return min(time.Duration(rand.ExpFloat64()*w.spec.WorkMeanMS*float64(time.Millisecond)), 3*time.Second)
}

// held reports a ticket the server decided as a request hold; it is set
// before Admit returns and never changes.
func held(t *openrails.Ticket) bool {
	v := reflect.ValueOf(t).Elem().FieldByName("held")
	return v.IsValid() && v.Kind() == reflect.Bool && v.Bool()
}

func (w *worker) request(m *mrun, customer billing.CustomerID, drain bool) {
	defer w.wg.Done()
	defer w.inflight.Add(-1)
	units := w.spec.Units[0] + rand.Int64N(w.spec.Units[1]-w.spec.Units[0]+1)
	id := fmt.Sprintf("%s-%d", w.spec.Prefix, w.seq.Add(1))
	p := billing.AdmitParams{RequestID: id, CustomerID: customer, Invoker: customer.String(), InvokerType: billing.InvokerTypeCustomer, Currency: "USD",
		Usage: &billing.UsageEstimate{EventType: "compute", Dimensions: map[string]int64{"units": units}}}
	pr := &probe{}
	ctx := context.WithValue(w.ctx, probeKey{}, pr)
	phase := w.phaseName()
	syncsBefore := m.syncs.Load()
	start := time.Now()
	t, err := m.gate.Admit(ctx, p)
	lat := time.Since(start)
	_, known := m.seen.Load(customer)
	path := "local"
	switch {
	case pr.server.Load() || err == nil && t.Allowed() && held(t):
		path = "server"
	case !known:
		path = "cold"
	case strings.HasPrefix(phase, "ch-outage"):
		path = "offline"
	}
	outcome := "allowed"
	switch {
	case errors.Is(err, openrails.ErrUnavailable):
		outcome = "unavailable"
		w.unavailable.Add(1)
	case err != nil:
		outcome = "error"
		w.failed.Add(1)
		w.noteErr(err)
	case !t.Allowed():
		outcome = "refused:" + string(t.Refusal().DenyCode)
	}
	if err == nil {
		m.seen.Store(customer, struct{}{})
	}
	class := m.Class
	if drain {
		class = "drain"
	}
	w.decisions.add(phase+"|"+class+"|"+path+"|"+outcome, lat)
	if path == "local" && lat >= time.Millisecond && (w.spec.Mode == "embedded" || m.syncs.Load() != syncsBefore) {
		w.count("waited|"+phase+"|"+class, 1)
	}
	if class == "hot" {
		w.wmu.Lock()
		w.window.Add(lat)
		w.wmu.Unlock()
	}
	w.total.Add(1)
	if err != nil || !t.Allowed() {
		return
	}
	cost := units * w.spec.UnitPrice
	if drain {
		w.amu.Lock()
		w.admissions = append(w.admissions, admission{Merchant: m.Slug, Customer: customer.String(), Proc: w.spec.Proc, At: start.UnixNano(), Cost: cost, Path: path})
		w.amu.Unlock()
	}
	time.Sleep(w.workTime())
	if rand.Float64() < w.spec.ReleaseShare {
		_ = t.Release(w.ctx)
		return
	}
	if err := t.Capture(w.ctx, billing.CaptureUsage{Dimensions: map[string]int64{"units": units}}); err != nil {
		w.count("capture_errors|"+phase+"|"+class, 1)
		w.noteErr(fmt.Errorf("capture: %w", err))
		return
	}
	m.captured.Add(1)
	w.pmu.Lock()
	w.captured[m.Slug][customer.String()]++
	w.amounts[m.Slug][customer.String()] += cost
	w.pmu.Unlock()
	if w.capLog != nil {
		w.logMu.Lock()
		_, _ = fmt.Fprintf(w.capLog, "%s %s %d\n", id, customer, cost)
		w.logMu.Unlock()
	}
}

// report writes one live line a second until done.
func (w *worker) report(done <-chan struct{}) {
	enc := json.NewEncoder(os.Stdout)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-done:
			return
		case <-tick.C:
		}
		w.wmu.Lock()
		win, sw := w.window, w.syncWindow
		w.window, w.syncWindow = newHist(), newHist()
		w.wmu.Unlock()
		l := live{Proc: w.spec.Proc, At: time.Now(), Decisions: w.total.Load(), Unavailable: w.unavailable.Load(), Errors: w.failed.Load(),
			Inflight: w.inflight.Load(), Shed: w.shed.Load(), Lagged: w.lagged.Load(), Captured: map[string]int64{},
			HotP50: win.Q(0.5), HotP99: win.Q(0.99), SyncP99: sw.Q(0.99), Syncs: w.syncCount.Load()}
		for _, m := range w.ms {
			l.Captured[m.Slug] = m.captured.Load()
		}
		_ = enc.Encode(l)
	}
}

// fileOutbox is a host's durable outbox as an append-only file: a Put is one
// write of its items, an Ack one of their references.
type fileOutbox struct {
	mu      sync.Mutex
	f       *os.File
	pending map[billing.LeaseUsageRef]billing.LeaseUsage
}

type outboxRecord struct {
	Put []billing.OutboxItem    `json:"put,omitempty"`
	Ack []billing.LeaseUsageRef `json:"ack,omitempty"`
}

func openOutbox(path string) (*fileOutbox, error) {
	o := &fileOutbox{pending: map[billing.LeaseUsageRef]billing.LeaseUsage{}}
	if data, err := os.ReadFile(path); err == nil {
		for line := range strings.SplitSeq(string(data), "\n") {
			var r outboxRecord
			if json.Unmarshal([]byte(line), &r) != nil {
				continue
			}
			for _, it := range r.Put {
				o.pending[billing.LeaseUsageRef{LeaseID: it.LeaseID, N: it.Usage.N}] = it.Usage
			}
			for _, ref := range r.Ack {
				delete(o.pending, ref)
			}
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	o.f = f
	return o, nil
}

func (o *fileOutbox) close() { _ = o.f.Close() }

func (o *fileOutbox) write(r outboxRecord) error {
	line, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = o.f.Write(append(line, '\n'))
	return err
}

func (o *fileOutbox) Put(_ context.Context, items []billing.OutboxItem) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := o.write(outboxRecord{Put: items}); err != nil {
		return err
	}
	for _, it := range items {
		o.pending[billing.LeaseUsageRef{LeaseID: it.LeaseID, N: it.Usage.N}] = it.Usage
	}
	return nil
}

func (o *fileOutbox) Pending(context.Context) ([]billing.OutboxItem, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]billing.OutboxItem, 0, len(o.pending))
	for ref, u := range o.pending {
		out = append(out, billing.OutboxItem{LeaseID: ref.LeaseID, Usage: u})
	}
	return out, nil
}

func (o *fileOutbox) Ack(_ context.Context, refs []billing.LeaseUsageRef) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := o.write(outboxRecord{Ack: refs}); err != nil {
		return err
	}
	for _, r := range refs {
		delete(o.pending, r)
	}
	return nil
}
