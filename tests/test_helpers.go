//go:build integration

package tests

import (
	"bytes"
	"context"
	"io"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	authtesting "github.com/open-rails/authkit/testing"
	"github.com/stretchr/testify/require"

	server "github.com/open-rails/openrails/internal/http"
)

var (
	// Test issuer for auth verification (shared across tests)
	testIssuerOnce sync.Once
	testIssuer     *authtesting.TestIssuer

	// Shared test container suite for tests that need infra
	sharedSuiteOnce sync.Once
	sharedSuite     *TestContainerSuite
)

func init() {
	gin.SetMode(gin.TestMode)
}

// getTestIssuer returns a shared test issuer for authentication.
// The issuer provides a JWKS endpoint and can sign tokens.
func getTestIssuer() *authtesting.TestIssuer {
	testIssuerOnce.Do(func() {
		testIssuer = authtesting.NewTestIssuerWithAudience("test-app")
	})
	return testIssuer
}

// GetTestIssuerURL returns the URL of the test JWKS server to use as issuer.
// This is called by testcontainer_suite.go when configuring the auth verifier.
func GetTestIssuerURL() string {
	return getTestIssuer().URL()
}

// getSharedTestSuite returns a shared TestContainerSuite for integration tests.
// The suite is initialized once and reused across tests for performance.
func getSharedTestSuite(t *testing.T) *TestContainerSuite {
	sharedSuiteOnce.Do(func() {
		sharedSuite = NewTestContainerSuite(t)
	})
	// The suite is shared across tests; keep its t handle fresh to avoid panics
	// when helpers call require/assert via suite.t.
	sharedSuite.t = t
	return sharedSuite
}

// Helper function to log HTTP response for debugging
func logResponse(t *testing.T, w *httptest.ResponseRecorder, testName string) {
	t.Helper()
	body := w.Body.String()
	if body == "" {
		body = "(empty body)"
	}
	t.Logf("[%s]: Status=%d, Body=%s", testName, w.Code, body)
}

// setupTestServer creates a test server using testcontainers infrastructure.
// This requires the integration build tag and Docker to be available.
func setupTestServer(t *testing.T) *server.Server {
	suite := getSharedTestSuite(t)

	// Register cleanup only once at the end of all tests
	t.Cleanup(func() {
		// Don't cleanup the shared suite here - it will be cleaned up when all tests finish
		// The suite.Cleanup() should be called in TestMain or similar
	})

	return suite.Server
}

// setupTestSuite returns the shared test suite for tests that need direct database access.
// Use this when you need to seed data or query the database directly.
func setupTestSuite(t *testing.T) *TestContainerSuite {
	return getSharedTestSuite(t)
}

// setupTestServerWithAuth creates a test server with a valid JWT token.
// The token is signed by the test issuer and will validate against the JWKS endpoint.
func setupTestServerWithAuth(t *testing.T) (*server.Server, string) {
	srv := setupTestServer(t)
	token := getTestIssuer().CreateToken("test-user-billing-12345", "test@billing.openrails.com")
	return srv, token
}

// setupTestSuiteWithAuth returns the shared test suite with a valid JWT token and user ID.
// Use this when you need to seed data and make authenticated requests.
// The userID is a valid UUID string that can be used in database columns expecting UUID format.
func setupTestSuiteWithAuth(t *testing.T) (*TestContainerSuite, string, string) {
	suite := getSharedTestSuite(t)
	// Generate a valid UUID for the user ID (required by database schema)
	userID := uuid.New().String()
	email := "test-" + t.Name() + "@test.example.com"
	token := getTestIssuer().CreateToken(userID, email)
	return suite, token, userID
}

// setupTestServerWithRSAuth creates a test server with RS256-authenticated JWT token.
// This is the same as setupTestServerWithAuth since all tokens use RS256.
func setupTestServerWithRSAuth(t *testing.T) (*server.Server, string) {
	srv := setupTestServer(t)
	token := getTestIssuer().CreateToken("test-user-billing-rs256", "rs256@billing.openrails.com")
	return srv, token
}

// setupTestSuiteWithAdminAuth returns the shared test suite with an admin JWT token and user ID.
// Use this for testing admin endpoints that require the "admin" role.
func setupTestSuiteWithAdminAuth(t *testing.T) (*TestContainerSuite, string, string) {
	suite := getSharedTestSuite(t)
	userID := uuid.New().String()
	email := "admin-" + t.Name() + "@test.example.com"
	token := getTestIssuer().CreateTokenWithRoles(userID, email, []string{"admin"})

	// AdminRequired checks Postgres roles (profiles.user_roles), not JWT claims.
	// Seed the minimal role assignment so admin endpoints can be exercised.
	ctx := context.Background()
	_, err := suite.BunDB.ExecContext(ctx, "CREATE EXTENSION IF NOT EXISTS pgcrypto")
	require.NoError(t, err)
	_, err = suite.BunDB.ExecContext(ctx,
		"INSERT INTO profiles.users (id, email, email_verified, created_at, updated_at) VALUES (?::uuid, ?, true, NOW(), NOW()) ON CONFLICT (id) DO NOTHING",
		userID, email,
	)
	require.NoError(t, err)
	_, err = suite.BunDB.ExecContext(ctx,
		"INSERT INTO profiles.roles (slug, name) VALUES ('admin', 'Admin') ON CONFLICT (slug) DO NOTHING",
	)
	require.NoError(t, err)
	_, err = suite.BunDB.ExecContext(ctx,
		"INSERT INTO profiles.user_roles (user_id, role_id) VALUES (?::uuid, profiles.role_id('admin')) ON CONFLICT (user_id, role_id) DO NOTHING",
		userID,
	)
	require.NoError(t, err)

	return suite, token, userID
}

// CreateAdminToken creates a JWT token with admin role for the given user ID.
// Use this when you need an admin token for a specific user ID.
func CreateAdminToken(t *testing.T, userID string) string {
	t.Helper()
	email := "admin-" + userID[:8] + "@test.example.com"
	return getTestIssuer().CreateTokenWithRoles(userID, email, []string{"admin"})
}

// CreateUserToken creates a JWT token without admin role for the given user ID.
// Use this when you need a regular user token for a specific user ID.
func CreateUserToken(t *testing.T, userID string) string {
	t.Helper()
	email := "user-" + userID[:8] + "@test.example.com"
	return getTestIssuer().CreateToken(userID, email)
}

// CleanupSharedSuite should be called at the end of all tests to cleanup containers.
func CleanupSharedSuite() {
	if sharedSuite != nil {
		sharedSuite.Server.Close(context.Background())
		sharedSuite.App.Close(context.Background())
		sharedSuite.Cleanup()
	}
	if testIssuer != nil {
		testIssuer.Close()
	}
}

// newRequestBody creates an io.ReadCloser from a byte slice for HTTP request bodies.
func newRequestBody(data []byte) io.ReadCloser {
	return io.NopCloser(bytes.NewReader(data))
}
