//go:build integration

package tests

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/doujins-org/doujins-billing/config"
	"github.com/doujins-org/doujins-billing/internal/app"
	"github.com/doujins-org/doujins-billing/internal/db/models"
	"github.com/doujins-org/doujins-billing/internal/migrate"
	"github.com/doujins-org/doujins-billing/internal/server"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	_ "github.com/lib/pq" // PostgreSQL driver for schema creation
	"github.com/redis/go-redis/v9"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/clickhouse"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	redismodule "github.com/testcontainers/testcontainers-go/modules/redis"
	"github.com/testcontainers/testcontainers-go/wait"
	"github.com/uptrace/bun"
)

// TestContainerSuite manages test containers for integration testing
type TestContainerSuite struct {
	t *testing.T

	// Containers
	postgresContainer   *postgres.PostgresContainer
	redisContainer      *redismodule.RedisContainer
	clickhouseContainer *clickhouse.ClickHouseContainer

	// Application and database connections
	App         *app.App
	BunDB       *bun.DB
	RedisClient *redis.Client

	// Server and configuration
	Server     *server.Server
	httpServer *http.Server
	Config     *config.Config
	ServerURL  string

	// Context for container operations
	ctx context.Context

	workersCancel context.CancelFunc
	workersErrCh  chan error
}

// NewTestContainerSuite creates a new test container suite
func NewTestContainerSuite(t *testing.T) *TestContainerSuite {
	suite := &TestContainerSuite{
		t:   t,
		ctx: context.Background(),
	}

	suite.SetupSuite()
	return suite
}

// SetupSuite initializes all test containers and services
func (suite *TestContainerSuite) SetupSuite() {
	suite.t.Helper()

	// Set log level to reduce noise during tests
	logrus.SetLevel(logrus.WarnLevel)

	// Start containers
	suite.startPostgresContainer()
	suite.startRedisContainer()
	suite.startClickHouseContainer()

	// Initialize config with container connection details
	suite.initializeDatabaseConnections()

	// Run database migrations (creates schema before app connects)
	suite.runDatabaseMigrations()

	// Initialize server (bootstraps the app and sets up DB connection)
	suite.initializeServer()

	// Wait for server to be ready
	suite.waitForServerReady()
}

// startPostgresContainer starts a PostgreSQL test container
func (suite *TestContainerSuite) startPostgresContainer() {
	suite.t.Helper()

	container, err := postgres.Run(suite.ctx,
		"postgres:15-alpine",
		postgres.WithDatabase("test_db"),
		postgres.WithUsername("test_user"),
		postgres.WithPassword("test_password"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(30*time.Second),
		),
	)
	require.NoError(suite.t, err)

	suite.postgresContainer = container
}

// startRedisContainer starts a Redis test container
func (suite *TestContainerSuite) startRedisContainer() {
	suite.t.Helper()

	container, err := redismodule.Run(suite.ctx,
		"redis:7-alpine",
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

	container, err := clickhouse.Run(suite.ctx,
		"clickhouse/clickhouse-server:23.8-alpine",
		clickhouse.WithUsername("test_user"),
		clickhouse.WithPassword("test_password"),
		clickhouse.WithDatabase("test_analytics"),
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

	// Get PostgreSQL connection string
	postgresConnStr, err := suite.postgresContainer.ConnectionString(suite.ctx, "sslmode=disable")
	require.NoError(suite.t, err)

	// Get Redis connection details
	redisHost, err := suite.redisContainer.Host(suite.ctx)
	require.NoError(suite.t, err)
	redisPort, err := suite.redisContainer.MappedPort(suite.ctx, "6379")
	require.NoError(suite.t, err)

	// Get ClickHouse connection details
	clickhouseHost, err := suite.clickhouseContainer.Host(suite.ctx)
	require.NoError(suite.t, err)
	clickhousePort, err := suite.clickhouseContainer.MappedPort(suite.ctx, "8123")
	require.NoError(suite.t, err)

	// Create configuration
	// Use "dev" to skip NMI/CCBill validation in config.Validate()
	// Use the JWKS server URL as the issuer so auth verification works
	jwksIssuer := GetTestIssuerURL()
	suite.Config = &config.Config{
		Env:  "dev",
		Host: "localhost",
		Port: 8080, // Fixed port for testing
		DB: &config.DBConfig{
			URL: postgresConnStr,
		},
		Redis: &config.RedisConfig{
			Addr:     fmt.Sprintf("%s:%s", redisHost, redisPort.Port()),
			Password: "",
			DB:       0,
		},
		ClickHouse: &config.ClickHouseConfig{
			HTTPAddr: fmt.Sprintf("http://%s:%s", clickhouseHost, clickhousePort.Port()),
			Database: "test_analytics",
			Username: "test_user",
			Password: "test_password",
		},
		Auth: &config.AuthConfig{
			Issuers:          []string{jwksIssuer},
			ExpectedAudience: "test-app",
		},
		// All payment processor configs use the unified Processors map
		Processors: map[string]*config.ProcessorConfig{
			// CCBill config with test_mode enabled to bypass IP verification in webhook tests
			"ccbill": {
				Type:         config.ProcessorTypeCCBill,
				ClientAccNum: "945280",
				ClientSubAcc: "0000",
				Salt:         "test-salt",
			},
			// Solana config for testing Solana payment endpoints
			"solana": {
				Type:            config.ProcessorTypeSolana,
				RecipientWallet: "DzGLHdTfgHCYh8v3qNGJHn85CyX7aeFmqoUdVRBYkWMh",
				SupportedTokens: config.DefaultDevnetTokens(),
				// RPCEndpoint and Network are derived from test_mode
			},
			// NMI demo account for real API integration tests
			// Uses the public NMI demo security key (test mode)
			// See: https://docs.nmi.com/
			"mobius": {
				Type:          config.ProcessorTypeNMI,
				SecurityKey:   "6457Thfj624V5r7WUwc5v6a68Zsd6YEm", // NMI demo key
				DirectPostURL: "https://secure.networkmerchants.com/api/transact.php",
				QueryURL:      "https://secure.networkmerchants.com/api/query.php",
			},
		},
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

// runDatabaseMigrations runs database migrations for testing
func (suite *TestContainerSuite) runDatabaseMigrations() {
	suite.t.Helper()

	// First, create the required schemas (normally done by bootstrap SQL)
	postgresConnStr, err := suite.postgresContainer.ConnectionString(suite.ctx, "sslmode=disable")
	require.NoError(suite.t, err)

	// Connect directly to create schemas
	sqlDB, err := sql.Open("postgres", postgresConnStr)
	require.NoError(suite.t, err)
	defer sqlDB.Close()

	// Create required schemas
	_, err = sqlDB.ExecContext(suite.ctx, `
		CREATE SCHEMA IF NOT EXISTS billing;
		CREATE SCHEMA IF NOT EXISTS profiles;
	`)
	require.NoError(suite.t, err)

	// Run all migrations using the migrate package
	err = migrate.RunPostgres(suite.ctx, suite.Config)
	require.NoError(suite.t, err)
}

// initializeServer starts the billing server for testing
func (suite *TestContainerSuite) initializeServer() {
	suite.t.Helper()

	// Bootstrap the application (creates runtime, cache, auth verifier, etc.)
	application, err := app.Bootstrap(suite.Config)
	require.NoError(suite.t, err)
	suite.App = application

	// Get the BunDB from the app runtime
	suite.BunDB = application.Runtime.DB.GetDB().(*bun.DB)

	// Create server with dependencies
	billingServer, err := server.New(server.Dependencies{
		Config:       suite.Config,
		Cache:        application.Cache,
		Runtime:      application.Runtime,
		Redis:        application.RedisClient,
		AuthProvider: application.AuthProvider,
	})
	require.NoError(suite.t, err)

	suite.Server = billingServer

	// Start workers in-process for the integration suite (separate from HTTP server).
	workersCtx, cancel := context.WithCancel(context.Background())
	suite.workersCancel = cancel
	suite.workersErrCh = make(chan error, 1)
	go func() {
		suite.workersErrCh <- suite.App.Runtime.RunWorkers(workersCtx)
	}()

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

	// Terminate containers
	if suite.postgresContainer != nil {
		if err := suite.postgresContainer.Terminate(suite.ctx); err != nil {
			suite.t.Logf("Failed to terminate postgres container: %v", err)
		}
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

// ExecuteSQL executes a SQL query on the test database
func (suite *TestContainerSuite) ExecuteSQL(query string, args ...interface{}) (sql.Result, error) {
	return suite.BunDB.ExecContext(suite.ctx, query, args...)
}

// ResetDatabase clears all data from test tables for clean test state
func (suite *TestContainerSuite) ResetDatabase() {
	suite.t.Helper()

	// List of tables to truncate (in dependency order)
	tables := []string{
		"billing.subscriptions",
		"billing.payments",
		"billing.payment_methods",
		"billing.prices",
		"billing.products",
	}

	for _, table := range tables {
		_, err := suite.BunDB.ExecContext(suite.ctx, fmt.Sprintf("TRUNCATE TABLE IF EXISTS %s CASCADE", table))
		if err != nil {
			// Log but don't fail - table might not exist
			suite.t.Logf("Failed to truncate table %s: %v", table, err)
		}
	}
}

// SetMockClock replaces the runtime's clock with a mock clock and returns the mock.
// This allows tests to control time for testing time-dependent logic.
// It also updates the clock on services that use time-dependent logic.
func (suite *TestContainerSuite) SetMockClock(t ...time.Time) *clockwork.FakeClock {
	suite.t.Helper()
	var mockClock *clockwork.FakeClock
	if len(t) > 0 {
		mockClock = clockwork.NewFakeClockAt(t[0])
	} else {
		// Default to a fixed time for reproducible tests
		mockClock = clockwork.NewFakeClockAt(time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC))
	}
	suite.App.Runtime.Clock = mockClock

	// Set the clock on all services that use time-dependent logic
	rt := suite.App.Runtime

	// High priority services (core billing logic)
	if rt.SubscriptionLifecycleService != nil {
		rt.SubscriptionLifecycleService.SetClock(mockClock)
	}
	if rt.SubscriptionService != nil {
		rt.SubscriptionService.Clock = mockClock
	}
	if rt.EntitlementService != nil {
		rt.EntitlementService.Clock = mockClock
	}
	if rt.PaymentService != nil {
		rt.PaymentService.Clock = mockClock
	}

	// Vault and payment method services
	if rt.VaultService != nil {
		rt.VaultService.Clock = mockClock
	}

	// Webhook services
	if rt.WebhookDispatcher != nil {
		rt.WebhookDispatcher.Clock = mockClock
	}

	// Email service (if it has a clock)
	if rt.EmailService != nil {
		rt.EmailService.Clock = mockClock
	}

	// Event log service (ClickHouse audit logging)
	if rt.EventLogService != nil {
		rt.EventLogService.Clock = mockClock
	}

	return mockClock
}

// GetClock returns the current clock from the runtime (real or mock).
func (suite *TestContainerSuite) GetClock() clockwork.Clock {
	return suite.App.Runtime.Clock
}

// GetRiverClient returns the River client for job enqueueing and inspection.
// Returns nil if River is not initialized.
func (suite *TestContainerSuite) GetRiverClient() interface{} {
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

	// Query the river_job table to check for completed jobs
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var count int
		err := suite.BunDB.QueryRowContext(suite.ctx,
			"SELECT COUNT(*) FROM billing.river_job WHERE state = 'completed'").Scan(&count)
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
	err := suite.BunDB.QueryRowContext(suite.ctx,
		"SELECT COUNT(*) FROM billing.river_job WHERE state = 'available'").Scan(&count)
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
	err := suite.BunDB.QueryRowContext(suite.ctx,
		"SELECT COUNT(*) FROM billing.river_job WHERE state = 'completed'").Scan(&count)
	if err != nil {
		suite.t.Logf("Error getting completed job count: %v", err)
		return 0
	}
	return count
}

// ClearJobQueue removes all jobs from the River queue for clean test state.
func (suite *TestContainerSuite) ClearJobQueue() {
	suite.t.Helper()
	_, err := suite.BunDB.ExecContext(suite.ctx, "DELETE FROM billing.river_job")
	if err != nil {
		suite.t.Logf("Error clearing job queue: %v", err)
	}
}

// GetPrice retrieves a price by ID from the database.
func (suite *TestContainerSuite) GetPrice(priceID uuid.UUID) *models.Price {
	suite.t.Helper()
	price := new(models.Price)
	err := suite.BunDB.NewSelect().Model(price).Where("id = ?", priceID).Scan(suite.ctx)
	require.NoError(suite.t, err, "Failed to get price by ID")
	return price
}
