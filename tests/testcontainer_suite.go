//go:build integration

package tests

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	server "github.com/open-rails/openrails/internal/http"
	"github.com/open-rails/openrails/internal/integrationharness"
	"github.com/open-rails/openrails/internal/integrations/nmi"
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
// (a Stripe rail, ClickHouse analytics).
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
	Rails  config.RailMerchantAccountSet
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
	clickhouse      bool
	persistent      bool
}

// TestSuiteOption customizes an integration test suite before it boots.
type TestSuiteOption func(*TestContainerSuite)

const testNMIRailMerchantAccountID = "OpenRails Test Merchant <billing@test.example>"

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

// WithSuiteClickHouse opts the suite into analytics: the shared ClickHouse
// (dbtest.SharedClickHouse) is wired + migrated so EventLogService runs.
// Forces a fresh (non-shared) suite boot.
func WithSuiteClickHouse() TestSuiteOption {
	return func(suite *TestContainerSuite) {
		suite.clickhouse = true
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
	if suite.clickhouse {
		httpAddr, nativeAddr := dbtest.SharedClickHouse(suite.t)
		opts = append(opts, integrationharness.WithClickHouse(&config.ClickHouseConfig{
			HTTPAddr:   httpAddr,
			ClientAddr: nativeAddr,
			Database:   dbtest.ClickHouseEnvOr("OPENRAILS_TEST_CH_DATABASE", "test_analytics"),
			Username:   dbtest.ClickHouseEnvOr("OPENRAILS_TEST_CH_USERNAME", "test_user"),
			Password:   dbtest.ClickHouseEnvOr("OPENRAILS_TEST_CH_PASSWORD", "test_password"),
		}))
	}

	// Super DSN: the legacy tests/ fixtures and direct service calls predate
	// merchant-pinned contexts; RLS enforcement is covered by the harness's
	// StartStandalone consumers (cross-merchant isolation + rls suites).
	suite.surface = suite.harness.StartStandaloneSuper("usd", opts...)
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
func defaultSuiteRails(stripeSecretKey string) config.RailMerchantAccountSet {
	rails := config.RailMerchantAccountSet{
		"ccbill": {
			Rail: models.RailCCBill,
			// #711: the clientAccnum/clientSubacc pair derives from the account_id.
			AccountID: "945280-0000",
			CCBill: &config.CCBillRailConfig{
				Salt: "test-salt",
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
		"mobius": {
			Rail:      models.RailNMI,
			AccountID: envOrDefault("OPENRAILS_TEST_MOBIUS_GATEWAY_ID", "579145"),
			NMI: &config.NMIRailConfig{
				SecurityKey:          envOrDefault("OPENRAILS_TEST_MOBIUS_SECURITY_KEY", "6457Thfj624V5r7WUwc5v6a68Zsd6YEm"),
				WebhookSigningSecret: envOrDefault("OPENRAILS_TEST_MOBIUS_WEBHOOK_SECRET", ""),
			},
		},
	}
	if stripeSecretKey != "" {
		rails["stripe"] = &config.RailMerchantAccountConfig{
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

	// NMI follows deployment posture (#681): test_mode ⇒ environment=test rows.
	nmiEnv := config.ExpectedProviderEnvironment(suite.Config.IsTestMode())
	suite.seedRailMerchantAccountWithEvidence(ctx, "nmi", nmiEnv, testNMIRailMerchantAccountID, `{"source":"test_fixture"}`)
	nmiSecret, err := merchants.RailMerchantAccountSecretName("nmi", nmiEnv, testNMIRailMerchantAccountID, "security_key")
	require.NoError(suite.t, err)
	_, err = suite.App.Runtime.Merchants.PutCredential(ctx, dbtest.TestMerchantID, nmiSecret, "test-security-key")
	require.NoError(suite.t, err)

	// CCBill must be sandbox-posture (#668): the webhook IP-allowlist bypass is
	// refused while ANY environment=live CCBill row exists, and the test_mode
	// webhook path resolves accounts under environment=test.
	ccbillAccountID := "945280-0000"
	ccbillEnv := config.ExpectedProviderEnvironment(suite.Config.IsTestMode())
	suite.seedRailMerchantAccountWithEvidence(ctx, "ccbill", ccbillEnv, ccbillAccountID, `{"source":"test_fixture"}`)
	ccbillSecret, err := merchants.RailMerchantAccountSecretName("ccbill", ccbillEnv, ccbillAccountID, "salt")
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
		INSERT INTO openrails.rail_merchant_accounts
		    (merchant_id, rail, environment, account_id, archived, evidence, last_verified_at)
		VALUES ($1::uuid, $2, $3, $4, false, $5::jsonb, now())
		ON CONFLICT (rail, environment, account_id) DO UPDATE
		   SET archived = false,
		       evidence = EXCLUDED.evidence,
		       last_verified_at = EXCLUDED.last_verified_at,
		       updated_at = now()
		 WHERE openrails.rail_merchant_accounts.merchant_id = EXCLUDED.merchant_id
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
	if suite == nil || suite.App == nil || suite.App.Runtime == nil {
		return
	}
	clients := make(map[string]*nmi.NMIClient)
	for _, proc := range suite.Rails.ByRail(models.RailNMI) {
		accountID := proc.EffectiveAccountID()
		settings := proc.ToNMIProviderSettings()
		client, err := nmi.NewClient(accountID, settings, suite.Config.IsTestMode())
		require.NoError(suite.t, err)
		client.ReadOnly = suite.Config.IsProviderReadOnly()
		// Keyed by account_id, with the active account aliased under the rail key
		// "nmi" below (matching createNMIClients).
		clients[accountID] = client
	}
	if _, active, err := suite.Rails.ActiveRailByType(models.RailNMI); err == nil && active != nil {
		if c, ok := clients[active.EffectiveAccountID()]; ok {
			clients[string(models.RailNMI)] = c
		}
	}

	rt := suite.App.Runtime
	rt.NMIClients = clients
	if rt.SubscriptionService != nil {
		rt.SubscriptionService.NMIClients = clients
	}
	if rt.VaultService != nil {
		rt.VaultService.NMIClients = clients
	}
	if rt.CheckoutService != nil {
		rt.CheckoutService.NMIClients = clients
		if rt.CheckoutService.NMISaleService != nil {
			rt.CheckoutService.NMISaleService.NMIClients = clients
		}
	}
	suite.RearmIntentPlumbing()
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
	if rt.VaultService != nil {
		rt.VaultService.DeleteIntents = &intents.VaultDeleteThrough{Runner: runner}
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

// GetPrice retrieves a price by ID from the database.
func (suite *TestContainerSuite) GetPrice(priceID uuid.UUID) *models.Price {
	suite.t.Helper()
	price, err := catalog.NewPriceService(suite.App.Runtime.DB).GetByID(suite.ctx, priceID)
	require.NoError(suite.t, err, "Failed to get price by ID")
	return price
}
