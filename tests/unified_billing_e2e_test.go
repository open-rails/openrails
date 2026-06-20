//go:build integration

package tests

// Unified billing e2e harness (issue #244, OpenRails side) — #513/#512 model.
//
// Issue #244 asks for an end-to-end test of the unified billing money path
// across gen-orchestrator -> Tensorhub -> embedded OpenRails. The full
// three-service live flow lives in the embedding repo; what we prove HERE is
// that OpenRails' own standalone surfaces implement the unified money lifecycle
// correctly: ADMIT for an estimate (places a Redis spendgate hold), CAPTURE the
// metered actual (full and PARTIAL), RELEASE on failure, REJECT when in-flight
// holds exhaust the spendable balance, dedupe on replay, and scope every
// operation to a tenant subject.
//
// POST-#513/#512 MODEL (what changed from the original single-entry version):
//   - There is NO /credits/hold create route anymore: placing a hold IS
//     POST /v1/service/admit (the spendgate). The hold handle is the caller's
//     request_id; capture/release address it as /credits/holds/<request_id>/...
//   - Holds live in Redis (#505), NOT as money_transactions rows. The durable
//     ledger (#512: ledger_transfers/ledger_accounts) records only SETTLED money
//     (deposits + captured spend). So "held" is observed through ADMIT GATING (a
//     follow-up admit is denied while a hold is in flight, allowed once it is
//     captured/released), never through a "hold" ledger row. The public balance
//     endpoint reports held=0 because durable held was removed in the hard cut.
//   - Balance/conservation are asserted against the #512 ledger via the money
//     service (GetBalanceForCustomer / GetTransactionsByCustomer), not the
//     dropped money_transactions/money_blocks tables.
//
// Everything flows over the service-token-authenticated PUBLIC service routes
// (deposit, admit, capture, release, balance) — the standalone server-to-server
// contract gen-orchestrator / Tensorhub actually use.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	authcore "github.com/open-rails/authkit/core"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	ginmw "github.com/open-rails/openrails/internal/http/middleware/ginmw"
	httproutes "github.com/open-rails/openrails/internal/http/routes/ginroutes"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/identity"
)

// billingE2EHarness is a small reusable driver over the service-token-authenticated public
// service routes. It mirrors the harness style already established in
// service_facade_parity_test.go (stubServiceTokenResolver + RegisterServiceRoutes) so
// the same server-to-server contract gen-orchestrator / Tensorhub use is exercised here.
type billingE2EHarness struct {
	t          *testing.T
	suite      *TestContainerSuite
	router     *gin.Engine
	creditType string
	// ms + ctx read durable money state from the #512 ledger (the dropped
	// money_transactions/money_blocks tables are gone).
	ms  *money.MoneyService
	ctx context.Context
}

func newBillingE2EHarness(t *testing.T, suite *TestContainerSuite) *billingE2EHarness {
	t.Helper()
	require.NotNil(t, suite.App.Runtime.RedisClient, "spendgate admit needs Redis")

	// credit_type is accepted-and-ignored on the wire; money needs no type row.
	creditTypeName := "e2e_credits_" + uuid.NewString()

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(ginmw.ResolveMerchant(dbtest.TestMerchantID))
	group := router.Group("/v1/service")
	// Merchant-wide token (no CustomerResource): AllowsCustomer returns true for
	// every customer under the merchant, so each test's random payer is in scope.
	resolver := stubServiceTokenResolver{permissions: []string{
		controlplane.PermCreditsRead,
		controlplane.PermCreditsWrite,
		controlplane.PermCreditsSpend,
	}, resources: []authcore.APIKeyResource{
		controlplane.MerchantResource(dbtest.TestMerchantID),
	}}
	httproutes.RegisterServiceRoutes(group, suite.App.Runtime, ginmw.ServiceTokenRequired(resolver))

	return &billingE2EHarness{
		t:          t,
		suite:      suite,
		router:     router,
		creditType: creditTypeName,
		ms:         money.NewMoneyService(suite.App.Runtime.DB),
		ctx:        dbtest.WithTestMerchant(context.Background()),
	}
}

func (h *billingE2EHarness) do(method, path string, body any) *httptest.ResponseRecorder {
	h.t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(h.t, err)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader([]byte("{}"))
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Authorization", "Bearer openrails_st_testkeyid_testsecret")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, req)
	return w
}

// deposit funds a tenant subject's balance via the public deposit route (a #512
// ledger deposit; the deposit path ensures the customer row).
func (h *billingE2EHarness) deposit(userID string, amount int64) {
	h.t.Helper()
	srcID := uuid.New()
	w := h.do(http.MethodPost, "/v1/service/credits/deposit", map[string]any{
		"customer_id": personalOwnerID(userID).String(),
		"invoker":     userID,
		"credit_type": h.creditType,
		"amount":      amount,
		"source":      "e2e_deposit",
		"source_id":   srcID.String(),
	})
	require.Equal(h.t, http.StatusOK, w.Code, "deposit body: %s", w.Body.String())
}

type admitView struct {
	Allowed   bool   `json:"allowed"`
	BlockedBy string `json:"blocked_by"`
	DenyCode  string `json:"deny_code"`
}

// admit places a spendgate hold for an estimate via POST /v1/service/admit. The
// request_id IS the hold handle (capture/release address it). Returns the
// recorder so callers can assert allow (200) vs money-deny (402).
func (h *billingE2EHarness) admit(userID, source, requestID string, amount int64) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.do(http.MethodPost, "/v1/service/admit", map[string]any{
		"customer_id":      personalOwnerID(userID).String(),
		"invoker":          userID,
		"invoker_type":     "payer",
		"currency":         money.DefaultCurrency,
		"estimated_amount": amount,
		"request_id":       requestID,
		"source":           source,
		"expires_at":       time.Now().Add(15 * time.Minute).Unix(),
	})
}

// mustAdmit admits and asserts the hold was placed (200, allowed=true). Returns
// the request_id, which is the capture/release handle.
func (h *billingE2EHarness) mustAdmit(userID, source, requestID string, amount int64) string {
	h.t.Helper()
	w := h.admit(userID, source, requestID, amount)
	require.Equal(h.t, http.StatusOK, w.Code, "admit body: %s", w.Body.String())
	var a admitView
	require.NoError(h.t, json.Unmarshal(w.Body.Bytes(), &a))
	require.True(h.t, a.Allowed, "admit must be allowed: %s", w.Body.String())
	return requestID
}

func (h *billingE2EHarness) capture(requestID string, amount int64) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.do(http.MethodPost, "/v1/service/credits/holds/"+requestID+"/capture", map[string]any{
		"amount": amount,
	})
}

func (h *billingE2EHarness) release(requestID string) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.do(http.MethodPost, "/v1/service/credits/holds/"+requestID+"/release", nil)
}

// balance reads available + held via the public balance route. NOTE: under #513
// held is Redis-only and not surfaced here, so HeldBalance is always 0 — assert
// in-flight holds via admit gating instead.
type balanceView struct {
	Balance     int64
	HeldBalance int64
}

func (h *billingE2EHarness) balance(userID string) balanceView {
	h.t.Helper()
	w := h.do(http.MethodGet, "/v1/service/credits/balance?customer_id="+personalOwnerID(userID).String()+"&currency="+money.DefaultCurrency, nil)
	require.Equal(h.t, http.StatusOK, w.Code, "balance body: %s", w.Body.String())
	var payload struct {
		BalanceAmount int64 `json:"balance_amount"`
		HeldAmount    int64 `json:"held_amount"`
	}
	require.NoError(h.t, json.Unmarshal(w.Body.Bytes(), &payload))
	return balanceView{Balance: payload.BalanceAmount, HeldBalance: payload.HeldAmount}
}

// ledgerTxns returns the durable #512 ledger transfers for a tenant subject
// (USD) as MoneyTransaction DTOs — deposits and captured withdrawals; there are
// no "hold" rows (holds are Redis-only).
func (h *billingE2EHarness) ledgerTxns(userID string) []models.MoneyTransaction {
	h.t.Helper()
	payer := identity.CustomerID(personalOwnerID(userID))
	rows, _, err := h.ms.GetTransactionsByCustomer(h.ctx, payer, money.DefaultCurrency, 200, 0)
	require.NoError(h.t, err)
	return rows
}

// ledgerBalance returns the DERIVED balance from the #512 ledger account.
func (h *billingE2EHarness) ledgerBalance(userID string) int64 {
	h.t.Helper()
	payer := identity.CustomerID(personalOwnerID(userID))
	bal, err := h.ms.GetBalanceForCustomer(h.ctx, payer, money.DefaultCurrency)
	require.NoError(h.t, err)
	return bal.Balance
}

// sumLedgerAmounts sums the signed transfer amounts (deposit positive, capture
// negative) — equals the derived balance when conserved.
func (h *billingE2EHarness) sumLedgerAmounts(userID string) int64 {
	h.t.Helper()
	var total int64
	for _, r := range h.ledgerTxns(userID) {
		total += r.Amount
	}
	return total
}

func countByType(rows []models.MoneyTransaction, txType string) int {
	n := 0
	for _, r := range rows {
		if r.TransactionType == txType {
			n++
		}
	}
	return n
}

// --- Scenarios ---------------------------------------------------------------

// TestUnifiedBilling_PrepaidHoldCapture_Full: deposit -> admit an estimate ->
// capture the full estimate. The durable balance is debited only at capture, by
// the full actual; the capture is a single withdrawal transfer.
func TestUnifiedBilling_PrepaidHoldCapture_Full(t *testing.T) {
	suite := getSharedTestSuite(t)
	h := newBillingE2EHarness(t, suite)
	user := uuid.NewString()

	h.deposit(user, 10_000)
	require.Equal(t, int64(10_000), h.ledgerBalance(user))

	req := h.mustAdmit(user, "gen_job", "job-full-1", 3_000)
	// The hold reserves in Redis but does not debit: durable balance unchanged.
	require.Equal(t, int64(10_000), h.ledgerBalance(user))

	w := h.capture(req, 3_000)
	require.Equal(t, http.StatusOK, w.Code, "capture body: %s", w.Body.String())

	require.Equal(t, int64(7_000), h.ledgerBalance(user))
	require.Equal(t, int64(7_000), h.sumLedgerAmounts(user))
	rows := h.ledgerTxns(user)
	require.Equal(t, 1, countByType(rows, "deposit"), "the seed deposit")
	require.Equal(t, 1, countByType(rows, "withdrawal"), "one capture withdrawal, no hold rows")
}

// TestUnifiedBilling_PrepaidHoldCapture_Partial: the core money-path scenario.
// Deposit, admit a generous estimate, then capture LESS than the estimate (the
// metered actual). Only the actual is debited — the held remainder simply frees
// (Redis), it is never debited — and total credits are conserved.
func TestUnifiedBilling_PrepaidHoldCapture_Partial(t *testing.T) {
	suite := getSharedTestSuite(t)
	h := newBillingE2EHarness(t, suite)
	user := uuid.NewString()

	h.deposit(user, 10_000)

	const estimate = 4_000
	const actual = 1_500
	req := h.mustAdmit(user, "gen_job", "job-partial-1", estimate)

	w := h.capture(req, actual)
	require.Equal(t, http.StatusOK, w.Code, "capture body: %s", w.Body.String())

	// Only the actual is debited; conservation: deposit(+10000) + capture(-1500).
	require.Equal(t, int64(10_000-actual), h.ledgerBalance(user))
	require.Equal(t, int64(10_000-actual), h.sumLedgerAmounts(user))
	require.Equal(t, 1, countByType(h.ledgerTxns(user), "withdrawal"))
}

// TestUnifiedBilling_InsufficientBalance: admitting beyond available is rejected
// cleanly (402 on the money axis) and the balance is untouched.
func TestUnifiedBilling_InsufficientBalance(t *testing.T) {
	suite := getSharedTestSuite(t)
	h := newBillingE2EHarness(t, suite)
	user := uuid.NewString()

	h.deposit(user, 1_000)

	w := h.admit(user, "gen_job", "job-overdraw-1", 5_000)
	require.Equal(t, http.StatusPaymentRequired, w.Code, "body: %s", w.Body.String())
	var a admitView
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &a))
	require.False(t, a.Allowed)
	require.Equal(t, "money", a.BlockedBy, "denied on the money axis")

	// Balance fully intact, nothing debited (no capture transfer written).
	require.Equal(t, int64(1_000), h.ledgerBalance(user))
	require.Equal(t, 0, countByType(h.ledgerTxns(user), "withdrawal"))
}

// TestUnifiedBilling_FailureRelease: admit then release (worker failed / zero
// usage). Nothing is debited and the hold is freed — proven by a fresh admit for
// the entire balance succeeding.
func TestUnifiedBilling_FailureRelease(t *testing.T) {
	suite := getSharedTestSuite(t)
	h := newBillingE2EHarness(t, suite)
	user := uuid.NewString()

	h.deposit(user, 8_000)
	req := h.mustAdmit(user, "gen_job", "job-fail-1", 2_500)

	w := h.release(req)
	require.Equal(t, http.StatusOK, w.Code, "release body: %s", w.Body.String())

	require.Equal(t, int64(8_000), h.ledgerBalance(user))
	require.Equal(t, 0, countByType(h.ledgerTxns(user), "withdrawal"), "release debits nothing")
	require.Equal(t, http.StatusOK, h.admit(user, "gen_job", "job-fail-2", 8_000).Code,
		"release freed the hold, so the full balance is admittable again")
}

// TestUnifiedBilling_Idempotency_HoldReplay: replaying the same request_id is an
// idempotent no-op (the spendgate hold is not doubled).
func TestUnifiedBilling_Idempotency_HoldReplay(t *testing.T) {
	suite := getSharedTestSuite(t)
	h := newBillingE2EHarness(t, suite)
	user := uuid.NewString()

	h.deposit(user, 10_000)

	req := h.mustAdmit(user, "gen_job", "job-idem-1", 3_000)
	// Replay with the identical request_id: same handle, hold NOT doubled.
	require.Equal(t, req, h.mustAdmit(user, "gen_job", "job-idem-1", 3_000))

	// Proof the hold was not doubled: only 3_000 is held, so 7_000 is still
	// admittable (a doubled 6_000 hold would deny this). Release the probe after.
	require.Equal(t, http.StatusOK, h.admit(user, "gen_job", "job-idem-probe", 7_000).Code,
		"replay must not double the hold")
	require.Equal(t, http.StatusOK, h.release("job-idem-probe").Code)

	// Capturing the original once debits once.
	w := h.capture(req, 3_000)
	require.Equal(t, http.StatusOK, w.Code, "capture body: %s", w.Body.String())
	require.Equal(t, int64(7_000), h.ledgerBalance(user))
	require.Equal(t, 1, countByType(h.ledgerTxns(user), "withdrawal"))
}

// TestUnifiedBilling_OwnerScoping: owner A's admit/capture never touches owner B.
// Owners here are the deterministic personal orgs of two distinct user ids,
// which is exactly how the service routes scope (resolveOwner(nil, userID)).
func TestUnifiedBilling_OwnerScoping(t *testing.T) {
	suite := getSharedTestSuite(t)
	h := newBillingE2EHarness(t, suite)
	ownerA := uuid.NewString()
	ownerB := uuid.NewString()

	h.deposit(ownerA, 5_000)
	h.deposit(ownerB, 9_000)

	// request_ids are merchant-global (the spendgate keys holds by request_id),
	// so each owner uses its own id; balances are independently scoped.
	reqA := h.mustAdmit(ownerA, "gen_job", "job-A-1", 2_000)
	reqB := h.mustAdmit(ownerB, "gen_job", "job-B-1", 6_000)
	require.NotEqual(t, reqA, reqB)

	// Capture A only. B is completely unaffected.
	w := h.capture(reqA, 2_000)
	require.Equal(t, http.StatusOK, w.Code, "capture body: %s", w.Body.String())

	require.Equal(t, int64(3_000), h.ledgerBalance(ownerA))
	require.Equal(t, int64(9_000), h.ledgerBalance(ownerB), "owner B untouched by owner A's capture")

	// Each owner's ledger holds only its own customer's transfers.
	for _, r := range h.ledgerTxns(ownerA) {
		require.Equal(t, personalOwnerID(ownerA), r.CustomerID)
	}
	for _, r := range h.ledgerTxns(ownerB) {
		require.Equal(t, personalOwnerID(ownerB), r.CustomerID)
	}
}

// TestUnifiedBilling_LifecycleViaPublicServiceTokenRoutes is the end-to-end
// "happy path" a gen-orchestrator / Tensorhub driver runs for a single job, all
// over the service-token-authenticated public surface: fund -> admit estimate ->
// capture actual -> ledger conserved, with the public balance endpoint agreeing.
func TestUnifiedBilling_LifecycleViaPublicServiceTokenRoutes(t *testing.T) {
	suite := getSharedTestSuite(t)
	h := newBillingE2EHarness(t, suite)
	user := uuid.NewString()

	h.deposit(user, 20_000)
	req := h.mustAdmit(user, "gen_job", "lifecycle-job-1", 5_000)

	// Metered actual comes in under the estimate -> partial capture.
	w := h.capture(req, 3_200)
	require.Equal(t, http.StatusOK, w.Code, "capture body: %s", w.Body.String())

	require.Equal(t, int64(16_800), h.ledgerBalance(user))
	require.Equal(t, int64(16_800), h.sumLedgerAmounts(user))

	// The public balance endpoint agrees with the ledger; held is Redis-only so
	// the endpoint reports 0.
	bal := h.balance(user)
	require.Equal(t, int64(16_800), bal.Balance, "balance endpoint reflects the ledger")
	require.Equal(t, int64(0), bal.HeldBalance, "held is Redis-only; the endpoint reports 0")
}
