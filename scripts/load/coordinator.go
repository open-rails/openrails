package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
)

// Results is results.json.
type Results struct {
	Started  time.Time               `json:"started"`
	Finished time.Time               `json:"finished"`
	Plan     Plan                    `json:"plan"`
	Host     map[string]string       `json:"host"`
	Postgres map[string]string       `json:"postgres_settings"`
	Worlds   map[string]*WorldResult `json:"worlds"`
	Posting  *PostingResult          `json:"posting,omitempty"`
	Errors   []string                `json:"errors"`
}

// WorldResult is one world: the standalone server over HTTP, or embedded
// engines.
type WorldResult struct {
	Name       string           `json:"name"`
	Mode       string           `json:"mode"`
	Phases     []Span           `json:"phases"`
	Decisions  map[string]*Hist `json:"decisions"`
	Syncs      map[string]*Hist `json:"syncs"`
	Counters   map[string]int64 `json:"counters"`
	Steps      []Step           `json:"steps"`
	Saturation string           `json:"saturation"`
	// CooldownSeconds is how long the settlement backlog took to settle at
	// the base rates after the ramp; -1 when it did not within four minutes.
	CooldownSeconds float64               `json:"cooldown_seconds"`
	TopUps          *Hist                 `json:"top_ups,omitempty"`
	BillingPass     *PassResult           `json:"billing_pass,omitempty"`
	Checks          []Check               `json:"checks"`
	Overshoot       *Overshoot            `json:"overshoot,omitempty"`
	Storage         []TableStorage        `json:"clickhouse_storage"`
	Queries         map[string]QueryStats `json:"clickhouse_queries"`
	Rows            *RowStats             `json:"rows,omitempty"`
	Samples         []Sample              `json:"samples"`
	Outage          *OutageResult         `json:"outage,omitempty"`
	Crash           []CrashResult         `json:"crash,omitempty"`
	ProcErrors      []string              `json:"process_errors"`
	Errors          []string              `json:"errors"`
	smu             sync.Mutex
	captured        map[string]map[string]int64 // merged, by merchant slug and customer
	amounts         map[string]map[string]int64
	admissions      []admission
}

type Span struct {
	Name  string    `json:"name"`
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

type PassResult struct {
	Attempts   int       `json:"attempts"`
	EnqueuedAt time.Time `json:"enqueued_at"`
	Seconds    float64   `json:"seconds"`
	State      string    `json:"state"`
	Error      string    `json:"error,omitempty"`
}

func (w *WorldResult) fail(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(os.Stderr, "[%s] %s\n", w.Name, msg)
	w.Errors = append(w.Errors, msg)
}

func (w *WorldResult) check(name string, ok bool, format string, args ...any) {
	w.Checks = append(w.Checks, Check{Name: name, OK: ok, Detail: fmt.Sprintf(format, args...)})
	fmt.Fprintf(os.Stderr, "[%s] check %s ok=%v %s\n", w.Name, name, ok, fmt.Sprintf(format, args...))
}

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "%s %s\n", time.Now().UTC().Format("15:04:05"), fmt.Sprintf(format, args...))
}

// env is what every world shares: the services and the run's directory.
type env struct {
	plan Plan
	out  string
	pool *pgxpool.Pool
	ch   driver.Conn
	self string
}

func coordinate(ctx context.Context, plan Plan, out string) error {
	res := &Results{Started: time.Now().UTC(), Plan: plan, Host: hostInfo(), Worlds: map[string]*WorldResult{}}
	defer func() {
		res.Finished = time.Now().UTC()
		if err := writeJSON(filepath.Join(out, "results.json"), res); err != nil {
			logf("write results: %v", err)
		}
		if err := os.WriteFile(filepath.Join(out, "summary.md"), []byte(summarize(res)), 0o644); err != nil {
			logf("write summary: %v", err)
		}
	}()
	self, err := os.Executable()
	if err != nil {
		return err
	}
	pool, err := openPool(ctx, plan.DSN, 16, nil)
	if err != nil {
		return fmt.Errorf("postgres: %w", err)
	}
	defer pool.Close()
	ch, err := openClickHouse(plan.ClickHouse)
	if err != nil {
		return fmt.Errorf("clickhouse: %w", err)
	}
	defer ch.Close()
	res.Postgres = postgresSettings(ctx, pool)
	e := &env{plan: plan, out: out, pool: pool, ch: ch, self: self}
	for _, name := range plan.Worlds {
		if ctx.Err() != nil {
			break
		}
		start := time.Now()
		logf("world %s: start", name)
		func() {
			// A world that panics leaves the others to run.
			defer func() {
				if p := recover(); p != nil {
					res.Errors = append(res.Errors, fmt.Sprintf("world %s panicked: %v", name, p))
				}
			}()
			switch name {
			case "http":
				res.Worlds[name] = e.runHTTP(ctx)
			case "embedded":
				res.Worlds[name] = e.runEmbedded(ctx)
			case "posting":
				res.Posting = e.runPosting(ctx)
			default:
				res.Errors = append(res.Errors, "unknown world "+name)
			}
		}()
		logf("world %s: done in %s", name, time.Since(start).Round(time.Second))
		_ = writeJSON(filepath.Join(out, "results.json"), res)
	}
	return nil
}

// resetWorld drops and recreates nothing but the world's own schemas and
// ClickHouse database: OpenRails creates them.
func (e *env) resetWorld(ctx context.Context, schema string) error {
	for _, s := range []string{schema, schema + "_auth"} {
		if _, err := e.pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{s}.Sanitize()+" CASCADE"); err != nil {
			return err
		}
	}
	return e.ch.Exec(ctx, "DROP DATABASE IF EXISTS `"+schema+"` SYNC")
}

// child is a process of this binary: a server or a gate process.
type child struct {
	name   string
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	mu     sync.Mutex
	last   live
	exited chan struct{}
	err    error
}

func (e *env) start(name string, args ...string) (*child, error) {
	// A child's lifetime is its own: stop, kill or the parent's death ends it.
	cmd := exec.CommandContext(context.Background(), e.self, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	logFile, err := os.OpenFile(filepath.Join(e.out, name+".log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	cmd.Stderr = logFile
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	c := &child{name: name, cmd: cmd, stdin: stdin, exited: make(chan struct{})}
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			var l live
			if json.Unmarshal(sc.Bytes(), &l) == nil {
				c.mu.Lock()
				c.last = l
				c.mu.Unlock()
			}
		}
		c.err = cmd.Wait()
		_ = logFile.Close()
		close(c.exited)
	}()
	return c, nil
}

func (c *child) send(ctl control) {
	line, _ := json.Marshal(ctl)
	_, _ = c.stdin.Write(append(line, '\n'))
}

func (c *child) live() live {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.last
}

func (c *child) pid() int { return c.cmd.Process.Pid }

func (c *child) running() bool {
	select {
	case <-c.exited:
		return false
	default:
		return true
	}
}

// stop asks the child to finish and waits for it; past timeout it is killed.
func (c *child) stop(timeout time.Duration) error {
	c.send(control{Op: "stop"})
	_ = c.stdin.Close()
	select {
	case <-c.exited:
		return c.err
	case <-time.After(timeout):
		_ = c.cmd.Process.Kill()
		<-c.exited
		return fmt.Errorf("%s did not stop within %s", c.name, timeout)
	}
}

func (c *child) kill() {
	_ = c.cmd.Process.Signal(syscall.SIGKILL)
	<-c.exited
}

func (c *child) terminate(timeout time.Duration) {
	_ = c.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-c.exited:
	case <-time.After(timeout):
		c.kill()
	}
}

// fleet is a world's gate processes.
type fleet struct {
	e        *env
	world    *WorldResult
	ms       []MerchantPlan // the main processes' merchants, the hot one first
	children []*child
	specs    []gateSpec
	mu       sync.Mutex
	phase    string
}

func (f *fleet) add(spec gateSpec, name string) error {
	path := filepath.Join(f.e.out, name+".json")
	if err := writeJSON(path, spec); err != nil {
		return err
	}
	c, err := f.e.start(name, "gates", path)
	if err != nil {
		return err
	}
	f.mu.Lock()
	f.children = append(f.children, c)
	f.specs = append(f.specs, spec)
	phase := f.phase
	f.mu.Unlock()
	if phase != "" {
		c.send(control{Op: "phase", Phase: phase})
	}
	return nil
}

func (f *fleet) all() []*child {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.children)
}

func (f *fleet) send(ctl control) {
	for _, c := range f.all() {
		if c.running() {
			c.send(ctl)
		}
	}
}

func (f *fleet) setPhase(name string) {
	now := time.Now().UTC()
	if n := len(f.world.Phases); n > 0 && f.world.Phases[n-1].End.IsZero() {
		f.world.Phases[n-1].End = now
	}
	f.world.Phases = append(f.world.Phases, Span{Name: name, Start: now})
	f.mu.Lock()
	f.phase = name
	f.mu.Unlock()
	f.send(control{Op: "phase", Phase: name})
	logf("[%s] phase %s", f.world.Name, name)
}

func (f *fleet) endPhase() {
	if n := len(f.world.Phases); n > 0 && f.world.Phases[n-1].End.IsZero() {
		f.world.Phases[n-1].End = time.Now().UTC()
	}
}

// rates sets every main merchant's rate, the hot one's to hot, split
// evenly over the main processes.
func (f *fleet) rates(hot float64) {
	n := float64(f.e.plan.Processes)
	r, active := map[string]float64{}, map[string]int{}
	for _, m := range f.ms {
		r[m.Slug] = m.Rate / n
		active[m.Slug] = f.e.plan.active(m, m.Rate)
	}
	r[f.ms[0].Slug] = hot / n
	active[f.ms[0].Slug] = f.e.plan.active(f.ms[0], hot)
	f.send(control{Op: "rates", Rates: r, Active: active})
}

// cooldown holds the base rates for least, then until the main
// merchants' settlement backlog is under two seconds of their traffic, at
// most four minutes in all; it returns how long the backlog took to settle.
func (f *fleet) cooldown(ctx context.Context, schema string, ids []billing.MerchantID, least time.Duration) float64 {
	start := time.Now()
	sleepCtx(ctx, least)
	limit := int64(2 * f.offered(f.e.plan.Hot.Rate))
	for time.Since(start) < 4*time.Minute && ctx.Err() == nil {
		var captured int64
		for _, m := range f.ms {
			captured += f.totals().Captured[m.Slug]
		}
		stored, err := f.e.countEvents(ctx, schema, ids...)
		if err == nil && captured-stored <= limit {
			return time.Since(start).Seconds()
		}
		sleepCtx(ctx, 2*time.Second)
	}
	return -1
}

// offered is every main merchant's rate with the hot one's at hot.
func (f *fleet) offered(hot float64) float64 {
	for _, m := range f.ms[1:] {
		hot += m.Rate
	}
	return hot
}

// totals sums the processes' latest live reports.
func (f *fleet) totals() live {
	var t live
	t.Captured = map[string]int64{}
	for _, c := range f.all() {
		l := c.live()
		t.Decisions += l.Decisions
		t.Unavailable += l.Unavailable
		t.Errors += l.Errors
		t.Inflight += l.Inflight
		t.Shed += l.Shed
		t.Lagged += l.Lagged
		t.Syncs += l.Syncs
		t.HotP99 = max(t.HotP99, l.HotP99)
		t.SyncP99 = max(t.SyncP99, l.SyncP99)
		for k, v := range l.Captured {
			t.Captured[k] += v
		}
	}
	return t
}

// killRunning kills what is still running: a world that returns early
// leaves no process behind.
func (f *fleet) killRunning() {
	for _, c := range f.all() {
		if c.running() {
			c.kill()
		}
	}
}

func (f *fleet) pids() map[string]int {
	out := map[string]int{}
	for _, c := range f.all() {
		if c.running() {
			out[c.name] = c.pid()
		}
	}
	return out
}

// stopAll stops every process and merges what they report.
func (f *fleet) stopAll() {
	children := f.all()
	var wg sync.WaitGroup
	for _, c := range children {
		if !c.running() {
			continue
		}
		wg.Go(func() {
			if err := c.stop(6 * time.Minute); err != nil {
				f.world.fail("%s: %v", c.name, err)
			}
		})
	}
	wg.Wait()
}

// collect merges the results files of specs.
func (w *WorldResult) collect(specs []gateSpec) {
	if w.Decisions == nil {
		w.Decisions, w.Syncs, w.Counters = map[string]*Hist{}, map[string]*Hist{}, map[string]int64{}
		w.captured, w.amounts = map[string]map[string]int64{}, map[string]map[string]int64{}
	}
	merge := func(dst map[string]*Hist, src map[string]*Hist) {
		for k, h := range src {
			if dst[k] == nil {
				dst[k] = newHist()
			}
			dst[k].Merge(h)
		}
	}
	for _, s := range specs {
		var r procResult
		if err := readJSON(s.Results, &r); err != nil {
			w.fail("results of process %d (%s): %v", s.Proc, s.Results, err)
			continue
		}
		merge(w.Decisions, r.Decisions)
		merge(w.Syncs, r.Syncs)
		for k, v := range r.Counters {
			w.Counters[k] += v
		}
		for _, into := range []struct{ dst, src map[string]map[string]int64 }{{w.captured, r.Captured}, {w.amounts, r.Amounts}} {
			for m, cs := range into.src {
				if into.dst[m] == nil {
					into.dst[m] = map[string]int64{}
				}
				for c, n := range cs {
					into.dst[m][c] += n
				}
			}
		}
		w.admissions = append(w.admissions, r.Admissions...)
		for _, e := range append(r.Errors, r.FlushErrors...) {
			if len(w.ProcErrors) < 100 {
				w.ProcErrors = append(w.ProcErrors, fmt.Sprintf("p%d: %s", s.Proc, e))
			}
		}
	}
}

func freeAddr() (string, error) {
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer ln.Close()
	return ln.Addr().String(), nil
}

// seeded is a merchant's customers once funded.
type seeded struct {
	slug, class string
	id          billing.MerchantID
	key         string
	customers   []billing.CustomerID
	drains      []billing.CustomerID
}

const steadyFunding = 10_000_000_000 // $10,000: never runs out

// seed gives a merchant its meter (UnitPrice per unit) and funds its
// customers: steady ones generously, draining ones with DrainFunding.
func seed(ctx context.Context, c *openrails.Client, plan Plan, s *seeded, customers, drains int) error {
	product, err := c.CreateProduct(ctx, billing.CreateProductParams{Key: "usage-compute", DisplayName: "Usage compute"})
	if err != nil {
		return fmt.Errorf("product: %w", err)
	}
	params := billing.SetMeterParams{EventType: "compute", ValueProperty: "units", Aggregation: catalog.AggregationSum,
		RateCard: catalog.Value(billing.MeterRateCardParams{ProductID: product.ID, Price: catalog.RatePrice{Model: catalog.ModelPerUnit, Currency: "USD", PerUnit: &catalog.PerUnitPrice{UnitAmount: plan.UnitPrice, DivideBy: 1}}})}
	if _, err := c.SetMeter(ctx, "compute", params); err != nil {
		return fmt.Errorf("meter: %w", err)
	}
	var grants []billing.CreateCreditGrantParams
	for i := range customers + drains {
		id := billing.CustomerID(uuid.New())
		amount := int64(steadyFunding)
		if i < customers {
			s.customers = append(s.customers, id)
		} else {
			s.drains = append(s.drains, id)
			amount = plan.DrainFunding
		}
		grants = append(grants, billing.CreateCreditGrantParams{CustomerID: id, Currency: "USD", Amount: amount, Source: "load", SourceID: "fund-" + id.String()})
	}
	return grantAll(ctx, c, grants)
}

func grantAll(ctx context.Context, c *openrails.Client, grants []billing.CreateCreditGrantParams) error {
	for batch := range slices.Chunk(grants, billing.MaxBatchItems) {
		if _, err := c.CreateCreditGrants(ctx, batch); err != nil {
			return fmt.Errorf("grants: %w", err)
		}
	}
	return nil
}

func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// docker runs a docker command, for the ClickHouse outage.
func docker(ctx context.Context, args ...string) error {
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
