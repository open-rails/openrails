//go:build integration

package tests

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/bootstrap"
	"github.com/open-rails/openrails/internal/bootstrap/ginboot"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/db/models"
	dbrepo "github.com/open-rails/openrails/internal/db/repo"
	"github.com/open-rails/openrails/internal/dbtest"
	server "github.com/open-rails/openrails/internal/http"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/migrate"
	solanatokens "github.com/open-rails/openrails/internal/modules/solana/tokens"
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
	"github.com/testcontainers/testcontainers-go"
	clickhousecontainer "github.com/testcontainers/testcontainers-go/modules/clickhouse"
	redismodule "github.com/testcontainers/testcontainers-go/modules/redis"
	"github.com/testcontainers/testcontainers-go/wait"
)

// TestContainerSuite is the tests package's full-stack app harness. Postgres
// lifecycle and migrations live in internal/dbtest; this type owns the remaining
// Redis, ClickHouse, HTTP server, worker, and fixture-helper surface used by the
// older tests in this package.
type TestContainerSuite struct {
	t *testing.T

	// Containers
	redisContainer      *redismodule.RedisContainer
	clickhouseContainer *clickhousecontainer.ClickHouseContainer

	// Application and database connections
	App         *app.App
	Pool        *pgxpool.Pool
	RedisClient *redis.Client

	// Server and configuration
	Server     *server.Server
	httpServer *http.Server
	Config     *config.Config
	Rails      config.RailSet
	ServerURL  string

	// Context for container operations
	ctx context.Context

	workersCancel context.CancelFunc
	workersErrCh  chan error

	clock clockwork.Clock
	port  int
}

// TestSuiteOption customizes an integration test suite before it boots.
type TestSuiteOption func(*TestContainerSuite)

const testNMIProviderAccountID = "OpenRails Test Merchant <billing@test.example>"

// WithSuiteClock injects a clock before the runtime, services, workers, and
// seed helpers are created. Prefer this over SetMockClock for new tests.
func WithSuiteClock(clock clockwork.Clock) TestSuiteOption {
	return func(suite *TestContainerSuite) {
		suite.clock = clock
	}
}

// WithSuitePort sets the HTTP listen port for isolated suites.
func WithSuitePort(port int) TestSuiteOption {
	return func(suite *TestContainerSuite) {
		suite.port = port
	}
}

// NewTestContainerSuite creates a new test container suite
func NewTestContainerSuite(t *testing.T, opts ...TestSuiteOption) *TestContainerSuite {
	suite := &TestContainerSuite{
		t:   t,
		ctx: context.Background(),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(suite)
		}
	}

	suite.SetupSuite()
	return suite
}

// SetupSuite initializes all test containers and services
func (suite *TestContainerSuite) SetupSuite() {
	suite.t.Helper()

	// Set log level to reduce noise during tests
	logrus.SetLevel(logrus.WarnLevel)

	// Postgres is process-shared through internal/dbtest; this suite owns only
	// the extra services its full-stack tests still need.
	suite.startRedisContainer()
	suite.startClickHouseContainer()

	// Initialize config with container connection details
	suite.initializeDatabaseConnections()

	// Run non-Postgres migrations and seed the merchant before app connects.
	suite.prepareDatastores()

	// Initialize server (bootstraps the app and sets up DB connection)
	suite.initializeServer()

	// Wait for server to be ready
	suite.waitForServerReady()
}

// startRedisContainer starts a Redis test container
func (suite *TestContainerSuite) startRedisContainer() {
	suite.t.Helper()

	if strings.TrimSpace(os.Getenv("OPENRAILS_TEST_REDIS_ADDR")) != "" {
		return
	}

	container, err := redismodule.Run(suite.ctx,
		"redis:7-alpine",
		dbtest.WithRedisLimits(),
		testcontainers.WithWaitStrategy(
			wait.ForLog("Ready to accept connections").
				WithStartupTimeout(30*time.Second),
		),
	)
	require.NoError(suite.t, err)

	suite.redisContainer = container
}

// startClickHouseContainer starts a ClickHouse test container
func (suite *TestContainerSuite) startClickHouseContainer() {
	suite.t.Helper()

	if strings.TrimSpace(os.Getenv("OPENRAILS_TEST_CH_HTTP_ADDR")) != "" ||
		strings.TrimSpace(os.Getenv("OPENRAILS_TEST_CH_HTTP_URL")) != "" {
		return
	}

	container, err := clickhousecontainer.Run(suite.ctx,
		"clickhouse/clickhouse-server:25.8-alpine",
		clickhousecontainer.WithUsername("test_user"),
		clickhousecontainer.WithPassword("test_password"),
		clickhousecontainer.WithDatabase("test_analytics"),
		dbtest.WithClickHouseLimits(),
		testcontainers.WithWaitStrategy(
			wait.ForHTTP("/ping").
				WithPort("8123/tcp").
				WithStartupTimeout(60*time.Second),
		),
	)
	require.NoError(suite.t, err)

	suite.clickhouseContainer = container
}

// initializeDatabaseConnections sets up database connections
func (suite *TestContainerSuite) initializeDatabaseConnections() {
	suite.t.Helper()

	postgresConnStr := dbtest.SharedPostgresDSN(suite.t)

	// Get Redis connection details
	redisAddr, err := suite.redisAddress()
	require.NoError(suite.t, err)

	// Get ClickHouse connection details
	clickhouseHTTPAddr, clickhouseClientAddr, err := suite.clickhouseAddresses()
	require.NoError(suite.t, err)

	// Create configuration
	// Use "dev" to skip NMI/CCBill validation in config.Validate()
	suite.Config = &config.Config{
		Env: "dev",
		// Sandbox semantics MUST be explicit (#355): the dev default is now
		// live credentials + mode=full, so the suite sets test_mode=true to keep
		// rails on their sandbox environments (and the NMI demo-key boot
		// probe working).
		TestMode: true,
		Host:     "localhost",
		Port:     8080, // Fixed port for shared test suite
		DB: &config.DBConfig{
			URL: postgresConnStr,
		},
		Redis: &config.RedisConfig{
			Addr:     redisAddr,
			Password: "",
			DB:       0,
		},
		ClickHouse: &config.ClickHouseConfig{
			HTTPAddr:   clickhouseHTTPAddr,
			ClientAddr: clickhouseClientAddr,
			Database:   envOrDefault("OPENRAILS_TEST_CH_DATABASE", "test_analytics"),
			Username:   envOrDefault("OPENRAILS_TEST_CH_USERNAME", "test_user"),
			Password:   envOrDefault("OPENRAILS_TEST_CH_PASSWORD", "test_password"),
		},
		Auth: &config.AuthConfig{
			// HARDCUT (#312/#537): admin authority is LIVE merchant-local AuthKit
			// org permission state (control-plane evaluated), or a
			// deployment-minted admin API key - NOT a claim-based operator
			// merchant. Admin-route integration tests therefore require the embedded
			// control plane wired with the test admin granted the operator role, so
			// the suite enables it here exactly like a production standalone boot.
			Issuer: "https://controlplane.openrails.test",
		},
	}
	// Payment rail credentials are construction-time merchant/provider
	// state, not infrastructure config.yaml state.
	suite.Rails = config.RailSet{
		"ccbill": {
			Type:         config.RailTypeCCBill,
			ClientAccNum: "945280",
			ClientSubAcc: "0000",
			Salt:         "test-salt",
		},
		"solana": {
			Type:            config.RailTypeSolana,
			RecipientWallet: "DzGLHdTfgHCYh8v3qNGJHn85CyX7aeFmqoUdVRBYkWMh",
			Tokens:          solanatokens.DefaultDevnetTokens(),
		},
		"mobius": {
			Type:            config.RailTypeNMI,
			SecurityKey:     envOrDefault("RAILS_MOBIUS_SECURITY_KEY", "6457Thfj624V5r7WUwc5v6a68Zsd6YEm"),
			TokenizationKey: envOrDefault("RAILS_MOBIUS_TOKENIZATION_KEY", ""),
			WebhookSecret:   envOrDefault("RAILS_MOBIUS_WEBHOOK_SECRET", ""),
		},
	}
	if suite.port != 0 {
		suite.Config.Port = config.FlexiblePort(suite.port)
	}

	// Initialize Redis connection
	suite.RedisClient = redis.NewClient(&redis.Options{
		Addr:     suite.Config.Redis.Addr,
		Password: suite.Config.Redis.Password,
		DB:       suite.Config.Redis.DB,
	})

	// Test Redis connection
	err = suite.RedisClient.Ping(suite.ctx).Err()
	require.NoError(suite.t, err)
}

func (suite *TestContainerSuite) prepareDatastores() {
	suite.t.Helper()

	err := migrate.RunClickHouse(suite.ctx, suite.Config)
	require.NoError(suite.t, err)

	// #336: no default merchant — seed the merchant this suite's engine binds to,
	// then configure it (slug) so ResolveMerchant pins it.
	seedPool, perr := pgxpool.New(suite.ctx, suite.Config.DB.URL)
	require.NoError(suite.t, perr)
	defer seedPool.Close()
	dbtest.EnsureTestMerchant(suite.ctx, suite.t, seedPool)
}

func envOrDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func firstNonEmptyEnv(keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
	}
	return ""
}

func (suite *TestContainerSuite) redisAddress() (string, error) {
	if addr := strings.TrimSpace(os.Getenv("OPENRAILS_TEST_REDIS_ADDR")); addr != "" {
		return addr, nil
	}
	if suite.redisContainer == nil {
		return "", fmt.Errorf("redis test container is not initialized")
	}
	host, err := suite.redisContainer.Host(suite.ctx)
	if err != nil {
		return "", err
	}
	port, err := suite.redisContainer.MappedPort(suite.ctx, "6379")
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s:%s", host, port.Port()), nil
}

func (suite *TestContainerSuite) clickhouseAddresses() (string, string, error) {
	if httpAddr := firstNonEmptyEnv("OPENRAILS_TEST_CH_HTTP_ADDR", "OPENRAILS_TEST_CH_HTTP_URL"); httpAddr != "" {
		return httpAddr, envOrDefault("OPENRAILS_TEST_CH_ADDR", envOrDefault("OPENRAILS_TEST_CH_NATIVE_ADDR", "localhost:9003")), nil
	}
	if suite.clickhouseContainer == nil {
		return "", "", fmt.Errorf("clickhouse test container is not initialized")
	}
	host, err := suite.clickhouseContainer.Host(suite.ctx)
	if err != nil {
		return "", "", err
	}
	httpPort, err := suite.clickhouseContainer.MappedPort(suite.ctx, "8123")
	if err != nil {
		return "", "", err
	}
	nativePort, err := suite.clickhouseContainer.MappedPort(suite.ctx, "9000")
	if err != nil {
		return "", "", err
	}
	return fmt.Sprintf("http://%s:%s", host, httpPort.Port()), fmt.Sprintf("%s:%s", host, nativePort.Port()), nil
}

// initializeServer starts the billing server for testing
func (suite *TestContainerSuite) initializeServer() {
	suite.t.Helper()

	// Bootstrap the application (creates runtime, cache, auth verifier, etc.)
	assembled, err := ginboot.NewServer(suite.Config, &bootstrap.Options{
		Clock:                  suite.clock,
		ConfiguredMerchant:     dbtest.TestMerchantID,
		Rails:                  suite.Rails,
		Authenticator:          suiteTestAuthenticator{},
		DelegatedAuthenticator: suiteTestDelegatedAuthenticator{},
	})
	require.NoError(suite.t, err)
	suite.App = assembled.App

	// Get the pgx pool from the app runtime
	suite.Pool = assembled.App.Runtime.DB.Pool()
	suite.Server = assembled.Server
	suite.seedProviderAccountFixtures()

	// Bootstrap the control plane exactly like the standalone serve path (#312):
	// ensure the test merchant's AuthKit org exists with the operator role holding
	// the concrete merchant-local org catalog, so admin identities created by the
	// test helpers carry LIVE merchant-local org authority. Idempotent; runs after
	// migrations (profiles.* + openrails.merchants exist). #336: bootstrap is
	// pinned to an explicit merchant slug (no default merchant).
	_, err = embcp.RunBootstrap(suite.ctx, suite.App, controlplane.BootstrapOptions{
		BootstrapOrgSlug:  dbtest.TestMerchantSlug,
		MintInitialAPIKey: false,
	})
	require.NoError(suite.t, err, "control plane bootstrap")

	// Start workers in-process for the integration suite (separate from HTTP server).
	workersCtx, cancel := context.WithCancel(context.Background())
	suite.workersCancel = cancel
	suite.workersErrCh = make(chan error, 1)
	go func() {
		suite.workersErrCh <- suite.App.Runtime.RunWorkers(workersCtx)
	}()

	// Wait for the River client: tests assume workers are up, and RunWorkers
	// initialises River asynchronously. (The bun-era double database init was
	// slow enough to mask this race; the pgx-only boot is faster and loses it.)
	riverDeadline := time.Now().Add(30 * time.Second)
	for suite.App.Runtime.RiverClient == nil {
		select {
		case werr := <-suite.workersErrCh:
			suite.workersErrCh <- werr
			require.NoError(suite.t, werr, "RunWorkers exited before River init")
			require.Fail(suite.t, "RunWorkers returned before River init")
		default:
		}
		require.True(suite.t, time.Now().Before(riverDeadline), "timed out waiting for River client")
		time.Sleep(50 * time.Millisecond)
	}

	// Create HTTP server
	httpServer := &http.Server{
		Handler: suite.Server.Handler(),
		Addr:    fmt.Sprintf("%s:%d", suite.Config.Host, suite.Config.Port),
	}

	// Start server in a goroutine
	go func() {
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			suite.t.Logf("Server failed to start: %v", err)
		}
	}()

	// Store the HTTP server for cleanup
	suite.httpServer = httpServer
	suite.ServerURL = fmt.Sprintf("http://localhost:%d", suite.Config.Port)
}

func (suite *TestContainerSuite) seedProviderAccountFixtures() {
	suite.t.Helper()
	ctx := dbtest.WithTestMerchant(context.Background())

	suite.seedProviderAccount(ctx, "nmi", testNMIProviderAccountID)
	nmiSecret, err := merchants.ProviderAccountSecretName("nmi", "live", testNMIProviderAccountID, "production_key")
	require.NoError(suite.t, err)
	_, err = suite.App.Runtime.Merchants.PutCredential(ctx, dbtest.TestMerchantID, nmiSecret, "test-security-key")
	require.NoError(suite.t, err)

	ccbillAccountID := "945280/0000"
	suite.seedProviderAccount(ctx, "ccbill", ccbillAccountID)
	ccbillSecret, err := merchants.ProviderAccountSecretName("ccbill", "live", ccbillAccountID, "account_config")
	require.NoError(suite.t, err)
	_, err = suite.App.Runtime.Merchants.PutCredential(ctx, dbtest.TestMerchantID, ccbillSecret, `{"client_acc_num":"945280","client_sub_acc":"0000","salt":"test-salt"}`)
	require.NoError(suite.t, err)
}

func (suite *TestContainerSuite) seedProviderAccount(ctx context.Context, providerType, accountID string) {
	suite.t.Helper()
	tx, err := suite.Pool.Begin(ctx)
	require.NoError(suite.t, err)
	defer tx.Rollback(ctx)

	_, err = tx.Exec(ctx, `
		UPDATE openrails.provider_accounts
		   SET role = 'legacy',
		       replaced_at = COALESCE(replaced_at, now()),
		       updated_at = now()
		 WHERE merchant_id = $1::uuid
		   AND provider_type = $2
		   AND environment = 'live'
		   AND account_id <> $3
		   AND role = 'primary'
		   AND status = 'enabled'
	`, dbtest.TestMerchantID.UUID(), providerType, accountID)
	require.NoError(suite.t, err)

	_, err = tx.Exec(ctx, `
		INSERT INTO openrails.provider_accounts
		    (merchant_id, provider_type, environment, account_id, role, status, evidence, last_verified_at)
		VALUES ($1::uuid, $2, 'live', $3, 'primary', 'enabled', '{"source":"test_fixture"}'::jsonb, now())
		ON CONFLICT (merchant_id, provider_type, environment, account_id) DO UPDATE
		   SET role = 'primary',
		       status = 'enabled',
		       evidence = EXCLUDED.evidence,
		       last_verified_at = EXCLUDED.last_verified_at,
		       updated_at = now()
	`, dbtest.TestMerchantID.UUID(), providerType, accountID)
	require.NoError(suite.t, err)
	require.NoError(suite.t, tx.Commit(ctx))
}

// waitForServerReady waits for the server to be ready to accept requests
func (suite *TestContainerSuite) waitForServerReady() {
	suite.t.Helper()

	// Wait for server to start
	maxRetries := 30
	for i := 0; i < maxRetries; i++ {
		resp, err := http.Get(suite.ServerURL + "/health/live")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}

	suite.t.Log("Server ready (or timeout reached)")
}

// Cleanup cleans up all test containers and resources
func (suite *TestContainerSuite) Cleanup() {
	suite.t.Helper()

	if suite.workersCancel != nil {
		suite.workersCancel()
	}
	if suite.workersErrCh != nil {
		select {
		case <-suite.workersErrCh:
		case <-time.After(2 * time.Second):
		}
	}

	// Stop HTTP server
	if suite.httpServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		suite.httpServer.Shutdown(ctx)
	}

	// Stop billing server
	if suite.Server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		suite.Server.Close(ctx)
	}

	// Close application (handles DB, Redis, cache, etc.)
	if suite.App != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		suite.App.Close(ctx)
	}

	if suite.redisContainer != nil {
		if err := suite.redisContainer.Terminate(suite.ctx); err != nil {
			suite.t.Logf("Failed to terminate redis container: %v", err)
		}
	}

	if suite.clickhouseContainer != nil {
		if err := suite.clickhouseContainer.Terminate(suite.ctx); err != nil {
			suite.t.Logf("Failed to terminate clickhouse container: %v", err)
		}
	}
}

// ExecuteSQL executes a SQL query on the test database ($1-style placeholders).
func (suite *TestContainerSuite) ExecuteSQL(query string, args ...interface{}) (pgconn.CommandTag, error) {
	return suite.Pool.Exec(suite.ctx, query, args...)
}

// ResetDatabase clears all data from test tables for clean test state
func (suite *TestContainerSuite) ResetDatabase() {
	suite.t.Helper()

	// List of tables to truncate (in dependency order)
	tables := []string{
		"openrails.subscriptions",
		"openrails.payments",
		"openrails.payment_methods",
		"openrails.prices",
		"openrails.products",
	}

	for _, table := range tables {
		_, err := suite.Pool.Exec(suite.ctx, fmt.Sprintf("TRUNCATE TABLE IF EXISTS %s CASCADE", table))
		if err != nil {
			// Log but don't fail - table might not exist
			suite.t.Logf("Failed to truncate table %s: %v", table, err)
		}
	}
}

// SetMockClock replaces the runtime's clock with a mock clock and returns the mock.
// This allows tests to control time for testing time-dependent logic.
// It also updates the clock on services that use time-dependent logic.
//
// Deprecated: new tests should prefer NewTestContainerSuite(t, WithSuiteClock(clock))
// or setupTestSuite(t, WithSuiteClock(clock)) so fake time is installed before
// runtime construction and seed data.
func (suite *TestContainerSuite) SetMockClock(t ...time.Time) *clockwork.FakeClock {
	suite.t.Helper()
	var clock *clockwork.FakeClock
	if len(t) > 0 {
		clock = clockwork.NewFakeClockAt(t[0])
	} else {
		// Default to a fixed time for reproducible tests
		clock = clockwork.NewFakeClockAt(time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC))
	}
	suite.setClock(clock)
	return clock
}

func (suite *TestContainerSuite) ResetMutableRuntimeState() {
	suite.t.Helper()
	suite.setClock(clockwork.NewRealClock())
	suite.resetNMIClients()
}

func (suite *TestContainerSuite) resetNMIClients() {
	suite.t.Helper()
	if suite == nil || suite.App == nil || suite.App.Runtime == nil {
		return
	}
	clients := make(map[string]*nmi.NMIClient)
	for name, proc := range suite.Rails.GetNMIRails() {
		settings := proc.ToNMIProviderSettings(name)
		client, err := nmi.NewClient(name, settings, suite.Config.IsTestMode())
		require.NoError(suite.t, err)
		client.ReadOnly = suite.Config.IsProviderReadOnly()
		clients[name] = client
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
}

func (suite *TestContainerSuite) setClock(clock clockwork.Clock) {
	if suite == nil || suite.App == nil || suite.App.Runtime == nil || clock == nil {
		return
	}
	suite.App.Runtime.Clock = clock
	// Set the clock on all services that use time-dependent logic
	rt := suite.App.Runtime

	// High priority services (core billing logic)
	if rt.SubscriptionLifecycleService != nil {
		rt.SubscriptionLifecycleService.SetClock(clock)
	}
	if rt.SubscriptionService != nil {
		rt.SubscriptionService.SetClock(clock)
	}
	if rt.EntitlementService != nil {
		rt.EntitlementService.SetClock(clock)
	}
	if rt.PaymentService != nil {
		rt.PaymentService.SetClock(clock)
	}
	if rt.MoneyService != nil {
		rt.MoneyService.SetClock(clock)
	}

	// Vault and payment method services
	if rt.VaultService != nil {
		rt.VaultService.SetClock(clock)
	}
	if rt.UserSubscriptionService != nil {
		rt.UserSubscriptionService.SetClock(clock)
	}
	if rt.AdminSubscriptionService != nil {
		rt.AdminSubscriptionService.SetClock(clock)
	}
	if rt.CheckoutService != nil {
		rt.CheckoutService.SetClock(clock)
	}
	if rt.CheckoutSessionService != nil {
		rt.CheckoutSessionService.SetClock(clock)
	}
	if rt.SolanaPayService != nil {
		rt.SolanaPayService.SetClock(clock)
	}
	if rt.SolanaTransactionService != nil {
		rt.SolanaTransactionService.SetClock(clock)
	}

	// Webhook services
	if rt.WebhookDispatcher != nil {
		rt.WebhookDispatcher.Clock = clock
	}

	// Email service (if it has a clock)
	if rt.EmailService != nil {
		rt.EmailService.SetClock(clock)
	}

	// Event log service (ClickHouse audit logging)
	if rt.EventLogService != nil {
		rt.EventLogService.SetClock(clock)
	}
}

// GetClock returns the current clock from the runtime (real or mock).
func (suite *TestContainerSuite) GetClock() clockwork.Clock {
	return suite.App.Runtime.Clock
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
	price, err := dbrepo.NewPriceRepo(suite.App.Runtime.DB).GetByID(suite.ctx, priceID)
	require.NoError(suite.t, err, "Failed to get price by ID")
	return price
}
