//go:build integration

package tests

// Unified billing e2e harness (issue #244, OpenRails side).
//
// Issue #244 asks for an end-to-end test of the unified billing money path
// across gen-orchestrator -> Tensorhub -> embedded OpenRails. The full
// three-service live flow cannot run inside this single repo: the
// gen-orchestrator dispatch loop and the Tensorhub metering driver live in
// other repos and the issue itself notes the suite "lives where the embedding
// is (tensorhub) or as a cross-repo script". What we CAN (and must) prove here
// is that OpenRails' own surfaces -- the exact contract gen-orchestrator /
// Tensorhub call standalone -- implement the unified credit lifecycle
// correctly: hold for an estimate, capture the actual (full and PARTIAL),
// release on failure, reject on insufficient balance, dedupe on replay, and
// scope every operation to a tenant subject.
//
// HOST'S REMAINING PART (cross-service live wiring, intentionally NOT here):
//   - The gen-orchestrator driver that, on job submit, calls OpenRails
//     /v1/service/credits/hold for the price estimate, dispatches the worker,
//     and on completion calls .../holds/:id/capture with the metered actual
//     (or .../holds/:id/release on failure / zero usage).
//   - The Tensorhub metering path that turns model output (per_output,
//     per_output_second, per_million_tokens, tiered) into the captured actual.
//   - Stripe-test-mode auto-top-up, spend caps, expiry/arrears workers driven
//     end to end from a real job. Those exercise OpenRails subsystems that have
//     their own integration coverage in this repo; #244's cross-service driver
//     is what stitches them to a live job and belongs in the embedding repo.
//
// This file drives the lifecycle through the service-token-authenticated PUBLIC service
// routes (issue #222, /v1/service/credits/*) wherever a public route exists --
// deposit, hold, capture, release, get-credits, transaction lookup -- because
// that is the standalone server-to-server contract gen-orchestrator / Tensorhub
// actually use. Ledger correctness (rows, statuses, balances, conservation) is
// then asserted directly against the openrails.money_transactions /
// money_balances tables. We fall back to the in-process facade only where
// no public route exists; every scenario below has one, so all flow over HTTP.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
}

func newBillingE2EHarness(t *testing.T, suite *TestContainerSuite) *billingE2EHarness {
	t.Helper()

	// credit_type is accepted-and-ignored on the wire; money needs no type row.
	creditTypeName := "e2e_credits_" + uuid.NewString()

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(ginmw.ResolveTenant(dbtest.TestTenantID))
	group := router.Group("/v1/service")
	resolver := stubServiceTokenResolver{permissions: []string{
		controlplane.PermCreditsRead,
		controlplane.PermCreditsWrite,
		controlplane.PermCreditsSpend,
	}, resources: []authcore.ServiceTokenResource{
		controlplane.MerchantResource(dbtest.TestTenantID),
	}}
	// nil issuer-admin: the delegated issuer routes are
	// irrelevant to the money path.
	httproutes.RegisterServiceRoutes(group, suite.App.Runtime, ginmw.ServiceTokenRequired(resolver))

	return &billingE2EHarness{
		t:          t,
		suite:      suite,
		router:     router,
		creditType: creditTypeName,
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
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Authorization", "Bearer openrails_st_testkeyid_testsecret")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, req)
	return w
}

// deposit funds a tenant subject's balance via the public deposit route.
func (h *billingE2EHarness) deposit(userID string, amount int64) {
	h.t.Helper()
	srcID := uuid.New()
	w := h.do(http.MethodPost, "/v1/service/credits/deposit", map[string]any{
		"merchant_subject_id": personalOwnerID(userID).String(),
		"actor":             userID,
		"credit_type":       h.creditType,
		"amount":            amount,
		"source":            "e2e_deposit",
		"source_id":         srcID.String(),
	})
	require.Equal(h.t, http.StatusOK, w.Code, "deposit body: %s", w.Body.String())
}

// holdResult is the parsed id of a created hold.
type holdResult struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// hold reserves credits for an estimate. Returns the recorder so callers can
// assert on rejection (insufficient balance) too.
func (h *billingE2EHarness) hold(userID, source, sourceID string, amount int64) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.do(http.MethodPost, "/v1/service/credits/hold", map[string]any{
		"merchant_subject_id": personalOwnerID(userID).String(),
		"actor":             userID,
		"credit_type":       h.creditType,
		"amount":            amount,
		"source":            source,
		"source_id":         sourceID,
		"expires_at":        time.Now().Add(15 * time.Minute).Unix(),
	})
}

func (h *billingE2EHarness) mustHold(userID, source, sourceID string, amount int64) string {
	h.t.Helper()
	w := h.hold(userID, source, sourceID, amount)
	require.Equal(h.t, http.StatusOK, w.Code, "hold body: %s", w.Body.String())
	var hr holdResult
	require.NoError(h.t, json.Unmarshal(w.Body.Bytes(), &hr))
	require.NotEmpty(h.t, hr.ID)
	return hr.ID
}

func (h *billingE2EHarness) capture(holdID string, amount int64) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.do(http.MethodPost, "/v1/service/credits/holds/"+holdID+"/capture", map[string]any{
		"amount": amount,
	})
}

func (h *billingE2EHarness) release(holdID string) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.do(http.MethodPost, "/v1/service/credits/holds/"+holdID+"/release", nil)
}

// balance reads available + held via the public tenant-subject balance route.
type balanceView struct {
	Balance     int64 `json:"balance"`
	HeldBalance int64 `json:"held_balance"`
}

func (h *billingE2EHarness) balance(userID string) balanceView {
	h.t.Helper()
	w := h.do(http.MethodGet, "/v1/service/credits/balance?merchant_subject_id="+personalOwnerID(userID).String()+"&credit_type="+h.creditType, nil)
	require.Equal(h.t, http.StatusOK, w.Code, "balance body: %s", w.Body.String())
	var payload struct {
		BalanceMicros int64 `json:"balance_micros"`
		HeldMicros    int64 `json:"held_micros"`
	}
	require.NoError(h.t, json.Unmarshal(w.Body.Bytes(), &payload))
	return balanceView{Balance: payload.BalanceMicros, HeldBalance: payload.HeldMicros}
}

// ledgerRows returns all money_transactions for a tenant subject (USD), for
// direct ledger assertions.
func (h *billingE2EHarness) ledgerRows(userID string) []models.MoneyTransaction {
	h.t.Helper()
	owner := personalOwnerID(userID)
	pgRows, err := h.suite.Pool.Query(context.Background(),
		"SELECT "+moneyTransactionCols+" FROM openrails.money_transactions WHERE merchant_subject_id = $1 AND currency = $2 ORDER BY created_at ASC",
		owner, "USD")
	require.NoError(h.t, err)
	defer pgRows.Close()
	var rows []models.MoneyTransaction
	for pgRows.Next() {
		trx, err := scanMoneyTransaction(pgRows)
		require.NoError(h.t, err)
		rows = append(rows, *trx)
	}
	require.NoError(h.t, pgRows.Err())
	return rows
}

// rawBalanceRow returns the persisted balance row for a tenant subject, or nil.
func (h *billingE2EHarness) rawBalanceRow(userID string) *models.MoneyBalance {
	h.t.Helper()
	owner := personalOwnerID(userID)
	bal := new(models.MoneyBalance)
	err := h.suite.Pool.QueryRow(context.Background(), `
		SELECT id, merchant_id, merchant_subject_id, currency, balance, held_balance, created_at, updated_at
		FROM openrails.money_balances
		WHERE merchant_subject_id = $1 AND currency = $2
		LIMIT 1`, owner, "USD").
		Scan(&bal.ID, &bal.MerchantID, &bal.MerchantSubjectID, &bal.Currency,
			&bal.Balance, &bal.HeldBalance, &bal.CreatedAt, &bal.UpdatedAt)
	if err != nil {
		return nil
	}
	return bal
}

// sumDebits returns the sum of all withdrawal/capture amounts (negative values)
// recorded in the ledger for this owner. Used for conservation checks.
func (h *billingE2EHarness) sumLedgerAmounts(userID string) int64 {
	h.t.Helper()
	var total int64
	for _, r := range h.ledgerRows(userID) {
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

func findHold(rows []models.MoneyTransaction, holdID string) *models.MoneyTransaction {
	for i := range rows {
		if rows[i].ID.String() == holdID && rows[i].TransactionType == "hold" {
			return &rows[i]
		}
	}
	return nil
}

// --- Scenarios ---------------------------------------------------------------

// TestUnifiedBilling_PrepaidHoldCapture_Full: deposit -> hold estimate ->
// capture the full authorized amount. Balance reduced by the full amount, held
// returns to 0, hold row captured.
func TestUnifiedBilling_PrepaidHoldCapture_Full(t *testing.T) {
	suite := getSharedTestSuite(t)
	h := newBillingE2EHarness(t, suite)
	user := uuid.NewString()

	h.deposit(user, 10_000)
	require.Equal(t, balanceView{Balance: 10_000, HeldBalance: 0}, h.balance(user))

	holdID := h.mustHold(user, "gen_job", "job-full-1", 3_000)
	// Hold reserves but does not debit: available drops, balance unchanged.
	require.Equal(t, balanceView{Balance: 10_000, HeldBalance: 3_000}, h.balance(user))

	w := h.capture(holdID, 3_000)
	require.Equal(t, http.StatusOK, w.Code, "capture body: %s", w.Body.String())

	require.Equal(t, balanceView{Balance: 7_000, HeldBalance: 0}, h.balance(user))

	rows := h.ledgerRows(user)
	hold := findHold(rows, holdID)
	require.NotNil(t, hold)
	require.Equal(t, "captured", hold.Status)
	require.NotNil(t, hold.Captured)
	require.Equal(t, int64(3_000), *hold.Captured)
	require.Equal(t, int64(-3_000), hold.Amount)
}

// TestUnifiedBilling_PrepaidHoldCapture_Partial: the core money-path scenario.
// Deposit, hold a generous estimate, then capture LESS than held (the metered
// actual). The remainder must be released back to available, held returns to 0,
// and total credits are conserved (debited == captured actual only).
func TestUnifiedBilling_PrepaidHoldCapture_Partial(t *testing.T) {
	suite := getSharedTestSuite(t)
	h := newBillingE2EHarness(t, suite)
	user := uuid.NewString()

	h.deposit(user, 10_000)

	const estimate = 4_000
	const actual = 1_500
	holdID := h.mustHold(user, "gen_job", "job-partial-1", estimate)
	require.Equal(t, balanceView{Balance: 10_000, HeldBalance: estimate}, h.balance(user))

	w := h.capture(holdID, actual)
	require.Equal(t, http.StatusOK, w.Code, "capture body: %s", w.Body.String())

	// Captured amount debited; remainder (estimate-actual) released; held -> 0.
	got := h.balance(user)
	require.Equal(t, int64(10_000-actual), got.Balance, "only the actual is debited")
	require.Equal(t, int64(0), got.HeldBalance, "remainder released, no lingering hold")

	rows := h.ledgerRows(user)
	hold := findHold(rows, holdID)
	require.NotNil(t, hold)
	require.Equal(t, "captured", hold.Status)
	require.NotNil(t, hold.Authorized)
	require.Equal(t, int64(estimate), *hold.Authorized)
	require.NotNil(t, hold.Captured)
	require.Equal(t, int64(actual), *hold.Captured)
	require.Equal(t, int64(-actual), hold.Amount)

	// Conservation: deposit(+10000) + captured-hold(-1500) == persisted balance.
	require.Equal(t, int64(10_000-actual), h.sumLedgerAmounts(user))
	require.Equal(t, int64(10_000-actual), h.rawBalanceRow(user).Balance)
}

// TestUnifiedBilling_InsufficientBalance: holding beyond available is rejected
// cleanly (402 insufficient_credits) and the balance is untouched.
func TestUnifiedBilling_InsufficientBalance(t *testing.T) {
	suite := getSharedTestSuite(t)
	h := newBillingE2EHarness(t, suite)
	user := uuid.NewString()

	h.deposit(user, 1_000)

	w := h.hold(user, "gen_job", "job-overdraw-1", 5_000)
	require.Equal(t, http.StatusPaymentRequired, w.Code, "body: %s", w.Body.String())
	require.Contains(t, w.Body.String(), "insufficient_credits")

	// Balance fully intact, nothing held, no hold row written.
	require.Equal(t, balanceView{Balance: 1_000, HeldBalance: 0}, h.balance(user))
	require.Equal(t, 0, countByType(h.ledgerRows(user), "hold"))
}

// TestUnifiedBilling_FailureRelease: hold then release (worker failed / zero
// usage). The full reservation is restored and nothing is debited.
func TestUnifiedBilling_FailureRelease(t *testing.T) {
	suite := getSharedTestSuite(t)
	h := newBillingE2EHarness(t, suite)
	user := uuid.NewString()

	h.deposit(user, 8_000)
	holdID := h.mustHold(user, "gen_job", "job-fail-1", 2_500)
	require.Equal(t, balanceView{Balance: 8_000, HeldBalance: 2_500}, h.balance(user))

	w := h.release(holdID)
	require.Equal(t, http.StatusOK, w.Code, "release body: %s", w.Body.String())

	// Full reservation restored, balance untouched, nothing debited.
	require.Equal(t, balanceView{Balance: 8_000, HeldBalance: 0}, h.balance(user))

	rows := h.ledgerRows(user)
	hold := findHold(rows, holdID)
	require.NotNil(t, hold)
	require.Equal(t, "released", hold.Status)
	require.Equal(t, int64(0), hold.Amount, "released hold debits nothing")
	require.Equal(t, int64(8_000), h.sumLedgerAmounts(user))
}

// TestUnifiedBilling_Idempotency_HoldReplay: replaying the same (source,
// source_id) hold returns the SAME hold and does not double-reserve.
func TestUnifiedBilling_Idempotency_HoldReplay(t *testing.T) {
	suite := getSharedTestSuite(t)
	h := newBillingE2EHarness(t, suite)
	user := uuid.NewString()

	h.deposit(user, 10_000)

	holdID1 := h.mustHold(user, "gen_job", "job-idem-1", 3_000)
	require.Equal(t, balanceView{Balance: 10_000, HeldBalance: 3_000}, h.balance(user))

	// Replay with identical source/source_id: same hold id, held NOT doubled.
	holdID2 := h.mustHold(user, "gen_job", "job-idem-1", 3_000)
	require.Equal(t, holdID1, holdID2, "replay must return the same hold")
	require.Equal(t, balanceView{Balance: 10_000, HeldBalance: 3_000}, h.balance(user))

	require.Equal(t, 1, countByType(h.ledgerRows(user), "hold"), "exactly one hold row")

	// Capturing once debits once; the single hold is now captured.
	w := h.capture(holdID1, 3_000)
	require.Equal(t, http.StatusOK, w.Code, "capture body: %s", w.Body.String())
	require.Equal(t, balanceView{Balance: 7_000, HeldBalance: 0}, h.balance(user))
}

// TestUnifiedBilling_OwnerScoping: owner A's hold/capture never touches owner B.
// Owners here are the deterministic personal orgs of two distinct user ids,
// which is exactly how the service routes scope (resolveOwner(nil, userID)).
func TestUnifiedBilling_OwnerScoping(t *testing.T) {
	suite := getSharedTestSuite(t)
	h := newBillingE2EHarness(t, suite)
	ownerA := uuid.NewString()
	ownerB := uuid.NewString()

	h.deposit(ownerA, 5_000)
	h.deposit(ownerB, 9_000)

	// Same logical source_id under each owner: idempotency is per-owner, so both
	// holds succeed independently and are charged to their own balances.
	holdA := h.mustHold(ownerA, "gen_job", "shared-job-1", 2_000)
	holdB := h.mustHold(ownerB, "gen_job", "shared-job-1", 6_000)
	require.NotEqual(t, holdA, holdB)

	require.Equal(t, balanceView{Balance: 5_000, HeldBalance: 2_000}, h.balance(ownerA))
	require.Equal(t, balanceView{Balance: 9_000, HeldBalance: 6_000}, h.balance(ownerB))

	// Capture A only. B is completely unaffected.
	w := h.capture(holdA, 2_000)
	require.Equal(t, http.StatusOK, w.Code, "capture body: %s", w.Body.String())

	require.Equal(t, balanceView{Balance: 3_000, HeldBalance: 0}, h.balance(ownerA))
	require.Equal(t, balanceView{Balance: 9_000, HeldBalance: 6_000}, h.balance(ownerB),
		"owner B untouched by owner A's capture")

	// Tenant subject A's ledger has no rows belonging to tenant subject B and vice versa.
	for _, r := range h.ledgerRows(ownerA) {
		require.Equal(t, personalOwnerID(ownerA), r.MerchantSubjectID)
	}
	for _, r := range h.ledgerRows(ownerB) {
		require.Equal(t, personalOwnerID(ownerB), r.MerchantSubjectID)
	}
}

// TestUnifiedBilling_LifecycleViaPublicServiceTokenRoutes is the end-to-end "happy path"
// that a gen-orchestrator / Tensorhub driver would run for a single job, all
// over the service-token-authenticated public surface, proving the standalone-call contract:
// fund -> lookup confirms hold -> capture actual -> ledger conserved.
func TestUnifiedBilling_LifecycleViaPublicServiceTokenRoutes(t *testing.T) {
	suite := getSharedTestSuite(t)
	h := newBillingE2EHarness(t, suite)
	user := uuid.NewString()

	h.deposit(user, 20_000)
	holdID := h.mustHold(user, "gen_job", "lifecycle-job-1", 5_000)

	// Lookup the hold by source the way an orchestrator reconciles state.
	lookup := h.do(http.MethodGet, fmt.Sprintf(
		"/v1/service/credits/transactions/lookup?actor=%s&credit_type=%s&transaction_type=hold&source=gen_job&source_id=lifecycle-job-1",
		user, h.creditType), nil)
	require.Equal(t, http.StatusOK, lookup.Code, "lookup body: %s", lookup.Body.String())
	require.Contains(t, lookup.Body.String(), holdID)

	// Metered actual comes in under the estimate -> partial capture.
	w := h.capture(holdID, 3_200)
	require.Equal(t, http.StatusOK, w.Code, "capture body: %s", w.Body.String())

	require.Equal(t, balanceView{Balance: 16_800, HeldBalance: 0}, h.balance(user))
	require.Equal(t, int64(16_800), h.rawBalanceRow(user).Balance)
	require.Equal(t, int64(16_800), h.sumLedgerAmounts(user))
}
