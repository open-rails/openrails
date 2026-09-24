//go:build greenfield && integration

package subscriptions_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jonboulle/clockwork"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/embed"
)

// A fleet is N host processes embedding OpenRails over one database and one
// set of provider accounts, as Doujins and Hentai0 deploy: each replica has
// its own connections, River client (own leader election and liveness),
// engine clock and HTTP server. A hard crash cuts the replica's database
// link at once, so nothing it was doing afterwards is recorded.

const replicaHeader = "X-Greenfield-Replica"

// Two fleets at a time keeps the suite inside PostgreSQL's default
// connection limit next to the other parallel worlds.
var fleetSlots = make(chan struct{}, 2)

type fleet struct {
	t        *testing.T
	base     *world
	replicas []*world
	dead     map[*world]bool
	// latency delays every provider charge request, so restarts can land
	// while renewals are in flight.
	latency atomic.Int64
}

// replicaEnv is one process's private resources.
type replicaEnv struct {
	f     *fleet
	name  string
	queue string
	id    string
	gen   int
	proxy *dbProxy
	pool  *pgxpool.Pool
}

// newFleet starts n replicas; skews offsets each replica's engine clock.
func newFleet(t *testing.T, n int, skews ...time.Duration) *fleet {
	t.Helper()
	fleetSlots <- struct{}{}
	t.Cleanup(func() { <-fleetSlots })
	f := &fleet{t: t, base: prepareWorld(t, 4), dead: map[*world]bool{}}
	for i := range n {
		var skew time.Duration
		if i < len(skews) {
			skew = skews[i]
		}
		f.replicas = append(f.replicas, f.spawn(i, skew))
	}
	return f
}

func (f *fleet) spawn(i int, skew time.Duration) *world {
	b := f.base
	name := string(rune('a' + i))
	r := &world{t: f.t, pool: b.pool, dsn: b.dsn, schema: b.schema, slug: b.slug, stripe: b.stripe, nmi: b.nmi, auth: b.auth, cfg: b.cfg,
		clock:   clockwork.NewFakeClockAt(b.clock.Now().Add(skew)),
		replica: &replicaEnv{f: f, name: name, queue: "replica_" + name}}
	r.start()
	f.t.Cleanup(r.stop)
	return r
}

// connect opens this process generation's database link through its own
// proxy and wraps the shared providers so each request names its sender.
func (e *replicaEnv) connect(w *world) (*pgxpool.Pool, string, http.RoundTripper, http.RoundTripper) {
	t := w.t
	e.gen++
	e.id = fmt.Sprintf("gf-%s-%d", e.name, e.gen)
	config, err := pgxpool.ParseConfig(w.dsn)
	require.NoError(t, err)
	target := net.JoinHostPort(config.ConnConfig.Host, strconv.Itoa(int(config.ConnConfig.Port)))
	e.proxy = newDBProxy(t, target)
	host, port, err := net.SplitHostPort(e.proxy.addr())
	require.NoError(t, err)
	p, err := strconv.Atoi(port)
	require.NoError(t, err)
	config.ConnConfig.Host, config.ConnConfig.Port, config.ConnConfig.Fallbacks = host, uint16(p), nil
	config.ConnConfig.RuntimeParams["application_name"] = e.application(w.schema)
	config.MaxConns = 8
	e.pool, err = pgxpool.NewWithConfig(t.Context(), config)
	require.NoError(t, err)
	u, err := url.Parse(w.dsn)
	require.NoError(t, err)
	require.Contains(t, []string{"postgres", "postgresql"}, u.Scheme, "OPENRAILS_GREENFIELD_DSN must be a postgres:// URL for replica links")
	u.Host = e.proxy.addr()
	tag := func(next http.RoundTripper) http.RoundTripper {
		return replicaTransport{name: e.name, next: next, latency: &e.f.latency}
	}
	return e.pool, u.String(), tag(w.stripe), tag(w.nmi)
}

func (e *replicaEnv) application(schema string) string {
	return strings.TrimPrefix(schema, "gf_subs_") + "-" + e.name
}

func (e *replicaEnv) configureRiver(c *river.Config) {
	c.ID = e.id
	c.Queues[embed.QueueBilling] = river.QueueConfig{MaxWorkers: 2}
	c.Queues[e.queue] = river.QueueConfig{MaxWorkers: 1}
}

// disconnect closes this generation's connections. A pool left with a
// wedged connection after a crash is abandoned rather than awaited.
func (e *replicaEnv) disconnect() {
	if e.pool != nil {
		pool := e.pool
		done := make(chan struct{})
		go func() { pool.Close(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
		e.pool = nil
	}
	if e.proxy != nil {
		e.proxy.kill()
		e.proxy = nil
	}
}

type replicaTransport struct {
	name    string
	next    http.RoundTripper
	latency *atomic.Int64
}

func (t replicaTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if d := time.Duration(t.latency.Load()); d > 0 && chargeRequest(r) {
		select {
		case <-time.After(d):
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	}
	r = r.Clone(r.Context())
	r.Header.Set(replicaHeader, t.name)
	return t.next.RoundTrip(r)
}

func chargeRequest(r *http.Request) bool {
	return (r.Method == http.MethodPost && r.URL.Path == "/v1/payment_intents") || strings.HasSuffix(r.URL.Path, "/transact.php")
}

// dbProxy is a replica's network path to PostgreSQL.
type dbProxy struct {
	ln     net.Listener
	target string
	mu     sync.Mutex
	conns  map[net.Conn]struct{}
	dead   bool
}

func newDBProxy(t *testing.T, target string) *dbProxy {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	p := &dbProxy{ln: ln, target: target, conns: map[net.Conn]struct{}{}}
	go p.serve()
	return p
}

func (p *dbProxy) addr() string { return p.ln.Addr().String() }

func (p *dbProxy) serve() {
	for {
		down, err := p.ln.Accept()
		if err != nil {
			return
		}
		up, err := net.Dial("tcp", p.target)
		if err != nil {
			_ = down.Close()
			continue
		}
		if !p.track(down, up) {
			continue
		}
		go p.pipe(down, up)
		go p.pipe(up, down)
	}
}

func (p *dbProxy) track(conns ...net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.dead {
		for _, c := range conns {
			_ = c.Close()
		}
		return false
	}
	for _, c := range conns {
		p.conns[c] = struct{}{}
	}
	return true
}

func (p *dbProxy) pipe(dst, src net.Conn) {
	_, _ = io.Copy(dst, src)
	_ = dst.Close()
	_ = src.Close()
}

// kill drops every connection and refuses new ones.
func (p *dbProxy) kill() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dead = true
	_ = p.ln.Close()
	for c := range p.conns {
		_ = c.Close()
	}
}

func (f *fleet) live() []*world {
	var out []*world
	for _, r := range f.replicas {
		if !f.dead[r] && r.jobs != nil {
			out = append(out, r)
		}
	}
	return out
}

func (f *fleet) any() *world {
	live := f.live()
	require.NotEmpty(f.t, live, "a live replica")
	return live[0]
}

func (f *fleet) other(r *world) *world {
	for _, o := range f.live() {
		if o != r {
			return o
		}
	}
	f.t.Fatal("no other live replica")
	return nil
}

func (f *fleet) named(name string) *world {
	for _, r := range f.replicas {
		if r.replica.name == name {
			return r
		}
	}
	f.t.Fatalf("no replica %q", name)
	return nil
}

// advance moves every replica's engine clock, preserving their skew.
func (f *fleet) advance(d time.Duration) {
	f.base.clock.Advance(d)
	for _, r := range f.replicas {
		r.clock.Advance(d)
	}
}

// toDue moves engine time just past the latest paid period of cases.
func (f *fleet) toDue(cases ...*engineCase) {
	var end time.Time
	for _, e := range cases {
		if p := f.periodEnd(e); p.After(end) {
			end = p
		}
	}
	f.advance(end.Sub(f.base.clock.Now()) + time.Second)
}

func (f *fleet) q(sql string) string {
	return strings.ReplaceAll(sql, "openrails.", pgx.Identifier{f.base.schema}.Sanitize()+".")
}

// pass is one due pass queued on one replica's private queue.
type pass struct {
	r  *world
	id int64
}

// startPasses runs a due pass on each replica at once (every live replica
// when none is named).
func (f *fleet) startPasses(on ...*world) []pass {
	f.t.Helper()
	if len(on) == 0 {
		on = f.live()
	}
	var out []pass
	for _, r := range on {
		res, err := f.any().jobs.Insert(f.t.Context(), dunningPass{}, &river.InsertOpts{Queue: r.replica.queue})
		require.NoError(f.t, err)
		out = append(out, pass{r: r, id: res.Job.ID})
	}
	return out
}

// awaitPasses waits for every pass to complete on the replica it was queued
// to, without errors.
func (f *fleet) awaitPasses(passes []pass) {
	f.t.Helper()
	for _, p := range passes {
		var job *rivertype.JobRow
		require.Eventually(f.t, func() bool {
			var err error
			job, err = f.any().jobs.JobGet(f.t.Context(), p.id)
			if err == nil && job.State == rivertype.JobStateRetryable {
				_, _ = f.any().jobs.JobRetry(f.t.Context(), p.id)
			}
			return err == nil && job.State == rivertype.JobStateCompleted
		}, 60*time.Second, 20*time.Millisecond, "due pass on replica %s", p.r.replica.name)
		require.Contains(f.t, job.AttemptedBy[len(job.AttemptedBy)-1], "gf-"+p.r.replica.name+"-", "the pass ran on its own replica")
	}
	f.settle()
}

// passes runs one due pass on every live replica at once and waits for all.
func (f *fleet) passes() { f.awaitPasses(f.startPasses()) }

func (f *fleet) settle() { f.any().settle() }

func (f *fleet) wake() { f.any().wake() }

// until steps every clock ten minutes and wakes due work until cond holds.
func (f *fleet) until(cond func() bool, msg string) {
	f.t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			f.any().dumpJobs()
			f.t.Fatalf("timed out: %s", msg)
		}
		f.advance(10 * time.Minute)
		f.wake()
		time.Sleep(50 * time.Millisecond)
	}
}

// crash kills r as SIGKILL does: its database link is cut first, so neither
// its jobs nor its operations record anything more. Its running jobs are
// then aged past the rescue silence, as elapsed time would.
func (f *fleet) crash(r *world) {
	f.t.Helper()
	id := r.replica.id
	r.replica.proxy.kill()
	r.stop()
	f.dead[r] = true
	_, err := f.base.pool.Exec(f.t.Context(), f.q(`UPDATE openrails.river_job SET attempted_at = now() - interval '10 minutes'
		WHERE state = 'running' AND attempted_by[array_length(attempted_by, 1)] = $1`), id)
	require.NoError(f.t, err)
}

// revive starts a crashed replica's next process generation.
func (f *fleet) revive(r *world) {
	f.t.Helper()
	delete(f.dead, r)
	r.start()
}

// restart is a rolling deploy's graceful replacement of one process.
func (f *fleet) restart(r *world) {
	f.t.Helper()
	r.stop()
	r.start()
}

// recover lets the survivors take over: leases lapse, the silent jobs are
// rescued, and parked work is woken.
func (f *fleet) recover() {
	f.t.Helper()
	f.advance(30 * time.Minute)
	f.any().rescue()
	f.wake()
}

// leader is the replica holding River leadership.
func (f *fleet) leader() *world {
	var id string
	err := f.base.pool.QueryRow(f.t.Context(), f.q(`SELECT leader_id FROM openrails.river_leader WHERE expires_at > now()`)).Scan(&id)
	if err != nil {
		return nil
	}
	for _, r := range f.live() {
		if r.replica.id == id {
			return r
		}
	}
	return nil
}

func (f *fleet) awaitLeader(not *world) *world {
	f.t.Helper()
	var leader *world
	require.Eventually(f.t, func() bool {
		leader = f.leader()
		return leader != nil && leader != not
	}, 90*time.Second, 100*time.Millisecond, "a live replica leads River")
	return leader
}

// rowLock holds a customer's spend lock, the row every admission,
// execution fence and completion takes first: a barrier for forcing races.
type rowLock struct {
	f  *fleet
	tx pgx.Tx
}

func (f *fleet) lockCustomer(e *engineCase) *rowLock {
	f.t.Helper()
	tx, err := f.base.pool.Begin(f.t.Context())
	require.NoError(f.t, err)
	var id string
	require.NoError(f.t, tx.QueryRow(f.t.Context(), f.q(`SELECT c.id::text FROM openrails.customers c
		JOIN openrails.subscriptions s ON s.merchant_id = c.merchant_id AND s.customer_id = c.id
		WHERE s.id = $1 FOR UPDATE OF c`), subUUID(e.sub)).Scan(&id))
	return &rowLock{f: f, tx: tx}
}

// awaitWaiters waits until each named replica has a connection blocked on a
// lock (the barrier).
func (l *rowLock) awaitWaiters(on ...*world) {
	f := l.f
	f.t.Helper()
	require.Eventually(f.t, func() bool {
		rows, err := f.base.pool.Query(f.t.Context(), `SELECT DISTINCT application_name FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND datname = current_database()`)
		if err != nil {
			return false
		}
		waiting, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return false
		}
		for _, r := range on {
			found := false
			for _, app := range waiting {
				found = found || app == r.replica.application(f.base.schema)
			}
			if !found {
				return false
			}
		}
		return true
	}, 30*time.Second, 10*time.Millisecond, "every contender reaches the barrier")
}

func (l *rowLock) release() {
	require.NoError(l.f.t, l.tx.Commit(l.f.t.Context()))
}

// held is a provider gate that records which replica it caught.
type held struct {
	f    *fleet
	g    *gate
	mu   sync.Mutex
	by   string
	rail string
}

// hold parks the first matching provider request of rail; later ones pass.
// match runs under the fake's lock and must not call its locking methods.
func (f *fleet) hold(rail string, match func(*http.Request) bool, commit bool) *held {
	h := &held{f: f, rail: rail}
	h.g = newGate(func(r *http.Request) bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.by != "" || !match(r) {
			return false
		}
		h.by = r.Header.Get(replicaHeader)
		return true
	}, commit)
	if rail == "stripe" {
		f.base.stripe.hold(h.g)
	} else {
		f.base.nmi.hold(h.g)
	}
	return h
}

// wait returns the replica whose request the gate is holding.
func (h *held) wait() *world {
	h.f.t.Helper()
	select {
	case <-h.g.arrived:
	case <-time.After(30 * time.Second):
		h.f.any().dumpJobs()
		h.f.t.Fatal("the provider request never arrived")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.f.named(h.by)
}

// release lets held requests through and removes the gate.
func (h *held) release() {
	h.f.base.stripe.unhold()
	h.f.base.nmi.unhold()
	select {
	case <-h.g.release:
	default:
		close(h.g.release)
	}
}

func (f *fleet) unhold() {
	f.base.stripe.unhold()
	f.base.nmi.unhold()
}

func subUUID(id openrails.SubscriptionID) string { return strings.TrimPrefix(id.String(), "sub_") }

func (f *fleet) subscription(e *engineCase) *openrails.Subscription {
	f.t.Helper()
	sub, err := f.any().client[embedded].GetSubscription(f.t.Context(), e.sub)
	require.NoError(f.t, err)
	return sub
}

func (f *fleet) periodEnd(e *engineCase) time.Time {
	sub := f.subscription(e)
	require.NotNil(f.t, sub.CurrentPeriodEndsAt)
	return *sub.CurrentPeriodEndsAt
}

// providerCustomers are the provider-side customers (NMI vaults, Stripe
// customers) holding the local customer's cards.
func (f *fleet) providerCustomers(e *engineCase) []string {
	f.t.Helper()
	rows, err := f.base.pool.Query(f.t.Context(), f.q(`SELECT DISTINCT pm.rail_customer_ref FROM openrails.payment_methods pm
		JOIN openrails.subscriptions s ON s.merchant_id = pm.merchant_id AND s.customer_id = pm.customer_id
		WHERE s.id = $1 AND pm.rail_customer_ref <> ''`), subUUID(e.sub))
	require.NoError(f.t, err)
	refs, err := pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(f.t, err)
	require.NotEmpty(f.t, refs)
	return refs
}

// charges are the provider's approved charges for the case's customer.
func (f *fleet) charges(e *engineCase) []ledgerEntry {
	var out []ledgerEntry
	for _, ref := range f.providerCustomers(e) {
		if e.rail == "stripe" {
			out = append(out, f.base.stripe.ledger(ref)...)
		} else {
			out = append(out, f.base.nmi.ledger(ref)...)
		}
	}
	return out
}

// submissions counts charge requests that reached the provider for the
// case's customer, approved or not.
func (f *fleet) submissions(e *engineCase) int {
	n := 0
	for _, ref := range f.providerCustomers(e) {
		if e.rail == "stripe" {
			n += f.base.stripe.attempts(ref)
		} else {
			n += f.base.nmi.attemptsFor(ref)
		}
	}
	return n
}

func (fk *nmiFake) attemptsFor(vault string) int {
	fk.mu.Lock()
	defer fk.mu.Unlock()
	n := 0
	for _, a := range fk.attempts {
		if a.Get("customer_vault_id") == vault {
			n++
		}
	}
	return n
}

// approvedLocked counts approved sales on vault; the caller holds fk.mu.
func (fk *nmiFake) approvedLocked(vault string) int {
	n := 0
	for _, s := range fk.sales {
		if s.Vault == vault && s.Declined == "" {
			n++
		}
	}
	return n
}

// collection is one engine renewal operation as stored.
type collection struct {
	PeriodEnd string
	Attempt   string
	Status    string
	Evidence  string
}

func (f *fleet) collections(e *engineCase) []collection {
	f.t.Helper()
	rows, err := f.base.pool.Query(f.t.Context(), f.q(`SELECT coalesce(payload->>'previous_period_end', ''), coalesce(payload->>'attempt', ''), status, coalesce(result_evidence::text, '')
		FROM openrails.rail_intents WHERE subscription_id = $1 AND intent_type = 'subscription_collection' ORDER BY created_at, id`), subUUID(e.sub))
	require.NoError(f.t, err)
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[collection])
	require.NoError(f.t, err)
	return out
}

// requireExactlyOnce is the fleet's money invariant for one membership:
// exactly one provider charge and one local payment per paid period, every
// provider charge recorded locally (no orphan, no duplicate), one succeeded
// operation per renewed period, nothing unresolved, and no due-pass refusal.
// submissions < 0 skips the exact provider request count.
func (f *fleet) requireExactlyOnce(e *engineCase, renewals, submissions int) {
	t := f.t
	t.Helper()
	charges := f.charges(e)
	require.Len(t, charges, 1+renewals, "one provider charge per paid period")
	if submissions >= 0 {
		require.Equal(t, submissions, f.submissions(e), "provider charge requests")
	}
	page, err := f.any().client[embedded].ListPayments(t.Context(), openrails.PaymentFilter{CustomerID: e.c.id, PageOptions: openrails.PageOptions{Limit: 100}})
	require.NoError(t, err)
	paid := completed(page.Data)
	require.Len(t, paid, 1+renewals, "one local payment per paid period")
	provider := map[string]int64{}
	for _, c := range charges {
		provider[c.ID] = c.Amount
		if c.Charge != "" {
			provider[c.Charge] = c.Amount
		}
	}
	seen := map[string]bool{}
	for _, p := range paid {
		cents, ok := provider[p.TransactionID]
		require.True(t, ok, "local payment %s is a provider charge", p.TransactionID)
		require.Equal(t, cents*10_000, p.Amount)
		require.False(t, seen[p.TransactionID], "no duplicate local payment")
		seen[p.TransactionID] = true
	}
	succeeded := map[string]int{}
	for _, c := range f.collections(e) {
		require.NotContains(t, []string{"pending", "in_flight", "unknown_needs_verify", "failed_retryable"}, c.Status, "no unresolved renewal operation")
		if c.Status == "succeeded" {
			succeeded[c.PeriodEnd]++
		}
	}
	for period, n := range succeeded {
		require.Equal(t, 1, n, "one succeeded renewal for the period ending %s", period)
	}
	require.Len(t, succeeded, renewals, "one succeeded renewal per renewed period")
	for _, key := range f.base.openFindings("life.due_pass.refused") {
		require.NotEqual(t, subUUID(e.sub), key, "no due-pass refusal for a membership another replica settled")
	}
}

// post delivers a signed provider notice to r's webhook route without
// waiting for its work; safe off the test goroutine.
func (r *world) post(rail string, payload any) (int, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, err
	}
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	path, header, secret, scheme := "/v1/webhooks/stripe/"+stripeAcct, "Stripe-Signature", whsecStripe, "v1"
	if rail == "nmi" {
		path, header, secret, scheme = "/v1/webhooks/nmi/"+nmiAcct, "Webhook-Signature", whsecNMI, "s"
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "." + string(body)))
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, r.server.URL+mountPrefix+path, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(header, fmt.Sprintf("t=%s,%s=%s", ts, scheme, hex.EncodeToString(mac.Sum(nil))))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, res.Body)
	return res.StatusCode, nil
}

// retryNow is the member's "pay now" on r, off the test goroutine.
func (r *world) retryNow(c *customer, sub openrails.SubscriptionID, key string) (int, string, error) {
	req, err := http.NewRequest(http.MethodPost, r.server.URL+mountPrefix+"/v1/me/subscriptions/"+sub.String()+"/retry-now", strings.NewReader("{}"))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(raw), nil
}
