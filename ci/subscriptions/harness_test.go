//go:build e2e && integration

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
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jonboulle/clockwork"
	riverkit "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	openrailshttp "github.com/open-rails/openrails/adapters/http"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/engine"
	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/open-rails/openrails/internal/sqlschema"
)

const (
	issuer      = "https://e2e.test"
	mountPrefix = "/billing"
	stripeAcct  = "acct_e2e"
	nmiAcct     = "e2e-nmi"
	ccbillAcct  = "999999-0000"
	whsecStripe = "whsec_e2e"
	whsecNMI    = "nmi_webhook_e2e"
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

// The harness host's own permissions, for the staff route groups it mounts.
type perm string

func (p perm) String() string { return string(p) }

const (
	staffReads   perm = "e2e:billing:read"
	staffWrites  perm = "e2e:billing:write"
	staffCatalog perm = "e2e:catalog:write"
	staffConfig  perm = "e2e:billing:admin"
	staffMetrics perm = "e2e:billing:metrics"
)

var permissions = openrails.Permissions{AdminRead: staffReads, AdminUpdate: staffWrites, Catalog: staffCatalog, MerchantConfig: staffConfig, Metrics: staffMetrics}

// routeGroups turns on every route group the harness mounts.
var routeGroups = openrails.RouteGroups{Admin: true, Catalog: true, MerchantConfig: true, Metrics: true, Programmatic: true}

// verifier is a neutral host's Auth: HS256 tokens. "staff" is a user holding
// every permission, "support" the staff reads and writes but not the
// merchant's configuration, "reader" the reads, "host" the host's own backend
// (an application with an API key) all of them; UUID subjects are native
// customers. Like AuthKit, its checks are live: a session revoked after its
// token was minted is refused as a revoked credential. A sign-in (auth_time)
// older than 15 minutes needs a step-up for operations that move money.
type verifier struct {
	secret  []byte
	revoked sync.Map // session id -> struct{}
}

var _ openrails.Auth = (*verifier)(nil)

// hostApp is the host backend's application subject.
const hostApp = "host"

type verifiedKey struct{}

// verified is one token as the verifier read it.
type verified struct {
	id       openrails.Identity
	signedIn time.Time
}

// grant is a token's identity: its subject's kind, credential and invoker
// default to a user acting itself in a session.
type grant struct {
	subject, kind, credential, sid string
	invokerIssuer, invoker         string
	signedIn                       time.Time
}

func (v *verifier) verify(r *http.Request) (verified, error) {
	if got, ok := r.Context().Value(verifiedKey{}).(verified); ok {
		return got, nil
	}
	token, err := jwt.Parse(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), func(*jwt.Token) (any, error) { return v.secret, nil },
		jwt.WithValidMethods([]string{"HS256"}), jwt.WithIssuer(issuer), jwt.WithAudience("billing"), jwt.WithExpirationRequired())
	if err != nil || !token.Valid {
		return verified{}, billingauth.Refusal(billing.CodeAuthenticationRequired)
	}
	claims := token.Claims.(jwt.MapClaims)
	subject, _ := claims["sub"].(string)
	if subject == "" {
		return verified{}, billingauth.Refusal(billing.CodeAuthenticationRequired)
	}
	text := func(name, fallback string) string {
		if value, _ := claims[name].(string); value != "" {
			return value
		}
		return fallback
	}
	sid := text("sid", "")
	if _, gone := v.revoked.Load(sid); gone && sid != "" {
		return verified{}, billingauth.Refusal(billing.CodeCredentialRevoked)
	}
	authTime, _ := claims["auth_time"].(float64)
	id := openrails.Identity{
		Issuer: issuer, Subject: subject, SubjectKind: openrails.SubjectKind(text("kind", string(openrails.SubjectUser))),
		Invoker:    openrails.Invoker{Issuer: text("inv_iss", issuer), ID: text("inv", subject)},
		Credential: openrails.Credential{Kind: openrails.CredentialKind(text("cred", string(openrails.CredentialSession))), ID: text("sid", text("jti", ""))},
	}
	return verified{id: id, signedIn: time.Unix(int64(authTime), 0)}, nil
}

// gate is middleware that verifies the token, then admits it by check.
func (v *verifier) gate(check func(verified) error) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got, err := v.verify(r)
			if err == nil {
				err = check(got)
			}
			if err != nil {
				billingauth.WriteRefusal(w, r, billingauth.AsRefusal(err))
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), verifiedKey{}, got)))
		})
	}
}

func (v *verifier) Required() func(http.Handler) http.Handler {
	return v.gate(func(verified) error { return nil })
}

func (v *verifier) RequirePermission(permission string) func(http.Handler) http.Handler {
	return v.gate(func(got verified) error {
		switch got.id.Subject {
		case "staff", hostApp:
			return nil
		case "support":
			if permission == staffReads.String() || permission == staffWrites.String() {
				return nil
			}
		case "reader":
			if permission == staffReads.String() {
				return nil
			}
		}
		return billingauth.Refusal(billing.CodePermissionRequired)
	})
}

func (v *verifier) Sensitive() func(http.Handler) http.Handler {
	return v.gate(func(got verified) error {
		if time.Since(got.signedIn) > 15*time.Minute {
			refusal := billingauth.Refusal(billing.CodeStepUpRequired)
			refusal.Metadata = map[string]any{"step_up_methods": []string{"password"}}
			return refusal
		}
		return nil
	})
}

func (v *verifier) Identity(ctx context.Context) (openrails.Identity, bool) {
	got, ok := ctx.Value(verifiedKey{}).(verified)
	return got.id, ok
}

func (v *verifier) token(t testing.TB, subject string) string {
	return v.sessionToken(t, subject, "")
}

// staleToken is subject's live session signed in an hour ago, as a stolen
// token's is.
func (v *verifier) staleToken(t testing.TB, subject string) string {
	return v.issue(t, grant{subject: subject, signedIn: time.Now().Add(-time.Hour)})
}

func (v *verifier) sessionToken(t testing.TB, subject, sid string) string {
	return v.issue(t, grant{subject: subject, sid: sid})
}

// apiKeyToken is subject's own API key, automating their account.
func (v *verifier) apiKeyToken(t testing.TB, subject string) string {
	return v.issue(t, grant{subject: subject, credential: string(openrails.CredentialAPIKey)})
}

// hostToken is the host backend's API key.
func (v *verifier) hostToken(t testing.TB) string {
	return v.issue(t, grant{subject: hostApp, kind: string(openrails.SubjectApplication), credential: string(openrails.CredentialAPIKey)})
}

func (v *verifier) issue(t testing.TB, g grant) string {
	if g.signedIn.IsZero() {
		g.signedIn = time.Now()
	}
	claims := jwt.MapClaims{"sub": g.subject, "iss": issuer, "aud": "billing", "exp": time.Now().Add(time.Hour).Unix(), "auth_time": g.signedIn.Unix(), "jti": uuid.NewString()}
	for name, value := range map[string]string{"sid": g.sid, "kind": g.kind, "cred": g.credential, "inv_iss": g.invokerIssuer, "inv": g.invoker} {
		if value != "" {
			claims[name] = value
		}
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(v.secret)
	require.NoError(t, err)
	return token
}

// world is one merchant in one fresh schema with the demo's embedded
// posture: sandbox credentials, full provider writes, River on the host's
// fleet.
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
	declare func(map[string]openrails.PSPConfig)
	// custodians are the merchant's declared card custodians.
	custodians map[string]openrails.CustodianConfig
	// mount adjusts the mounted HTTP surface before start.
	mount func(*openrails.Routes)
	// deps adjusts the host hooks before start.
	deps func(*openrails.Deps)
	// booted sees the engine the moment New returns, before anything else runs.
	booted func(*openrails.Client)
	// queries records named sqlc statements while counting.
	queries *queryLog

	rt     *openrails.Client
	jobs   *river.Client[pgx.Tx]
	server *httptest.Server
	client map[topology]*openrails.Client
	psp    map[string]billing.PSPID
	// psps is the merchant's provider declaration, as its manifest states it.
	psps map[string]openrails.PSPConfig

	// invariants are the money invariants checked when the world ends.
	invariants moneyInvariants

	// replica is set on one process of a multi-replica fleet
	// (replicas_harness_test.go): its own connections, River identity and
	// provider transports over the shared database and providers.
	replica *replicaEnv
}

func dsn(t testing.TB) string {
	if v := strings.TrimSpace(os.Getenv("OPENRAILS_E2E_DSN")); v != "" {
		return v
	}
	t.Fatal("OPENRAILS_E2E_DSN must point at a disposable PostgreSQL database")
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
	return prepareWorldAtDSN(t, maxConns, dsn(t), configure...)
}

func prepareWorldAtDSN(t *testing.T, maxConns int32, databaseURL string, configure ...func(*config.Config)) *world {
	t.Helper()
	poolConfig, err := pgxpool.ParseConfig(databaseURL)
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
		t: t, pool: pool, dsn: databaseURL, queries: queries,
		schema: "gf_subs_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16],
		slug:   "subs-" + uuid.NewString()[:8],
		clock:  clock,
		stripe: newStripeFake(),
		nmi:    newNMIFake(clock.Now),
		auth:   &verifier{secret: []byte("e2e-subscriptions-" + uuid.NewString())},
	}
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
	require.NoError(t, engine.Migrate(t.Context(), pool, openrails.Config{Database: openrails.DatabaseConfig{Schema: w.schema, RiverSchema: w.schema}}))
	return w
}

// start constructs one process's runtime: engine, River fleet and HTTP.
func (w *world) start() {
	t := w.t
	t.Helper()
	pool, dbURL := w.pool, w.dsn
	stripe, nmi := http.RoundTripper(w.stripe), http.RoundTripper(w.nmi)
	riverConfig := &river.Config{
		Schema: w.schema, Queues: map[string]river.QueueConfig{openrails.QueueBilling: {MaxWorkers: 4}},
		FetchCooldown: 5 * time.Millisecond, FetchPollInterval: 20 * time.Millisecond,
	}
	if w.replica != nil {
		pool, dbURL, stripe, nmi = w.replica.connect(w)
		w.replica.configureRiver(riverConfig)
	}
	cfg := &openrails.Config{
		Database:          openrails.DatabaseConfig{Schema: w.schema, RiverSchema: w.schema},
		TestMode:          openrails.Sandbox,
		ProviderWriteMode: openrails.ProviderWritesFull,
		DB:                &openrails.DBConfig{URL: dbURL},
		// The test server's loopback peer is the site's reverse proxy.
		TrustedProxies: []string{"127.0.0.1/32"},
		ReturnOrigins:  []string{"https://e2e.test"},
	}
	if w.cfg != nil {
		w.cfg(cfg)
	}
	psps := map[string]openrails.PSPConfig{
		"stripe": openrails.StripePSP{AccountID: stripeAcct, SecretKey: "sk_test_e2e", WebhookSigningSecret: whsecStripe, PublishableKey: "pk_test_e2e"}.PSPConfig(),
		"nmi":    openrails.NMIPSP{AccountID: nmiAcct, SecurityKey: "e2e-nmi-key", WebhookSigningSecret: whsecNMI, TokenizationKey: "e2e-tokenization"}.PSPConfig(),
		"ccbill": openrails.CCBillPSP{AccountID: ccbillAcct, Salt: "e2e-ccbill-salt"}.PSPConfig(),
	}
	if w.declare != nil {
		w.declare(psps)
	}
	w.psps = psps
	routes := openrails.Routes{Auth: w.auth, Prefix: mountPrefix, RouteGroups: routeGroups, Permissions: permissions}
	if w.mount != nil {
		w.mount(&routes)
	}
	cfg.Merchant = openrails.MerchantDeclaration{Slug: w.slug, DisplayName: w.slug, PSPs: psps, Custodians: w.custodians}
	deps := openrails.Deps{Postgres: pool, StripeTransport: stripe, NMITransport: nmi, Clock: w.clock}
	if w.deps != nil {
		w.deps(&deps)
	}
	rt, err := openrails.New(t.Context(), *cfg, deps)
	require.NoError(t, err)
	w.rt = rt
	if w.booted != nil {
		w.booted(rt)
	}
	jobs, err := riverkit.New(t.Context(), pool, riverConfig, rt.RiverJobs())
	require.NoError(t, err)
	if w.replica != nil && !w.replica.f.scheduled {
		jobs.PeriodicJobs().Clear()
	}
	require.NoError(t, jobs.Start(context.WithoutCancel(t.Context())))
	w.jobs = jobs
	mux := http.NewServeMux()
	require.NoError(t, openrailshttp.Mount(mux, rt, routes))
	w.server = httptest.NewServer(mux)
	local := rt
	host := w.auth.hostToken(t)
	over, err := openrails.NewRemote(w.server.URL+mountPrefix, openrails.WithDefaultMerchant(w.slug),
		openrails.WithTokenProvider(func(context.Context) (string, error) { return host, nil }))
	require.NoError(t, err)
	w.client = map[topology]*openrails.Client{embedded: local, remote: over}
	require.Eventually(t, func() bool { return rt.Ready(t.Context()) == nil }, 10*time.Second, 50*time.Millisecond, "runtime readiness")
	config := publicConfig(t, local)
	w.psp = map[string]billing.PSPID{}
	for _, psp := range config.Payment.PSPs {
		w.psp[psp.Rail] = psp.PSPID
	}
	for _, rail := range []string{"stripe", "nmi"} {
		if _, declared := psps[rail]; declared {
			require.False(t, w.psp[rail].IsZero(), "%+v stripe odd=%v nmi odd=%v", config, w.stripe.Unexpected(), w.nmi.Unexpected())
		}
	}
}

// stop ends this process. River work in flight is canceled, as in a crash.
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

// applySettings changes the merchant's settings through a configuration
// application against the current revision.
func (w *world) applySettings(ctx context.Context, settings billing.MerchantSettings) error {
	client := w.client[embedded]
	current, err := client.GetMerchantConfiguration(ctx)
	if err != nil {
		return err
	}
	_, err = client.UpdateMerchantConfiguration(ctx, billing.UpdateMerchantConfigurationParams{IdempotencyKey: uuid.NewString(), ExpectedRevision: &current.Revision, Settings: &settings})
	return err
}

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
	rows, err = w.pool.Query(ctx, `SELECT id::text, status, lease_expires_at FROM `+schema+`.provider_intents WHERE status = 'in_flight'`)
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
		_, err = w.pool.Exec(ctx, `UPDATE `+schema+`.provider_intents SET status = $2, lease_expires_at = $3 WHERE id = $1::uuid`, r.id, r.status, r.claimed)
		require.NoError(w.t, err)
	}
}

// runRenewals runs the engine's scheduled due pass once, then waits for the
// fleet to finish every operation it accepted.
func (w *world) runRenewals() {
	w.t.Helper()
	res, err := w.jobs.Insert(w.t.Context(), dunningPass{}, &river.InsertOpts{Queue: openrails.QueueBilling})
	require.NoError(w.t, err)
	w.waitJob(res.Job.ID)
	if os.Getenv("GF_DEBUG") != "" {
		page, _ := w.jobs.JobList(w.t.Context(), river.NewJobListParams().First(100))
		for _, j := range page.Jobs {
			w.t.Logf("job %d %s %s sched=%s attempt=%d errs=%v", j.ID, j.Kind, j.State, j.ScheduledAt.Format(time.RFC3339), j.Attempt, j.Errors)
		}
	}
}

// refreshProviders runs the normal observation worker after a healthy fixture
// advances its business clock. It is explicit: outage and ambiguity scenarios
// must not silently gain a provider refresh whenever they advance time.
func (w *world) refreshProviders() {
	w.t.Helper()
	id, _, err := riverjobs.EnqueueMerchantRefresh(w.t.Context(), w.jobs, w.client[embedded].MerchantID().UUID(), openrails.QueueBilling)
	require.NoError(w.t, err)
	// An annual obligation can require hundreds of daily history windows.
	// This read-completion bound does not change collection or crash deadlines.
	deadline := time.Now().Add(3 * time.Minute)
	for {
		job, err := w.jobs.JobGet(w.t.Context(), id)
		require.NoError(w.t, err)
		if job.State == rivertype.JobStateCompleted {
			w.settle()
			return
		}
		if job.State == rivertype.JobStateRetryable || job.State == rivertype.JobStateDiscarded || time.Now().After(deadline) {
			w.t.Fatalf("provider refresh did not complete: state=%s errors=%v", job.State, job.Errors)
		}
		// Bounded catch-up snoozes the same ordinary job between windows.
		// Match settle's due-job promotion without advancing business time.
		if job.State == rivertype.JobStateScheduled && !job.ScheduledAt.After(time.Now()) {
			_, err := w.jobs.JobRetry(w.t.Context(), id)
			require.NoError(w.t, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// settleCollectionScans drains the ordinary scans created by healthy refresh.
// Fault fixtures call this while still before due, then install their barrier.
func (w *world) settleCollectionScans() {
	w.t.Helper()
	require.Eventually(w.t, func() bool {
		jobs, err := w.jobs.JobList(w.t.Context(), river.NewJobListParams().Kinds("openrails.dunning", "openrails.invoice").States(rivertype.JobStateAvailable, rivertype.JobStateRunning, rivertype.JobStatePending).First(100))
		return err == nil && len(jobs.Jobs) == 0
	}, 30*time.Second, 20*time.Millisecond, "healthy refresh's ordinary scans finish before the fault")
}

var workKinds = []string{"openrails.provider_operation", "openrails.subscription_converge", "openrails.card_refresh"}

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
	rows, err := w.pool.Query(context.Background(), `SELECT intent_type, status, coalesce(last_failure_reason,''), coalesce(result_evidence::text,''), lease_expires_at FROM `+pgx.Identifier{w.schema}.Sanitize()+`.provider_intents`)
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
// does, after the engine clock passed its next attempt. An operation running
// as the clock moved may have read the old time and snoozed past it, so wake
// retries once more after the first round settles.
func (w *world) wake() {
	w.t.Helper()
	for range 2 {
		page, err := w.jobs.JobList(w.t.Context(), river.NewJobListParams().Kinds("openrails.provider_operation").States(rivertype.JobStateScheduled, rivertype.JobStateRetryable).First(100))
		require.NoError(w.t, err)
		for _, job := range page.Jobs {
			_, err := w.jobs.JobRetry(w.t.Context(), job.ID)
			require.NoError(w.t, err)
		}
		w.settle()
	}
}

// advanceHealthyTo models ordinary observation immediately before a planned
// billing action. Fault scenarios instead refreshBeforePeriodEnd, install their
// fault, then use raw advance/toPeriodEnd. Raw advance never implies refresh.
func (w *world) advanceHealthyTo(at time.Time) {
	w.t.Helper()
	if d := at.Add(-time.Minute).Sub(w.clock.Now()); d > 0 {
		w.advance(d)
	}
	w.refreshProviders()
	w.settleCollectionScans()
	if d := at.Sub(w.clock.Now()); d > 0 {
		w.advance(d)
	}
}

// inSchema relocates authored SQL to a world's schema.
func inSchema(schema, sql string) string {
	out, err := sqlschema.Rewrite(sql, schema)
	if err != nil {
		panic(err)
	}
	return out
}

// armDestructive is the documented operator arming (docs/operations.md, "The
// destructive-action kill switch"): the instance switch and this merchant's
// policy row. A fresh deployment ships with both off.
func (w *world) armDestructive() {
	w.t.Helper()
	ctx := w.t.Context()
	_, err := w.pool.Exec(ctx, w.q(`UPDATE billing.destructive_action_switch SET enabled = true, updated_by = 'e2e'`))
	require.NoError(w.t, err)
	_, err = w.pool.Exec(ctx, w.q(`INSERT INTO billing.merchant_destructive_policy (merchant_id, destructive_actions_enabled, enforce_armed_at, updated_by, reason)
		SELECT id, true, now(), 'e2e', 'reviewed' FROM billing.merchants WHERE slug = $1
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
	res, err := w.jobs.Insert(w.t.Context(), rescuePass{}, &river.InsertOpts{Queue: openrails.QueueBilling})
	require.NoError(w.t, err)
	w.waitJob(res.Job.ID)
}

func (dunningPass) Kind() string { return "openrails.dunning" }

func (w *world) advance(d time.Duration) { w.clock.Advance(d) }

// staff calls a merchant route with the staff credential and returns the
// status and raw body.
func (w *world) staff(method, path string) (int, string) {
	w.t.Helper()
	return w.merchantCall(w.auth.token(w.t, "staff"), method, path)
}

// merchantCall calls a merchant route with token and returns the status and
// raw body.
func (w *world) merchantCall(token, method, path string) (int, string) {
	w.t.Helper()
	req, err := http.NewRequestWithContext(w.t.Context(), method, w.server.URL+mountPrefix+path, nil)
	require.NoError(w.t, err)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("OpenRails-Merchant", w.slug)
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

// newCustomer is a user of the host, already a customer: settings create
// one OpenRails has not seen.
func (w *world) newCustomer() *customer {
	id := uuid.NewString()
	_, err := w.client[embedded].UpdateCustomer(w.t.Context(), billing.CustomerID(uuid.MustParse(id)), billing.UpdateCustomerParams{})
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
		setup := c.must(http.MethodPost, "/payment-method-setups", "setup-"+uuid.NewString(), map[string]any{"psp_id": c.w.psp["stripe"], "consent": true})
		c.w.stripe.completeSetup(strings.TrimSuffix(setup["client_secret"].(string), "_secret_gf"), card)
		confirmed := unwrap(c.must(http.MethodPost, fmt.Sprintf("/payment-method-setups/%s/confirm", setup["id"]), "", nil))
		return confirmed["payment_method_id"].(string)
	case "nmi":
		token := c.w.nmi.Tokenize(card)
		saved := unwrap(c.must(http.MethodPost, "/payment-methods", "", map[string]any{"psp_id": c.w.psp["nmi"], "payment_token": token, "billing_details": map[string]any{"name": "E2E Payer"}}))
		return saved["id"].(string)
	}
	c.w.t.Fatalf("unknown rail %s", rail)
	return ""
}

// subscribe enrolls an engine-owned membership: the merchant hands the
// customer a checkout session through tp's Client, and the customer pays it
// signed in with a saved card.
func (c *customer) subscribe(tp topology, rail, priceID, entitlement, method string) billing.SubscriptionID {
	c.w.t.Helper()
	id := c.enrollOnce(tp, rail, priceID, entitlement, method)
	subs, err := c.w.client[tp].ListSubscriptions(c.w.t.Context(), billing.SubscriptionListParams{CustomerID: c.customerID()})
	require.NoError(c.w.t, err)
	require.Len(c.w.t, subs.Items, 1)
	require.Equal(c.w.t, id, subs.Items[0].ID)
	return id
}

// subscribeAgain enrolls a returning customer, whose earlier memberships stay.
func (c *customer) subscribeAgain(tp topology, rail, priceID, entitlement, method string) billing.SubscriptionID {
	c.w.t.Helper()
	return c.enrollOnce(tp, rail, priceID, entitlement, method)
}

func (c *customer) enrollOnce(tp topology, rail, priceID, _, method string) billing.SubscriptionID {
	c.w.t.Helper()
	paid := c.mustCheckout(tp, order{price: pid(priceID), rail: rail, method: method, successURL: "https://e2e.test/return"})
	require.NotNil(c.w.t, paid.SubscriptionID, "the payment names the membership: %+v", paid.CheckoutSessionPayResult)
	return *paid.SubscriptionID
}

// identity is the customer as a host hands it to checkout.
func (c *customer) identity() billing.CheckoutCustomerIdentity {
	return billing.CheckoutCustomerIdentity{ID: cid(c.id)}
}

// cid and pid read test ids as their typed form.
func cid(id string) billing.CustomerID { return billing.CustomerID(uuid.MustParse(id)) }

func pmid(id string) billing.PaymentMethodID {
	parsed, err := billing.ParsePaymentMethodID(id)
	if err != nil {
		panic(err)
	}
	return parsed
}

func pid(id string) billing.PriceID {
	parsed, err := billing.ParsePriceID(id)
	if err != nil {
		panic(err)
	}
	return parsed
}

// asJSON is v as its wire JSON object.
func asJSON(t testing.TB, v any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(raw, &out))
	return out
}

func (c *customer) entitled(entitlement string) bool {
	c.w.t.Helper()
	return c.entitledAt(entitlement, c.w.clock.Now())
}

// customerID is the customer's typed id.
func (c *customer) customerID() billing.CustomerID {
	return billing.CustomerID(uuid.MustParse(c.id))
}

// giftProduct is a product that is not for sale (no price) granting keys.
func (w *world) giftProduct(keys ...string) billing.ProductID {
	w.t.Helper()
	product, err := w.client[embedded].CreateProduct(w.t.Context(), billing.CreateProductParams{Key: "gift-" + uuid.NewString()[:8], DisplayName: "Gift", Entitlements: keys})
	require.NoError(w.t, err)
	return product.ID
}

// grant grants the customer a product free through the host's client.
func (c *customer) grant(product billing.ProductID, hours *int, ends *time.Time) billing.ProductAccessGrant {
	c.w.t.Helper()
	granted, err := c.w.client[embedded].CreateProductAccess(c.w.t.Context(), billing.CreateProductAccessBatchParams{
		Items: []billing.CreateProductAccessParams{{CustomerID: c.customerID(), ProductID: product, Hours: hours, EndsAt: ends}},
	})
	require.NoError(c.w.t, err)
	return granted[0]
}

// membership creates a monthly auto-renew product and price.
func (w *world) membership(entitlement string, unitAmount int64) *billing.Price {
	w.t.Helper()
	return w.membershipEvery(entitlement, unitAmount, monthHours)
}

// membershipEvery is an engine membership renewing every hours.
func (w *world) membershipEvery(entitlement string, unitAmount int64, hours int) *billing.Price {
	w.t.Helper()
	client := w.client[embedded]
	group := benefitGroup(entitlement)
	product, err := client.CreateProduct(w.t.Context(), billing.CreateProductParams{Key: "member-" + uuid.NewString()[:8], DisplayName: "Membership", TierGroup: &group, Entitlements: []string{entitlement}})
	require.NoError(w.t, err)
	price, err := client.CreatePrice(w.t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: product.Key + "-usd", UnitAmount: unitAmount, Currency: "USD", BillingIntervalHours: &hours, AccessDurationHours: &hours})
	require.NoError(w.t, err)
	return price
}

// benefitGroup is the tier group of the harness's recurring products granting
// entitlement: recurring products sharing a benefit must share a group.
func benefitGroup(entitlement string) string { return "e2e:" + entitlement }

func (w *world) subscription(tp topology, id billing.SubscriptionID) *billing.Subscription {
	w.t.Helper()
	sub, err := w.client[tp].GetSubscription(w.t.Context(), id)
	require.NoError(w.t, err)
	return sub
}

// nextRetry is the dunning's next retry, nil out of dunning.
func nextRetry(sub *billing.Subscription) *time.Time {
	if sub.Dunning == nil {
		return nil
	}
	return sub.Dunning.NextRetryAt
}

// graceEnds is the subscription's stored dunning grace: internal state the
// wire no longer carries.
func (w *world) graceEnds(id billing.SubscriptionID) *time.Time {
	w.t.Helper()
	var at *time.Time
	require.NoError(w.t, w.pool.QueryRow(w.t.Context(), w.q(`SELECT grace_ends_at FROM billing.subscriptions WHERE id = $1`), id.UUID()).Scan(&at))
	return at
}

func (w *world) payments(tp topology, customerID string) []billing.Payment {
	w.t.Helper()
	id, err := billing.ParseCustomerID(customerID)
	require.NoError(w.t, err)
	page, err := w.client[tp].ListPayments(w.t.Context(), billing.PaymentListParams{CustomerID: id, PageRequest: billing.PageRequest{Limit: 100}})
	require.NoError(w.t, err)
	return page.Items
}

// cid is the customer's id as a Client takes it.
func (c *customer) cid() billing.CustomerID {
	c.w.t.Helper()
	id, err := billing.ParseCustomerID(c.id)
	require.NoError(c.w.t, err)
	return id
}

// declaredCard is a card's display facts as an import declares them.
func declaredCard(c card) *billing.CardDetails {
	month, year := 12, 2035
	return &billing.CardDetails{Brand: &c.Brand, Last4: &c.Last4, ExpMonth: &month, ExpYear: &year}
}

// completed is the payments whose money moved, a refunded charge included.
func completed(payments []billing.Payment) []billing.Payment {
	var out []billing.Payment
	for _, p := range payments {
		switch p.Status {
		case billing.PaymentSucceeded, billing.PaymentRefunded, billing.PaymentPartiallyRefunded:
			out = append(out, p)
		}
	}
	return out
}

// loseSubmissions makes the rail's next n charge requests fail in transit
// without reaching the provider.
func (w *world) loseSubmissions(rail string, n int) {
	if rail == "stripe" {
		w.stripe.LoseSubmissions(n)
	} else {
		w.nmi.LoseSales(n)
	}
}

// lostSubmissions is how many charge requests were lost in transit.
func (w *world) lostSubmissions(rail string) int {
	if rail == "stripe" {
		return w.stripe.Lost()
	}
	return w.nmi.Lost()
}

// readUnavailable makes the rail's authoritative payment read fail.
func (w *world) readUnavailable(rail string, down bool) {
	if rail == "stripe" {
		w.stripe.PaymentIntentListUnavailable(down)
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

// typedPriceID reads a price id the harness keeps as text.
func typedPriceID(t testing.TB, id string) billing.PriceID {
	t.Helper()
	parsed, err := billing.ParsePriceID(id)
	require.NoError(t, err)
	return parsed
}

// typedProductID reads a product id the harness keeps as text.
func typedProductID(t testing.TB, id string) billing.ProductID {
	t.Helper()
	parsed, err := billing.ParseProductID(id)
	require.NoError(t, err)
	return parsed
}

// recordUsage records one usage event; the item's refusal is the error.
func recordUsage(ctx context.Context, client *openrails.Client, params billing.RecordUsageParams) (*billing.UsageEvent, error) {
	results, err := client.RecordUsage(ctx, []billing.RecordUsageParams{params})
	if err != nil {
		return nil, err
	}
	return results[0].Event, results[0].Err()
}

// createCreditGrant grants one customer credit: a batch of one.
func createCreditGrant(ctx context.Context, client *openrails.Client, customer billing.CustomerID, params billing.CreateCreditGrantParams) (*billing.CreditGrant, error) {
	params.CustomerID = customer
	grants, err := client.CreateCreditGrants(ctx, []billing.CreateCreditGrantParams{params})
	if err != nil {
		return nil, err
	}
	return &grants[0], nil
}

// releaseAdmission releases one admission: a batch of one.
func releaseAdmission(ctx context.Context, client *openrails.Client, requestID string) (*billing.Admission, error) {
	results, err := client.ReleaseAdmissions(ctx, []string{requestID})
	if err != nil {
		return nil, err
	}
	return results[0].Admission, results[0].Err()
}

// extendAdmission extends one hold: a batch of one.
func extendAdmission(ctx context.Context, client *openrails.Client, requestID string, expiresAt time.Time) (*billing.Admission, error) {
	results, err := client.ExtendAdmissions(ctx, []billing.ExtendAdmissionParams{{RequestID: requestID, ExpiresAt: expiresAt}})
	if err != nil {
		return nil, err
	}
	return results[0].Admission, results[0].Err()
}
