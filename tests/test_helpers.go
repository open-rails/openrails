//go:build integration

package tests

import (
	"bytes"
	"io"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/config"
	server "github.com/open-rails/openrails/internal/http"
	"github.com/open-rails/openrails/internal/integrations/ccbill"
)

var (
	// Package-shared suite: booted once, reused by every test that has no
	// construction-time divergence (#694 phase 2 — the pooled harness).
	sharedSuiteOnce sync.Once
	sharedSuite     *TestContainerSuite
)

func testCCBillWebhookClient() *ccbill.RESTClient {
	return ccbill.NewRESTClient(&config.CCBillConfig{
		ClientAccNum: "1234",
		ClientSubAcc: "0000",
	})
}

// personalOwnerID returns the merchant-subject id for the self-hosted /
// single-merchant personal case. HARDCUT (#221): this matches the caller-provided
// payable subject id, never a synthesized stand-in. Test user ids are UUIDs, so
// the subject is simply the parsed value.
func personalOwnerID(userID string) uuid.UUID {
	return uuid.MustParse(userID)
}

// getSharedTestSuite returns the shared TestContainerSuite, booting it on first
// use. The suite's testing handle is refreshed per test, real time restored,
// and the NMI clients re-derived (tests mutate them).
func getSharedTestSuite(t *testing.T) *TestContainerSuite {
	sharedSuiteOnce.Do(func() {
		sharedSuite = newPersistentSuite(t)
	})
	if sharedSuite == nil {
		t.Fatalf("shared integration suite failed to initialize in an earlier test; inspect the first setup failure")
	}
	// The suite is shared across tests; keep its t handle fresh so helpers'
	// require/assert calls fail the CURRENT test.
	sharedSuite.t = t
	sharedSuite.harness.SetT(t)
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

// setupTestServer returns the shared suite's real standalone server.
func setupTestServer(t *testing.T) *server.Server {
	return getSharedTestSuite(t).Server
}

// setupTestSuite returns a suite for tests that need direct database access.
// Construction-time divergence (Stripe rail) boots a FRESH suite
// bound to t; everything else — including clock control, which is a per-test
// swap on the injectable SettableClock — pools onto the shared suite.
func setupTestSuite(t *testing.T, opts ...TestSuiteOption) *TestContainerSuite {
	probe := &TestContainerSuite{}
	for _, opt := range opts {
		if opt != nil {
			opt(probe)
		}
	}
	if probe.stripeSecretKey != "" {
		return NewTestContainerSuite(t, opts...)
	}
	suite := getSharedTestSuite(t)
	if probe.initialClock != nil {
		suite.clock.Set(probe.initialClock)
		t.Cleanup(func() { suite.clock.Set(nil) })
	}
	return suite
}

// setupTestServerWithAuth returns the shared server plus a REAL minted
// delegated access token. The subject is a fixed UUID — payable identities are
// UUID-only (#364) and the auth boundary rejects anything else.
func setupTestServerWithAuth(t *testing.T) (*server.Server, string) {
	suite := getSharedTestSuite(t)
	token := suite.MintUserToken("b1111111-1111-4111-8111-111111111111", "test@openrails.openrails.com")
	return suite.Server, token
}

// setupTestSuiteWithAuth returns the shared test suite with a real minted token
// and its user ID. Use this when you need to seed data and make authenticated
// requests. The userID is a valid UUID string usable in UUID columns.
func setupTestSuiteWithAuth(t *testing.T) (*TestContainerSuite, string, string) {
	suite := getSharedTestSuite(t)
	userID := uuid.New().String()
	email := "test-" + t.Name() + "@test.example.com"
	token := suite.MintUserToken(userID, email)
	return suite, token, userID
}

// CleanupSharedSuite should be called at the end of all tests (TestMain) to
// tear the shared suite down.
func CleanupSharedSuite() {
	if sharedSuite != nil {
		sharedSuite.Cleanup()
	}
}

// newRequestBody creates an io.ReadCloser from a byte slice for HTTP request bodies.
func newRequestBody(data []byte) io.ReadCloser {
	return io.NopCloser(bytes.NewReader(data))
}
