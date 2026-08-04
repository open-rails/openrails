//go:build integration

package tests

import (
	"context"
	"github.com/open-rails/openrails/internal/modules/money"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	server "github.com/open-rails/openrails/internal/http"
	"github.com/open-rails/openrails/internal/integrationharness"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/catalog"
	solanatokens "github.com/open-rails/openrails/internal/modules/solana/tokens"
	"github.com/open-rails/openrails/pkg/billingauth"
	embcp "github.com/open-rails/openrails/pkg/embedded/controlplane"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jonboulle/clockwork"
	"github.com/redis/go-redis/v9"
	"github.com/riverqueue/river"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
)

// TestContainerSuite is the tests package's compat facade over the ONE blessed
// integration stack (internal/dbtest + internal/integrationharness, #694). It
// preserves the legacy suite surface (App/Pool/Server/seed helpers) while the
// machinery — shared Postgres/Redis containers, the real standalone server
// boot, in-process River workers, real minted delegated credentials, and the
// per-test injectable clock — lives in the harness. ONE shared suite serves the
// whole package; fresh boots remain only for construction-time divergence
// (a Stripe rail).
type TestContainerSuite struct {
	t *testing.T

	harness *integrationharness.Harness
	surface *integrationharness.Surface

	// App/Server are the real standalone graph (serverboot.NewServer — the
	// cmd/openrails run-server composition root) with workers running.
	App    *app.App
	Server *server.Server
	// Pool is the PRIVILEGED fixture pool over the shared DSN (raw-SQL helpers).
	Pool        *pgxpool.Pool
	RedisClient *redis.Client
	// Config is the app's live *config.Config (tests mutate e.g. APIURL).
	Config *config.Config
	Rails  config.PSPSet
	// ServerURL is a real HTTP base URL (httptest-owned port).
	ServerURL string

	ctx context.Context

	// clock is the construction-time SettableClock every service captured; swap
	// its delegate for fake time — no post-boot per-service fan-out exists.
	clock *integrationharness.SettableClock
	// minter is the suite's registered delegated-token issuer: user credentials
	// are REAL RS256 delegated access tokens resolved through the real control
	// plane (issuer registry + authority intersection), per #339's default path.
	minter *integrationharness.DelegatedIssuer

	// construction options
	initialClock    clockwork.Clock
	stripeSecretKey string
	persistent      bool
}

// TestSuiteOption customizes an integration test suite before it boots.
type TestSuiteOption func(*TestContainerSuite)

const testNMIProviderKey = "mobius"

// testNMIRailMerchantAccountID is the suite's ONE active NMI provider
// account (#788: the mobius gateway id from defaultSuiteRails — the harness
// seeds it as the armed rail state every consumer resolves).
func testNMIRailMerchantAccountID() string {
	return envOrDefault("OPENRAILS_TEST_MOBIUS_GATEWAY_ID", "579145")
}

// WithSuiteClock injects the initial clock before the runtime, services,
// workers, and seed helpers are created. On the shared suite this simply swaps
// the settable clock's delegate for the test's duration.
func WithSuiteClock(clock clockwork.Clock) TestSuiteOption {
	return func(suite *TestContainerSuite) {
		suite.initialClock = clock
	}
}

// WithSuiteStripeRail adds an active Stripe rail (construction-time config,
// like the mobius/ccbill/solana defaults) so the hosted Stripe checkout path
// is reachable. Pair with stripeapi.SetTestBaseTransport so no request ever
// leaves the process. Forces a fresh (non-shared) suite boot.
func WithSuiteStripeRail(secretKey string) TestSuiteOption {
	return func(suite *TestContainerSuite) {
		suite.stripeSecretKey = secretKey
	}
}

// NewTestContainerSuite boots a FRESH suite bound to t (resources torn down by
// t.Cleanup). Prefer setupTestSuite/getSharedTestSuite, which pool onto the
// package-shared suite whenever construction-time options allow.
func NewTestContainerSuite(t *testing.T, opts ...TestSuiteOption) *TestContainerSuite {
	suite := &TestContainerSuite{t: t, ctx: context.Background()}
	for _, opt := range opts {
		if opt != nil {
			opt(suite)
		}
	}
	suite.harness = integrationharness.New(t, suite.ctx)
	suite.boot()
	return suite
}

// newPersistentSuite boots the package-shared suite: resource lifetimes are
// owned by CleanupSharedSuite (TestMain), not the creating test.
func newPersistentSuite(t *testing.T) *TestContainerSuite {
	suite := &TestContainerSuite{t: t, ctx: context.Background(), persistent: true}
	suite.harness = integrationharness.NewPersistent(t, suite.ctx)
	suite.boot()
	return suite
}

func (suite *TestContainerSuite) boot() {
	suite.t.Helper()

	// Reduce noise during tests.
	logrus.SetLevel(logrus.WarnLevel)

	suite.clock = integrationharness.NewSettableClock(suite.initialClock)
	suite.Rails = defaultSuiteRails(suite.stripeSecretKey)

	opts := []integrationharness.StandaloneOption{
		integrationharness.WithWorkers(),
		integrationharness.WithClock(suite.clock),
		integrationharness.WithRails(suite.Rails),
		integrationharness.WithConfiguredMerchant(dbtest.TestMerchantID),
		// User routes authenticate the SAME real delegated tokens the self
		// surface uses: real signature/expiry/registry resolution via the control
		// plane, mapped to a UserContext. Claim control (tests choose subject
		// UUIDs) is exactly the production delegated model — hosts pick
		// delegated_sub.
		integrationharness.WithAuthenticator(&suiteDelegatedUserAuthenticator{suite: suite}),
	}
	// The server connects as openrails_app: RLS enforces on every route this
	// suite drives, exactly as in production (or#867). Fixtures that write
	// merchant-owned rows go through suite.MerchantPool (pinned), not suite.Pool.
	suite.surface = suite.harness.StartStandalone("usd", opts...)
	suite.App = suite.surface.App()
	suite.Server = suite.surface.Server()
	suite.Pool = suite.harness.Pool()
	suite.RedisClient = suite.harness.Redis
	suite.Config = suite.App.Config
	suite.ServerURL = suite.surface.BaseURL

	suite.seedRailMerchantAccountFixtures()

	// One real delegated issuer per suite (unique slug: suites share one DB and
	// an upsert on a shared slug would rotate a live suite's keys mid-run).
	suite.minter = suite.surface.RegisterDelegatedIssuer("tests-host-"+uuid.NewString()[:8], dbtest.TestMerchantSlug)
}

// defaultSuiteRails is the construction-time payment-rail merchant-account
// state (not infrastructure config.yaml state) every suite boots with.
func defaultSuiteRails(stripeSecretKey string) config.PSPSet {
	rails := config.PSPSet{
		"ccbill": {
			Rail: models.RailCCBill,
			// #711: the clientAccnum/clientSubacc pair derives from the account_id.
			AccountID: "945280-0000",
			CCBill: &config.CCBillRailConfig{
				Salt:             "test-salt",
				DataLinkUsername: "dl-user",
				DataLinkPassword: "dl-pass",
			},
		},
		"solana": {
			Rail:      models.RailSolana,
			AccountID: "DzGLHdTfgHCYh8v3qNGJHn85CyX7aeFmqoUdVRBYkWMh",
			Solana: &config.SolanaRailConfig{
				Tokens: solanatokens.DefaultDevnetTokens(),
			},
		},
		// Test-only env overrides use the OPENRAILS_TEST_* prefix (#711 — the
		// runtime RAILS_ config prefix is retired; don't teach operators a dead one).
		testNMIProviderKey: {
			Rail:      models.RailNMI,
			AccountID: envOrDefault("OPENRAILS_TEST_MOBIUS_GATEWAY_ID", "579145"),
			NMI: &config.NMIRailConfig{
				SecurityKey:          envOrDefault("OPENRAILS_TEST_MOBIUS_SECURITY_KEY", "6457Thfj624V5r7WUwc5v6a68Zsd6YEm"),
				WebhookSigningSecret: envOrDefault("OPENRAILS_TEST_MOBIUS_WEBHOOK_SECRET", ""),
			},
		},
	}
	if stripeSecretKey != "" {
		rails["stripe"] = &config.PSPConfig{
			Rail:      models.RailStripe,
			AccountID: "acct_openrails_test",
			Stripe: &config.StripeRailConfig{
				SecretKey: stripeSecretKey,
			},
		}
	}
	return rails
}

func envOrDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

// suiteDelegatedUserAuthenticator adapts the real control-plane delegated-token
// resolver to billingauth.Authenticator for the user-route surface, so ONE real
// credential works across /v1/* and /v1/me/*. No claims are trusted unverified:
// resolution is signature + expiry + issuer-registry + authority intersection.
type suiteDelegatedUserAuthenticator struct {
	suite *TestContainerSuite
}

func (a *suiteDelegatedUserAuthenticator) Authenticate(ctx context.Context, r *http.Request) (billingauth.UserContext, error) {
	if a == nil || a.suite == nil || a.suite.App == nil {
		return billingauth.UserContext{}, billingauth.ErrUnauthenticated
	}
	cp := embcp.Get(a.suite.App)
	if cp == nil {
		return billingauth.UserContext{}, billingauth.ErrUnauthenticated
	}
	header := strings.TrimSpace(r.Header.Get("Authorization"))
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return billingauth.UserContext{}, billingauth.ErrUnauthenticated
	}
	resolved, err := cp.ResolveDelegated(ctx, strings.TrimSpace(header[len(prefix):]), r.Header.Get("Origin"))
	if err != nil {
		return billingauth.UserContext{}, billingauth.ErrUnauthenticated
	}
	return billingauth.UserContext{
		UserID:        resolved.DelegatedSubject,
		Email:         resolved.Email,
		EmailVerified: resolved.EmailVerified,
		Username:      resolved.Username,
	}, nil
}

// MintUserToken mints a REAL delegated access token for userID (a UUID string,
// #364) carrying email via the attributes escape hatch. It authenticates both
// the user-route and self-service surfaces.
func (suite *TestContainerSuite) MintUserToken(userID, email string) string {
	suite.t.Helper()
	return suite.minter.Mint(userID, email, "", nil)
}

func (suite *TestContainerSuite) seedRailMerchantAccountFixtures() {
	suite.t.Helper()
	ctx := dbtest.WithTestMerchant(context.Background())

	// #788: the NMI account is seeded ONCE by the harness from
	// defaultSuiteRails (the mobius gateway) — a second fixture row here would
	// make active-account resolution nondeterministic.

	// CCBill must be sandbox-posture (#668): the webhook IP-allowlist bypass is
	// refused while ANY environment=live CCBill row exists, and the test_mode
	// webhook path resolves accounts under environment=test.
	ccbillAccountID := "945280-0000"
	ccbillEnv := config.ExpectedProviderEnvironment(suite.Config.IsTestMode())
	suite.seedRailMerchantAccountWithEvidence(ctx, "ccbill", ccbillEnv, ccbillAccountID, `{"source":"test_fixture"}`)
	ccbillSecret, err := merchants.PSPSecretName("ccbill", ccbillEnv, ccbillAccountID, "salt")
	require.NoError(suite.t, err)
	_, err = suite.App.Runtime.Merchants.PutCredential(ctx, dbtest.TestMerchantID, ccbillSecret, "test-salt")
	require.NoError(suite.t, err)

	suite.seedRailMerchantAccountWithEvidence(ctx, "solana", config.ExpectedProviderEnvironment(suite.Config.IsTestMode()), "DzGLHdTfgHCYh8v3qNGJHn85CyX7aeFmqoUdVRBYkWMh", `{"source":"test_fixture"}`)
}

func (suite *TestContainerSuite) seedRailMerchantAccountWithEvidence(ctx context.Context, rail, environment, accountID, evidence string) {
	suite.t.Helper()
	if evidence == "" {
		evidence = `{"source":"test_fixture"}`
	}
	tx, err := suite.Pool.Begin(ctx)
	require.NoError(suite.t, err)
	defer tx.Rollback(ctx)

	_, err = tx.Exec(ctx, `
		INSERT INTO openrails.psps
		    (merchant_id, rail, environment, account_id, archived, evidence, last_verified_at)
		VALUES ($1::uuid, $2, $3, $4, false, $5::jsonb, now())
		ON CONFLICT (rail, environment, account_id) DO UPDATE
		   SET archived = false,
		       evidence = EXCLUDED.evidence,
		       last_verified_at = EXCLUDED.last_verified_at,
		       updated_at = now()
		 WHERE openrails.psps.merchant_id = EXCLUDED.merchant_id
	`, dbtest.TestMerchantID.UUID(), rail, environment, accountID, evidence)
	require.NoError(suite.t, err)
	require.NoError(suite.t, tx.Commit(ctx))
}

// Cleanup tears down the suite. Fresh suites are torn down by t.Cleanup
// automatically (harness-registered); the shared suite is closed here via
// CleanupSharedSuite from TestMain.
func (suite *TestContainerSuite) Cleanup() {
	if suite.persistent && suite.harness != nil {
		suite.harness.Close()
	}
}

// ExecuteSQL executes a SQL query on the test database ($1-style placeholders).
func (suite *TestContainerSuite) ExecuteSQL(query string, args ...interface{}) (pgconn.CommandTag, error) {
	return suite.Pool.Exec(suite.ctx, query, args...)
}

// SetMockClock swaps the runtime's construction-time SettableClock delegate to
// a fake clock and returns the fake. Every service captured the SettableClock
// at boot, so the swap is process-wide — no per-service fan-out.
func (suite *TestContainerSuite) SetMockClock(t ...time.Time) *clockwork.FakeClock {
	suite.t.Helper()
	var clock *clockwork.FakeClock
	if len(t) > 0 {
		clock = clockwork.NewFakeClockAt(t[0])
	} else {
		// Default to a fixed time for reproducible tests
		clock = clockwork.NewFakeClockAt(time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC))
	}
	suite.clock.Set(clock)
	return clock
}

// ResetMutableRuntimeState restores real time and re-derives the NMI clients
// from the suite's rail config (tests swap transports/keys on them).
func (suite *TestContainerSuite) ResetMutableRuntimeState() {
	suite.t.Helper()
	suite.clock.Set(nil)
	suite.resetNMIClients()
}

func (suite *TestContainerSuite) resetNMIClients() {
	suite.t.Helper()
	// #788: NMI clients arm per charge from the armed rail state (the seeded
	// rail_merchant_accounts rows + secrets); the only per-test mutable state
	// is the endpoint override, which resets to the real sandbox endpoints.
	suite.SetNMIGateway("")
}

// SetNMIGateway points every store-armed NMI charge/verify path at url (a
// fake gateway; "" = real endpoints). Clients are built per charge, so the
// override takes effect immediately; the intent plumbing is re-armed for the
// #674 write-through paths.
func (suite *TestContainerSuite) SetNMIGateway(url string) {
	suite.t.Helper()
	if suite == nil || suite.App == nil || suite.App.Runtime == nil {
		return
	}
	rt := suite.App.Runtime
	if b, ok := rt.CollectionResolver.(*money.MerchantCollectionAdapterBuilder); ok {
		b.Endpoints = money.CollectionEndpoints{NMIV5BaseURL: url, NMIDirectPostURL: url, NMIQueryURL: url}
	}
	if rt.CheckoutService != nil {
		rt.CheckoutService.NMIEndpointOverride = url
	}
	if rt.RailPaymentMethodService != nil {
		rt.RailPaymentMethodService.NMIEndpointOverride = url
	}
	suite.RearmIntentPlumbing()
}

// SeedNMIProviderAccount declares (or re-keys) an NMI provider account in the
// armed rail state — the #788 Layer-A write every consumer then resolves.
// The account is removed again at test cleanup so the suite's shared merchant
// keeps its default active account for later tests.
func (suite *TestContainerSuite) SeedNMIProviderAccount(t *testing.T, accountID, securityKey string) {
	t.Helper()
	integrationharness.SeedRailMerchantAccounts(context.Background(), t, suite.App.Runtime, dbtest.TestMerchantID, config.PSPSet{
		accountID: {
			Rail:      models.RailNMI,
			AccountID: accountID,
			NMI:       &config.NMIRailConfig{SecurityKey: securityKey},
		},
	})
	env := config.ExpectedProviderEnvironment(suite.Config.IsTestMode())
	rowID, _, _, _ := merchants.PSPNaturalKey(string(models.RailNMI), env, accountID)
	t.Cleanup(func() {
		ctx := dbtest.WithTestMerchant(context.Background())
		if store := suite.App.Runtime.Merchants.Secrets(); store != nil {
			for _, key := range []string{"security_key", "webhook_signing_secret"} {
				if name, err := merchants.PSPSecretName(string(models.RailNMI), env, accountID, key); err == nil {
					_ = store.Delete(ctx, dbtest.TestMerchantID, name)
				}
			}
		}
		// Archive (not delete): rows this test stamped (payments/subscriptions)
		// hold FK references; archived accounts drop out of active resolution.
		_, _ = suite.Pool.Exec(ctx, `UPDATE openrails.psps SET archived = true WHERE id = $1`, rowID)
	})
}

// RearmIntentPlumbing rebuilds the inline intent runner + through-structs from
// the runtime's CURRENT clients (#729). The intent registry captures NMIClients
// at build time, so any test that swaps the map must re-arm or the #674
// write-through paths keep talking to the boot-time clients.
func (suite *TestContainerSuite) RearmIntentPlumbing() {
	rt := suite.App.Runtime
	runner := rt.IntentRunner()
	rt.PaymentSourceUpdateIntents = &intents.PaymentSourceUpdateThrough{Runner: runner, DB: rt.DB}
	if rt.CheckoutService != nil {
		rt.CheckoutService.Intents = runner
		if rt.CheckoutService.NMISaleService != nil {
			rt.CheckoutService.NMISaleService.Intents = runner
		}
	}
	if rt.RailPaymentMethodService != nil {
		rt.RailPaymentMethodService.DeleteIntents = &intents.VaultDeleteThrough{Runner: runner}
	}
}

// GetClock returns the clock services currently observe (the settable clock's
// delegate: real, or the installed fake).
func (suite *TestContainerSuite) GetClock() clockwork.Clock {
	return suite.clock.Get()
}

// GetRiverClient returns the River client for job enqueueing and inspection.
// Returns nil if River is not initialized.
func (suite *TestContainerSuite) GetRiverClient() *river.Client[pgx.Tx] {
	if suite.App == nil || suite.App.Runtime == nil {
		return nil
	}
	return suite.App.Runtime.RiverClient
}

// WaitForJobCompletion waits for a specific number of jobs to complete in the billing queue.
// This is useful for testing async job processing.
// Returns true if the expected jobs completed, false if timeout.
func (suite *TestContainerSuite) WaitForJobCompletion(expectedJobs int, timeout time.Duration) bool {
	suite.t.Helper()

	// Query the river_job table to check for completed jobs. River tables live
	// in config.RiverSchema (public, #545), NOT the OpenRails billing schema.
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var count int
		err := suite.Pool.QueryRow(suite.ctx,
			"SELECT COUNT(*) FROM "+config.RiverSchema+".river_job WHERE state = 'completed'").Scan(&count)
		if err == nil && count >= expectedJobs {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// GetPendingJobCount returns the number of pending jobs in the billing queue.
func (suite *TestContainerSuite) GetPendingJobCount() int {
	suite.t.Helper()
	var count int
	err := suite.Pool.QueryRow(suite.ctx,
		"SELECT COUNT(*) FROM "+config.RiverSchema+".river_job WHERE state = 'available'").Scan(&count)
	if err != nil {
		suite.t.Logf("Error getting pending job count: %v", err)
		return 0
	}
	return count
}

// GetCompletedJobCount returns the number of completed jobs in the billing queue.
func (suite *TestContainerSuite) GetCompletedJobCount() int {
	suite.t.Helper()
	var count int
	err := suite.Pool.QueryRow(suite.ctx,
		"SELECT COUNT(*) FROM "+config.RiverSchema+".river_job WHERE state = 'completed'").Scan(&count)
	if err != nil {
		suite.t.Logf("Error getting completed job count: %v", err)
		return 0
	}
	return count
}

// ClearJobQueue removes all jobs from the River queue for clean test state.
func (suite *TestContainerSuite) ClearJobQueue() {
	suite.t.Helper()
	_, err := suite.Pool.Exec(suite.ctx, "DELETE FROM "+config.RiverSchema+".river_job")
	if err != nil {
		suite.t.Logf("Error clearing job queue: %v", err)
	}
}

// FixtureDB is the handle this suite's fixture helpers write and read through:
// the RLS-ENFORCING app role with app.merchant_id pinned to the suite's one
// merchant. Those helpers drive MODULE SERVICES and REPOS directly — below the
// layer that opens the merchant connection in production (MerchantDBConnMW on
// the HTTP path, the River worker's own wrap) — so the fixture has to supply
// what that layer supplies. suite.App.Runtime.DB is the SERVER's unpinned pool:
// code under test must pin it itself, which is exactly what these tests prove.
func (suite *TestContainerSuite) FixtureDB() *db.DB {
	suite.t.Helper()
	return suite.harness.MerchantDB(dbtest.TestMerchantID.UUID())
}

// MerchantCtx returns a context carrying the suite's merchant AND a pinned
// merchant DB connection on the app runtime's pool — literally what
// MerchantDBConnMW does for every request and what a River job does via
// RunInMerchantConn. The connection is released at test end.
//
// A test that calls an App.Runtime service DIRECTLY has stepped below the layer
// that pins in production, so it must stand in for that layer; this is how. A
// test driving a full ENTRY POINT (an HTTP route, a worker) must NOT use the
// result to reach past the entry point — proving the code pins itself is that
// test's whole point, and the server pins its own request context regardless of
// what the test holds.
func (suite *TestContainerSuite) MerchantCtx() context.Context {
	suite.t.Helper()
	ctx := dbtest.WithTestMerchant(context.Background())
	ctx, release, err := suite.App.Runtime.DB.WithMerchantConn(ctx)
	require.NoError(suite.t, err, "pin merchant db connection")
	suite.t.Cleanup(release)
	return ctx
}

// MerchantPool is the raw-SQL counterpart of FixtureDB: the RLS-ENFORCING app
// role pinned to the suite's merchant. Assertions about the suite merchant's own
// rows belong here, not on suite.App.Runtime.DB.Pool() — that is the server's
// BASE pool, which carries no GUC and therefore returns nothing.
func (suite *TestContainerSuite) MerchantPool() *pgxpool.Pool {
	suite.t.Helper()
	return suite.harness.MerchantPool(dbtest.TestMerchantID.UUID())
}

// WorkerCtx is the context a River worker actually receives in production: no
// merchant, no pinned connection. A test that drives a worker's Work() directly
// MUST hand it this and not MerchantCtx() — the worker pinning its own merchant
// connection (RunInMerchantConn) is precisely what such a test exists to prove,
// and a pre-pinned context does the worker's job for it, turning an inert sweep
// into a green test. That is the failure mode or#867 exists to remove.
func (suite *TestContainerSuite) WorkerCtx() context.Context { return context.Background() }

// GetPrice retrieves a price by ID from the database.
func (suite *TestContainerSuite) GetPrice(priceID uuid.UUID) *models.Price {
	suite.t.Helper()
	price, err := catalog.NewPriceService(suite.FixtureDB()).GetByID(suite.ctx, priceID)
	require.NoError(suite.t, err, "Failed to get price by ID")
	return price
}
