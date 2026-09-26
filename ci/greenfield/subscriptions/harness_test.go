//go:build greenfield && integration

package subscriptions_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jonboulle/clockwork"
	auth "github.com/open-rails/helpers/auth"
	riverkit "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	openrailshttp "github.com/open-rails/openrails/adapters/http"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/pkg/billingauth"
)

const (
	issuer      = "https://greenfield.test"
	mountPrefix = "/billing"
	stripeAcct  = "acct_greenfield"
	nmiAcct     = "greenfield-nmi"
	ccbillAcct  = "945280-0000"
	whsecStripe = "whsec_greenfield"
	whsecNMI    = "nmi_webhook_greenfield"
	monthHours  = 720
)

// Topology selects how merchant operations reach the engine: the embedded
// in-process Client or the same Client over HTTP (NewRemote). Customer
// actions always use the mounted /v1/me routes, as a browser does.
type topology string

const (
	embedded topology = "embedded"
	remote   topology = "remote"
)

// verifier is a neutral host identity provider: HS256 tokens, a staff subject
// with merchant authority and UUID subjects as native customers.
type verifier struct {
	secret []byte
	slug   string
}

type principal struct{ id auth.Identity }

func (p principal) Identity() auth.Identity { return p.id }
func (p principal) Can(context.Context, auth.Scope, string) (bool, error) {
	return p.id.Subject == "staff", nil
}

func (v *verifier) AuthenticateRequest(_ context.Context, r *http.Request) (auth.Principal, error) {
	token, err := jwt.Parse(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), func(*jwt.Token) (any, error) { return v.secret, nil },
		jwt.WithValidMethods([]string{"HS256"}), jwt.WithIssuer(issuer), jwt.WithAudience("billing"), jwt.WithExpirationRequired())
	if err != nil || !token.Valid {
		return nil, auth.ErrUnauthenticated
	}
	subject, err := token.Claims.GetSubject()
	if err != nil || subject == "" {
		return nil, auth.ErrUnauthenticated
	}
	return principal{auth.Identity{Kind: auth.KindUser, Issuer: issuer, Subject: subject}}, nil
}

func (v *verifier) token(t testing.TB, subject string) string {
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"sub": subject, "iss": issuer, "aud": "billing", "exp": time.Now().Add(time.Hour).Unix()}).SignedString(v.secret)
	require.NoError(t, err)
	return token
}

// world is one merchant in one fresh schema with the demo's embedded
// posture: sandbox credentials, full provider writes, host-owned River.
type world struct {
	t      *testing.T
	pool   *pgxpool.Pool
	dsn    string
	schema string
	slug   string
	clock  *clockwork.FakeClock
	stripe *stripeFake
	nmi    *nmiFake
	auth   *verifier
	cfg    func(*config.Config)
	// declare adjusts the merchant's provider declaration before start.
	declare func(map[string]embed.PSPConfig)
	// mount adjusts the mounted HTTP surface before start.
	mount func(*embed.HTTPConfig)
	// queries records named sqlc statements while counting.
	queries *queryLog

	rt     *embed.Runtime
	jobs   *river.Client[pgx.Tx]
	server *httptest.Server
	client map[topology]*openrails.Client
	psp    map[string]string
	// psps is the merchant's provider declaration, as its manifest states it.
	psps map[string]embed.PSPConfig

	// invariants are the money invariants checked when the world ends.
	invariants moneyInvariants

	// replica is set on one process of a multi-replica fleet
	// (replicas_harness_test.go): its own connections, River identity and
	// provider transports over the shared database and providers.
	replica *replicaEnv
}

func dsn(t testing.TB) string {
	if v := strings.TrimSpace(os.Getenv("OPENRAILS_GREENFIELD_DSN")); v != "" {
		return v
	}
	t.Fatal("OPENRAILS_GREENFIELD_DSN must point at a disposable PostgreSQL database")
	return ""
}

func newWorld(t *testing.T, configure ...func(*config.Config)) *world {
	t.Helper()
	w := prepareWorld(t, 12, configure...)
	w.start()
	return w
}

// prepareWorld creates the merchant's schema and shared test doubles without
// starting a runtime.
func prepareWorld(t *testing.T, maxConns int32, configure ...func(*config.Config)) *world {
	t.Helper()
	poolConfig, err := pgxpool.ParseConfig(dsn(t))
	require.NoError(t, err)
	poolConfig.MaxConns = maxConns
	queries := &queryLog{}
	poolConfig.ConnConfig.Tracer = queries
	pool, err := pgxpool.NewWithConfig(t.Context(), poolConfig)
	require.NoError(t, err)
	// Engine time starts in the past so every accepted operation is already
	// due on River's wall clock; tests advance it explicitly.
	clock := clockwork.NewFakeClockAt(time.Now().UTC().Add(-4 * 365 * 24 * time.Hour).Truncate(time.Second))
	w := &world{
		t: t, pool: pool, dsn: dsn(t), queries: queries,
		schema: "gf_subs_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16],
		slug:   "subs-" + uuid.NewString()[:8],
		clock:  clock,
		stripe: newStripeFake(),
		nmi:    newNMIFake(clock.Now),
		auth:   &verifier{secret: []byte("greenfield-subscriptions-" + uuid.NewString())},
	}
	w.auth.slug = w.slug
	if len(configure) > 0 {
		w.cfg = configure[0]
	}
	t.Cleanup(func() {
		w.stop()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, _ = pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{w.schema}.Sanitize()+" CASCADE")
		pool.Close()
	})
	t.Cleanup(w.checkMoneyInvariants)
	require.NoError(t, embed.ApplyMigrations(t.Context(), pool, embed.MigrationOptions{Schema: w.schema, River: embed.RiverFromHost(), RuntimePool: pool}))
	require.NoError(t, riverkit.ApplyMigrations(t.Context(), pool, w.schema))
	return w
}

// start constructs one process's runtime: engine, River fleet and HTTP.
func (w *world) start() {
	t := w.t
	t.Helper()
	identity, err := billingauth.NewIntegration(billingauth.IntegrationOptions{
		Verifier: w.auth,
		Customer: func(_ context.Context, p auth.Principal) (billingauth.CustomerIdentity, error) {
			if _, err := uuid.Parse(p.Identity().Subject); err == nil {
				return billingauth.CustomerIdentity{ID: p.Identity().Subject, CredentialClass: billingauth.CredentialClassUserSession}, nil
			}
			return billingauth.CustomerIdentity{}, nil
		},
		Authority: func(_ context.Context, q billingauth.Requirement) (billingauth.Authority, error) {
			if q.Scope != billingauth.MerchantScope || q.Target.MerchantSlug != w.slug {
				return billingauth.Authority{}, nil
			}
			return billingauth.Authority{Scope: auth.Scope{Authority: issuer, ID: "billing-staff"}, Permission: q.Permission}, nil
		},
	})
	require.NoError(t, err)
	pool, dbURL := w.pool, w.dsn
	stripe, nmi := http.RoundTripper(w.stripe), http.RoundTripper(w.nmi)
	riverConfig := &river.Config{
		Schema: w.schema, Queues: map[string]river.QueueConfig{embed.QueueBilling: {MaxWorkers: 4}},
		FetchCooldown: 5 * time.Millisecond, FetchPollInterval: 20 * time.Millisecond,
	}
	if w.replica != nil {
		pool, dbURL, stripe, nmi = w.replica.connect(w)
		w.replica.configureRiver(riverConfig)
	}
	cfg := &config.Config{
		TestMode:            config.CredentialPostureSandbox,
		ProviderWriteMode:   config.ProviderWriteModeFull,
		AllowCatalogUpdates: true,
		DB:                  &config.DBConfig{URL: dbURL, Schema: w.schema},
		// The test server's loopback peer is the site's reverse proxy.
		TrustedProxies: []string{"127.0.0.1/32"},
		ReturnOrigins:  []string{"https://greenfield.test"},
	}
	if w.cfg != nil {
		w.cfg(cfg)
	}
	psps := map[string]embed.PSPConfig{
		"stripe": {"stripe": {AccountID: stripeAcct, Secrets: map[string]string{"secret_key": "sk_test_greenfield", "webhook_signing_secret": whsecStripe}}},
		"nmi":    {"nmi": {AccountID: nmiAcct, Secrets: map[string]string{"security_key": "greenfield-nmi-key", "webhook_signing_secret": whsecNMI}, Settings: map[string]any{"tokenization_key": "greenfield-tokenization"}}},
		"ccbill": {"ccbill": {AccountID: ccbillAcct, Secrets: map[string]string{"salt": "greenfield-ccbill-salt"}}},
	}
	if w.declare != nil {
		w.declare(psps)
	}
	w.psps = psps
	httpConfig := &embed.HTTPConfig{MerchantAdmin: true, MerchantAPI: true, Catalog: true, CustomerRoutes: []embed.CustomerRoutesConfig{{Merchant: w.slug, Scope: embed.CustomerBillingManagement}}}
	if w.mount != nil {
		w.mount(httpConfig)
	}
	rt, err := embed.New(t.Context(), embed.Options{
		Auth:            identity,
		HTTP:            httpConfig,
		Merchant:        &embed.MerchantDeclaration{Slug: w.slug, Config: embed.MerchantConfig{DisplayName: w.slug, PSPs: psps}},
		Config:          cfg,
		PGXPool:         pool,
		River:           embed.RiverFromHost(),
		StripeTransport: stripe,
		NMITransport:    nmi,
		Clock:           w.clock,
	})
	require.NoError(t, err)
	w.rt = rt
	jobs, err := riverkit.New(t.Context(), pool, riverConfig, rt.RiverJobs())
	require.NoError(t, err)
	require.NoError(t, jobs.Start(context.WithoutCancel(t.Context())))
	w.jobs = jobs
	bundle, err := openrailshttp.Routes(rt)
	require.NoError(t, err)
	mux := http.NewServeMux()
	require.NoError(t, bundle.Mount(mux, mountPrefix))
	w.server = httptest.NewServer(mux)
	local, err := rt.Client()
	require.NoError(t, err)
	staff := w.auth.token(t, "staff")
	over, err := openrails.NewRemote(w.server.URL+mountPrefix, openrails.WithDefaultMerchant(w.slug),
		openrails.WithTokenProvider(func(context.Context) (string, error) { return staff, nil }))
	require.NoError(t, err)
	w.client = map[topology]*openrails.Client{embedded: local, remote: over}
	require.Eventually(t, func() bool { return rt.Ready(t.Context()) == nil }, 10*time.Second, 50*time.Millisecond, "runtime readiness")
	config, err := local.GetCheckoutConfig(t.Context())
	require.NoError(t, err)
	w.psp = map[string]string{}
	for _, psp := range config.PSPs {
		w.psp[psp.Rail] = psp.PSPID
	}
	for _, rail := range []string{"stripe", "nmi"} {
		if _, declared := psps[rail]; declared {
			require.NotEmpty(t, w.psp[rail], "%+v stripe odd=%v nmi odd=%v", config, w.stripe.unexpected(), w.nmi.Unexpected())
		}
	}
}

// stop ends this process. River work in flight is cancelled, as in a crash.
func (w *world) stop() {
	if w.jobs != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = w.jobs.StopAndCancel(ctx)
		cancel()
		w.jobs = nil
	}
	if w.server != nil {
		w.server.Close()
		w.server = nil
	}
	if w.rt != nil {
		_ = w.rt.Close(context.Background())
		w.rt = nil
	}
	if w.replica != nil {
		w.replica.disconnect()
	}
}

func (w *world) restart() { w.stop(); w.start() }

// kill ends the process as SIGKILL does: nothing it was doing gets recorded.
// The running jobs and in-flight operations are captured at the instant of
// death, the process stops, and those rows are put back as the dead process
// left them, aged past OpenRails' five-minute silence threshold, before restart.
func (w *world) kill() {
	w.t.Helper()
	ctx := w.t.Context()
	schema := pgx.Identifier{w.schema}.Sanitize()
	type intentRow struct {
		id      string
		status  string
		claimed *time.Time
	}
	var jobIDs []int64
	rows, err := w.pool.Query(ctx, `SELECT id FROM `+schema+`.river_job WHERE state = 'running' AND kind LIKE 'openrails.%'`)
	require.NoError(w.t, err)
	for rows.Next() {
		var id int64
		require.NoError(w.t, rows.Scan(&id))
		jobIDs = append(jobIDs, id)
	}
	rows.Close()
	var intents []intentRow
	rows, err = w.pool.Query(ctx, `SELECT id::text, status, claimed_until FROM `+schema+`.rail_intents WHERE status = 'in_flight'`)
	require.NoError(w.t, err)
	for rows.Next() {
		var r intentRow
		require.NoError(w.t, rows.Scan(&r.id, &r.status, &r.claimed))
		intents = append(intents, r)
	}
	rows.Close()
	require.NotEmpty(w.t, jobIDs, "a job was running at the kill")
	w.stop()
	_, err = w.pool.Exec(ctx, `UPDATE `+schema+`.river_job SET state = 'running', finalized_at = NULL, attempted_at = now() - interval '10 minutes' WHERE id = ANY($1)`, jobIDs)
	require.NoError(w.t, err)
	for _, r := range intents {
		_, err = w.pool.Exec(ctx, `UPDATE `+schema+`.rail_intents SET status = $2, claimed_until = $3 WHERE id = $1::uuid`, r.id, r.status, r.claimed)
		require.NoError(w.t, err)
	}
}

// runRenewals runs the engine's scheduled due pass once, then waits for the
// fleet to finish every operation it accepted.
func (w *world) runRenewals() {
	w.t.Helper()
	res, err := w.jobs.Insert(w.t.Context(), dunningPass{}, &river.InsertOpts{Queue: embed.QueueBilling})
	require.NoError(w.t, err)
	w.waitJob(res.Job.ID)
	if os.Getenv("GF_DEBUG") != "" {
		page, _ := w.jobs.JobList(w.t.Context(), river.NewJobListParams().First(100))
		for _, j := range page.Jobs {
			w.t.Logf("job %d %s %s sched=%s attempt=%d errs=%v", j.ID, j.Kind, j.State, j.ScheduledAt.Format(time.RFC3339), j.Attempt, j.Errors)
		}
	}
}

var workKinds = []string{"openrails.provider_operation", "openrails.subscription_converge", "openrails.subscription_cancel", "openrails.subscription_resume"}

// settle waits until no operation or convergence work is runnable. Jobs the
// engine scheduled for a moment already past are promoted at once (River's
// leader otherwise promotes them on its own multi-second cadence); a job
// snoozed into the wall-clock future stays asleep until wake.
func (w *world) settle() {
	w.t.Helper()
	if !assert.Eventually(w.t, func() bool {
		page, err := w.jobs.JobList(w.t.Context(), river.NewJobListParams().Kinds(workKinds...).States(rivertype.JobStateScheduled).First(100))
		if err != nil {
			return false
		}
		for _, job := range page.Jobs {
			if job.Kind == "openrails.subscription_converge" || !job.ScheduledAt.After(time.Now()) {
				_, _ = w.jobs.JobRetry(w.t.Context(), job.ID)
			}
		}
		page, err = w.jobs.JobList(w.t.Context(), river.NewJobListParams().Kinds(workKinds...).
			States(rivertype.JobStateAvailable, rivertype.JobStateRunning, rivertype.JobStatePending).First(50))
		if err != nil || len(page.Jobs) > 0 {
			return false
		}
		page, err = w.jobs.JobList(w.t.Context(), river.NewJobListParams().Kinds(workKinds...).States(rivertype.JobStateScheduled).First(100))
		if err != nil {
			return false
		}
		for _, job := range page.Jobs {
			if job.Kind == "openrails.subscription_converge" || !job.ScheduledAt.After(time.Now()) {
				return false
			}
		}
		return true
	}, 30*time.Second, 25*time.Millisecond, "operations settle") {
		w.dumpJobs()
		w.t.FailNow()
	}
}

func (w *world) dumpJobs() {
	page, _ := w.jobs.JobList(context.Background(), river.NewJobListParams().First(100))
	for _, j := range page.Jobs {
		w.t.Logf("job %d %s %s sched=%s attempted=%v attempt=%d args=%s errs=%v", j.ID, j.Kind, j.State, j.ScheduledAt.Format(time.RFC3339), j.AttemptedAt, j.Attempt, j.EncodedArgs, j.Errors)
	}
	rows, err := w.pool.Query(context.Background(), `SELECT intent_type, status, coalesce(last_failure_reason,''), coalesce(result_evidence::text,''), claimed_until FROM `+pgx.Identifier{w.schema}.Sanitize()+`.rail_intents`)
	if err == nil {
		for rows.Next() {
			var typ, status, reason, evidence string
			var claimed *time.Time
			_ = rows.Scan(&typ, &status, &reason, &evidence, &claimed)
			w.t.Logf("intent %s %s claimed=%v reason=%q evidence=%s", typ, status, claimed, reason, evidence)
		}
		rows.Close()
	}
}

// waitJob waits for one inserted engine job, then for the work it caused.
func (w *world) waitJob(id int64) {
	w.t.Helper()
	require.Eventually(w.t, func() bool {
		job, err := w.jobs.JobGet(w.t.Context(), id)
		return err == nil && job.State == rivertype.JobStateCompleted
	}, 20*time.Second, 20*time.Millisecond, "engine job")
	w.settle()
}

// settleQuiet promotes due scheduled work once without asserting, for use off
// the test goroutine while a request is held.
func (w *world) settleQuiet() {
	jobs := w.jobs
	if jobs == nil {
		return
	}
	page, err := jobs.JobList(context.Background(), river.NewJobListParams().Kinds(workKinds...).States(rivertype.JobStateScheduled).First(100))
	if err != nil {
		return
	}
	for _, job := range page.Jobs {
		if !job.ScheduledAt.After(time.Now()) {
			_, _ = jobs.JobRetry(context.Background(), job.ID)
		}
	}
}

// wake makes every snoozed operation runnable now, as an operator retry
// does, after the engine clock passed its next attempt.
func (w *world) wake() {
	w.t.Helper()
	page, err := w.jobs.JobList(w.t.Context(), river.NewJobListParams().Kinds("openrails.provider_operation").States(rivertype.JobStateScheduled, rivertype.JobStateRetryable).First(100))
	require.NoError(w.t, err)
	for _, job := range page.Jobs {
		_, err := w.jobs.JobRetry(w.t.Context(), job.ID)
		require.NoError(w.t, err)
	}
	w.settle()
}

// armDestructive is the documented operator arming (docs/operations.md, "The
// destructive-action kill switch"): the instance switch and this merchant's
// policy row. A fresh deployment ships with both off.
func (w *world) armDestructive() {
	w.t.Helper()
	ctx := w.t.Context()
	q := func(sql string) string {
		return strings.ReplaceAll(sql, "openrails.", pgx.Identifier{w.schema}.Sanitize()+".")
	}
	_, err := w.pool.Exec(ctx, q(`UPDATE openrails.destructive_action_switch SET enabled = true, updated_by = 'greenfield'`))
	require.NoError(w.t, err)
	_, err = w.pool.Exec(ctx, q(`INSERT INTO openrails.merchant_destructive_policy (merchant_id, destructive_actions_enabled, enforce_armed_at, updated_by, reason)
		SELECT id, true, now(), 'greenfield', 'reviewed' FROM openrails.merchants WHERE slug = $1
		ON CONFLICT (merchant_id) DO UPDATE SET enforce_armed_at = now(), destructive_actions_enabled = true`), w.slug)
	require.NoError(w.t, err)
}

// until polls cond, stepping engine time ten minutes and waking due
// operations between polls, as an idle fleet would over time.
func (w *world) until(cond func() bool, msg string) {
	w.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			page, _ := w.jobs.JobList(w.t.Context(), river.NewJobListParams().Kinds(workKinds...).First(100))
			for _, j := range page.Jobs {
				w.t.Logf("job %d %s %s sched=%s attempt=%d args=%s errs=%v", j.ID, j.Kind, j.State, j.ScheduledAt.Format(time.RFC3339), j.Attempt, j.EncodedArgs, j.Errors)
			}
			w.t.Fatalf("timed out: %s", msg)
		}
		w.advance(10 * time.Minute)
		w.wake()
		time.Sleep(50 * time.Millisecond)
	}
}

type dunningPass struct{}

type rescuePass struct{}

func (rescuePass) Kind() string { return "openrails.job_rescue" }

// rescue runs the minute's orphaned-job rescue pass now. (A process that
// restarts within the minute its predecessor ran the pass waits for the next
// minute's pass.)
func (w *world) rescue() {
	w.t.Helper()
	res, err := w.jobs.Insert(w.t.Context(), rescuePass{}, &river.InsertOpts{Queue: embed.QueueBilling})
	require.NoError(w.t, err)
	w.waitJob(res.Job.ID)
}

func (dunningPass) Kind() string { return "openrails.dunning" }

func (w *world) advance(d time.Duration) { w.clock.Advance(d) }

// staff calls a merchant route with the staff credential and returns the
// status and raw body.
func (w *world) staff(method, path string) (int, string) {
	w.t.Helper()
	req, err := http.NewRequestWithContext(w.t.Context(), method, w.server.URL+mountPrefix+path, nil)
	require.NoError(w.t, err)
	req.Header.Set("Authorization", "Bearer "+w.auth.token(w.t, "staff"))
	req.Header.Set("X-OpenRails-Merchant-Slug", w.slug)
	res, err := http.DefaultClient.Do(req)
	require.NoError(w.t, err)
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	require.NoError(w.t, err)
	return res.StatusCode, string(raw)
}

// customer is one native customer acting through the mounted /v1/me routes.
type customer struct {
	w     *world
	id    string
	token string
}

func (w *world) newCustomer() *customer {
	id := uuid.NewString()
	_, err := w.client[embedded].EnsureCustomer(w.t.Context(), id)
	require.NoError(w.t, err)
	return &customer{w: w, id: id, token: w.auth.token(w.t, id)}
}

func (c *customer) call(method, path, key string, body any) (int, map[string]any) {
	c.w.t.Helper()
	var data io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		require.NoError(c.w.t, err)
		data = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(c.w.t.Context(), method, c.w.server.URL+mountPrefix+"/v1/me"+path, data)
	require.NoError(c.w.t, err)
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	res, err := http.DefaultClient.Do(req)
	require.NoError(c.w.t, err)
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	require.NoError(c.w.t, err)
	out := map[string]any{}
	if len(raw) > 0 {
		require.NoError(c.w.t, json.Unmarshal(raw, &out), "%s %s: %s", method, path, raw)
	}
	return res.StatusCode, out
}

func (c *customer) must(method, path, key string, body any) map[string]any {
	c.w.t.Helper()
	status, out := c.call(method, path, key, body)
	require.True(c.w.t, status >= 200 && status < 300, "%s %s: %d %v", method, path, status, out)
	return out
}

func unwrap(v map[string]any) map[string]any {
	if data, ok := v["data"].(map[string]any); ok {
		return data
	}
	return v
}

// saveCard vaults a card on rail and returns its payment method id. The
// browser tokenization step is the fake provider's.
func (c *customer) saveCard(rail string, card card) string {
	c.w.t.Helper()
	switch rail {
	case "stripe":
		setup := c.must(http.MethodPost, "/payment-methods/stripe-setup", "setup-"+uuid.NewString(), map[string]any{"psp_id": c.w.psp["stripe"], "consent": true})
		c.w.stripe.completeSetup(strings.TrimSuffix(setup["client_secret"].(string), "_secret_gf"), card)
		confirmed := unwrap(c.must(http.MethodPost, fmt.Sprintf("/payment-methods/stripe-setup/%s/confirm", setup["id"]), "", nil))
		return confirmed["payment_method_id"].(string)
	case "nmi":
		token := c.w.nmi.Tokenize(card)
		saved := unwrap(c.must(http.MethodPost, "/payment-methods", "", map[string]any{"provider": "nmi", "psp_id": c.w.psp["nmi"], "payment_token": token, "name_on_card": "Greenfield Payer"}))
		return saved["id"].(string)
	}
	c.w.t.Fatalf("unknown rail %s", rail)
	return ""
}

// subscribe enrolls an engine-owned membership the way the demo does: the
// application creates the catalog-authoritative session through the merchant
// Client, then the signed-in payer reads the quote and confirms it.
func (c *customer) subscribe(tp topology, rail, priceID, entitlement, method string) openrails.SubscriptionID {
	c.w.t.Helper()
	id := c.enrollOnce(tp, rail, priceID, entitlement, method)
	subs, err := c.w.client[tp].ListSubscriptions(c.w.t.Context(), openrails.SubscriptionFilter{CustomerID: c.id})
	require.NoError(c.w.t, err)
	require.Len(c.w.t, subs.Data, 1)
	require.Equal(c.w.t, id, subs.Data[0].ID)
	return id
}

// subscribeAgain enrolls a returning customer, whose earlier memberships stay.
func (c *customer) subscribeAgain(tp topology, rail, priceID, entitlement, method string) openrails.SubscriptionID {
	c.w.t.Helper()
	return c.enrollOnce(tp, rail, priceID, entitlement, method)
}

func (c *customer) enrollOnce(tp topology, rail, priceID, entitlement, method string) openrails.SubscriptionID {
	c.w.t.Helper()
	session, err := c.w.client[tp].CreateCheckoutSession(c.w.t.Context(), openrails.CreateCheckoutSessionRequest{
		OfferKind: openrails.OfferRecurring, Customer: openrails.CheckoutCustomerIdentity{ID: c.id}, Entitlement: entitlement, PriceID: priceID,
		IdempotencyKey: "enroll-" + uuid.NewString(), PaymentOptions: openrails.CheckoutPaymentOptions{PSPID: c.w.psp[rail], Rail: rail, PaymentMethodID: method},
		SuccessURL: "https://greenfield.test/return", CancelURL: "https://greenfield.test/return?canceled=1",
	})
	require.NoError(c.w.t, err)
	quote := unwrap(c.must(http.MethodGet, "/checkout/"+session.ID, "", nil))
	require.NotNil(c.w.t, quote["membership_quote"], "engine enrollment is quoted: %v", quote)
	done := unwrap(c.must(http.MethodPost, fmt.Sprintf("/checkout/%s/confirm", session.ID), "", map[string]any{"payment": map[string]string{"rail": rail}}))
	require.Equal(c.w.t, "succeeded", done["status"], "%v", done)
	c.w.settle()
	sub, ok := done["subscription_id"].(string)
	require.True(c.w.t, ok, "confirmation names the membership: %v", done)
	var id openrails.SubscriptionID
	require.NoError(c.w.t, json.Unmarshal([]byte(`"`+sub+`"`), &id))
	return id
}

func (c *customer) entitled(entitlement string) bool {
	c.w.t.Helper()
	got, err := c.w.client[embedded].CheckEntitlements(c.w.t.Context(), c.id, []string{entitlement}, c.w.clock.Now())
	require.NoError(c.w.t, err)
	return got[entitlement]
}

// membership creates a monthly auto-renew product and price.
func (w *world) membership(entitlement string, unitAmount int64) *openrails.Price {
	w.t.Helper()
	return w.membershipEvery(entitlement, unitAmount, monthHours)
}

// membershipEvery is an engine membership renewing every hours.
func (w *world) membershipEvery(entitlement string, unitAmount int64, hours int) *openrails.Price {
	w.t.Helper()
	client := w.client[embedded]
	product, err := client.Products.Create(w.t.Context(), &openrails.ProductCreateParams{Key: "member-" + uuid.NewString()[:8], DisplayName: "Membership", EntitlementsSpec: map[string]*int{entitlement: nil}})
	require.NoError(w.t, err)
	price, err := client.Prices.Create(w.t.Context(), &openrails.PriceCreateParams{ProductID: product.ID, Key: product.Key + "-usd", UnitAmount: unitAmount, Currency: "USD", AutoRenew: true, AccessDurationHours: &hours})
	require.NoError(w.t, err)
	return price
}

func (w *world) subscription(tp topology, id openrails.SubscriptionID) *openrails.Subscription {
	w.t.Helper()
	sub, err := w.client[tp].GetSubscription(w.t.Context(), id)
	require.NoError(w.t, err)
	return sub
}

func (w *world) payments(tp topology, customerID string) []openrails.Payment {
	w.t.Helper()
	page, err := w.client[tp].ListPayments(w.t.Context(), openrails.PaymentFilter{CustomerID: customerID, PageOptions: openrails.PageOptions{Limit: 100}})
	require.NoError(w.t, err)
	return page.Data
}

func completed(payments []openrails.Payment) []openrails.Payment {
	var out []openrails.Payment
	for _, p := range payments {
		if p.Status == "succeeded" || p.Status == "completed" {
			out = append(out, p)
		}
	}
	return out
}

// loseSubmissions makes the rail's next n charge requests fail in transit
// without reaching the provider.
func (w *world) loseSubmissions(rail string, n int) {
	if rail == "stripe" {
		w.stripe.loseSubmissions(n)
	} else {
		w.nmi.LoseSales(n)
	}
}

// lostSubmissions is how many charge requests were lost in transit.
func (w *world) lostSubmissions(rail string) int {
	if rail == "stripe" {
		w.stripe.mu.Lock()
		defer w.stripe.mu.Unlock()
		return w.stripe.lost
	}
	return w.nmi.Lost()
}

// readUnavailable makes the rail's authoritative payment read fail.
func (w *world) readUnavailable(rail string, down bool) {
	if rail == "stripe" {
		w.stripe.listUnavailable(down)
	} else {
		w.nmi.QueryUnavailable(down)
	}
}

// openFindings lists the subject keys of standing findings of one type.
func (w *world) openFindings(findingType string) []string {
	w.t.Helper()
	rows, err := w.pool.Query(w.t.Context(), `SELECT subject_key FROM `+pgx.Identifier{w.schema}.Sanitize()+`.reconciliation_findings
		WHERE finding_type = $1 AND status IN ('reconcile_required', 'requires_review')`, findingType)
	require.NoError(w.t, err)
	keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(w.t, err)
	return keys
}
