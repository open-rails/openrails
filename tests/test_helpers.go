//go:build integration

package tests

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	authtesting "github.com/open-rails/authkit/authtest"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/dbtest"
	server "github.com/open-rails/openrails/internal/http"
	"github.com/open-rails/openrails/pkg/billingauth"
)

var (
	// Test issuer for auth verification (shared across tests)
	testIssuerOnce sync.Once
	testIssuer     *authtesting.TestIssuer

	// Shared test container suite for tests that need infra
	sharedSuiteOnce sync.Once
	sharedSuite     *TestContainerSuite
)

// personalOwnerID returns the merchant-subject id for the self-hosted /
// single-merchant personal case. HARDCUT (#221): this matches the caller-provided
// payable subject id, never a synthesized stand-in. Test user ids are UUIDs, so
// the subject is simply the parsed value.
func personalOwnerID(userID string) uuid.UUID {
	return uuid.MustParse(userID)
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

type testTokenClaims struct {
	Subject  string `json:"sub"`
	Email    string `json:"email"`
	Username string `json:"username"`
	Expires  int64  `json:"exp"`
}

func parseTestBearer(r *http.Request) (testTokenClaims, error) {
	if r == nil {
		return testTokenClaims{}, billingauth.ErrUnauthenticated
	}
	header := strings.TrimSpace(r.Header.Get("Authorization"))
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return testTokenClaims{}, billingauth.ErrUnauthenticated
	}
	parts := strings.Split(strings.TrimSpace(header[len(prefix):]), ".")
	if len(parts) != 3 {
		return testTokenClaims{}, errors.New("invalid_token")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return testTokenClaims{}, errors.New("invalid_token")
	}
	var claims testTokenClaims
	if err := json.Unmarshal(raw, &claims); err != nil {
		return testTokenClaims{}, errors.New("invalid_token")
	}
	if claims.Subject == "" {
		return testTokenClaims{}, errors.New("invalid_token")
	}
	if claims.Expires > 0 && time.Now().Unix() >= claims.Expires {
		return testTokenClaims{}, errors.New("invalid_token")
	}
	return claims, nil
}

type suiteTestAuthenticator struct{}

func (suiteTestAuthenticator) Authenticate(_ context.Context, r *http.Request) (billingauth.UserContext, error) {
	claims, err := parseTestBearer(r)
	if err != nil {
		return billingauth.UserContext{}, err
	}
	return billingauth.UserContext{
		UserID:   claims.Subject,
		Email:    claims.Email,
		Username: claims.Username,
	}, nil
}

type suiteTestDelegatedAuthenticator struct{}

func (suiteTestDelegatedAuthenticator) AuthenticateDelegated(_ context.Context, r *http.Request) (*billingauth.DelegatedPrincipal, error) {
	claims, err := parseTestBearer(r)
	if err != nil {
		return nil, err
	}
	return &billingauth.DelegatedPrincipal{
		MerchantID:   dbtest.TestMerchantID.String(),
		MerchantSlug: dbtest.TestMerchantSlug,
		SubjectID:    claims.Subject,
		Issuer:       getTestIssuer().URL(),
		Email:        claims.Email,
		Username:     claims.Username,
	}, nil
}

// getSharedTestSuite returns a shared TestContainerSuite for integration tests.
// The suite is initialized once and reused across tests for performance.
func getSharedTestSuite(t *testing.T) *TestContainerSuite {
	sharedSuiteOnce.Do(func() {
		sharedSuite = NewTestContainerSuite(t)
	})
	if sharedSuite == nil {
		t.Fatalf("shared integration suite failed to initialize in an earlier test; inspect the first setup failure")
	}
	// The suite is shared across tests; keep its t handle fresh to avoid panics
	// when helpers call require/assert via suite.t.
	sharedSuite.t = t
	sharedSuite.ResetMutableRuntimeState()
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
func setupTestSuite(t *testing.T, opts ...TestSuiteOption) *TestContainerSuite {
	opts = append([]TestSuiteOption{WithSuitePort(freeTestPort(t))}, opts...)
	suite := NewTestContainerSuite(t, opts...)
	t.Cleanup(suite.Cleanup)
	return suite
}

func freeTestPort(t *testing.T) int {
	t.Helper()
	for port := 20000; port <= 32767; port++ {
		listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", fmt.Sprintf("%d", port)))
		if err != nil {
			continue
		}
		require.NoError(t, listener.Close())
		return port
	}
	t.Fatal("failed to allocate a test port compatible with FlexiblePort")
	return 0
}

// setupTestServerWithAuth creates a test server with a valid JWT token.
// The token is signed by the test issuer and will validate against the JWKS
// endpoint. The subject is a fixed UUID — payable identities are UUID-only
// (#364) and the auth boundary rejects anything else.
func setupTestServerWithAuth(t *testing.T) (*server.Server, string) {
	srv := setupTestServer(t)
	token := getTestIssuer().CreateToken("b1111111-1111-4111-8111-111111111111", "test@openrails.openrails.com")
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
	token := getTestIssuer().CreateToken("b2222222-2222-4222-8222-222222222222", "rs256@openrails.openrails.com")
	return srv, token
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
