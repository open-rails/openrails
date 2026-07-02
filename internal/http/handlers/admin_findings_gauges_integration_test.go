//go:build integration

package handlers

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/grants"
	"github.com/open-rails/openrails/internal/reconcile/converge"
	"github.com/open-rails/openrails/internal/reconcile/recommend"
	"github.com/open-rails/openrails/pkg/merchant"
)

// ============================================================================
// #690 invariant gauges — REAL Postgres, the REAL converge detectors, the
// REAL findings queue endpoints. Freeloaders/duplicate_coverage are counted
// from detector-emitted findings (not seeded rows); verification_pressure is
// read live from the subscriptions table.
// ============================================================================

// gaugesProbe decodes the full gauges object incl. verification_pressure
// (the sibling findingsListBody predates the third gauge).
type gaugesProbe struct {
	Gauges struct {
		Freeloaders          int64 `json:"freeloaders"`
		DuplicateCoverage    int64 `json:"duplicate_coverage"`
		VerificationPressure struct {
			Count         int64 `json:"count"`
			MaxAgeSeconds int64 `json:"max_age_seconds"`
		} `json:"verification_pressure"`
		TotalOpen int64 `json:"total_open"`
	} `json:"gauges"`
}

func (fx *findingsFixture) gauges() gaugesProbe {
	fx.t.Helper()
	rec := fx.do(AdminListFindings, http.MethodGet, "/findings", nil, "")
	require.Equal(fx.t, http.StatusOK, rec.Code, rec.Body.String())
	var probe gaugesProbe
	require.NoError(fx.t, json.Unmarshal(rec.Body.Bytes(), &probe))
	return probe
}

func (fx *findingsFixture) sweep() {
	fx.t.Helper()
	_, err := converge.NewConvergeEngine(fx.dbi).Converge(fx.ctx,
		converge.Scope{Merchant: merchant.ID(fx.merchant), Customer: &fx.customer})
	require.NoError(fx.t, err)
}

// TestFindingsGaugesFreeloaderDetectorEndToEnd: a healthy merchant reads all
// gauges zero; a seeded freeloader shape (live standing window, subscription
// row missing) is DETECTED by the sweep, moves the freeloaders gauge to 1
// with the revoke recommendation, and approving the revoke through the queue
// drops the gauge back to zero — the next sweep confirms by re-measurement.
func TestFindingsGaugesFreeloaderDetectorEndToEnd(t *testing.T) {
	fx := newFindingsFixture(t)

	// Healthy dataset: all three gauges zero/empty.
	healthy := fx.gauges()
	assert.Zero(t, healthy.Gauges.Freeloaders)
	assert.Zero(t, healthy.Gauges.DuplicateCoverage)
	assert.Zero(t, healthy.Gauges.VerificationPressure.Count)
	assert.Zero(t, healthy.Gauges.VerificationPressure.MaxAgeSeconds)
	assert.Zero(t, healthy.Gauges.TotalOpen)

	// Freeloader: a live STANDING window justified by nothing (no sub row).
	var entID uuid.UUID
	require.NoError(t, fx.dbi.Pool().QueryRow(fx.ctx,
		`INSERT INTO openrails.entitlements (merchant_id, customer_id, entitlement, start_at, end_at, source_type, source_id)
		 VALUES ($1, $2, 'premium', now() - interval '30 days', NULL, 'subscription', $3) RETURNING id`,
		fx.merchant, fx.customer, uuid.New()).Scan(&entID))

	fx.sweep()

	body := fx.list("?finding_type=derive.entitlement.orphan")
	require.Len(t, body.Items, 1, "the sweep detected the freeloader")
	item := body.Items[0]
	assert.Equal(t, "high", item.Severity)
	require.NotNil(t, item.Recommendation, "detector emitted the structured recommendation")
	assert.Equal(t, recommend.ActionRevokeEntitlement, item.Recommendation.Action)
	assert.Equal(t, entID.String(), item.Recommendation.Params["entitlement_id"])
	assert.EqualValues(t, 1, body.Gauges.Freeloaders, "freeloaders gauge counts the open orphan finding")
	assert.EqualValues(t, 0, body.Gauges.DuplicateCoverage)

	// Approve the revoke through the queue.
	rec := fx.do(AdminResolveFinding, http.MethodPost, "/findings/"+item.ID+"/resolve",
		map[string]any{"outcome": "approve", "notes": "no justification found"}, item.ID)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var revokedAt *time.Time
	require.NoError(t, fx.dbi.Pool().QueryRow(fx.ctx,
		`SELECT revoked_at FROM openrails.entitlements WHERE id = $1`, entID).Scan(&revokedAt))
	require.NotNil(t, revokedAt, "approve executed the revoke")

	// Next sweep re-measures: condition gone, finding stays fixed, gauge zero.
	fx.sweep()
	after := fx.gauges()
	assert.Zero(t, after.Gauges.Freeloaders, "gauge back to zero after the approved fix")
	row, err := gen.New(fx.dbi.Pool()).GetReconciliationFinding(fx.ctx, uuid.MustParse(item.ID))
	require.NoError(t, err)
	assert.Equal(t, "fixed", row.Status, "sweep did not reopen the confirmed fix")
}

// TestFindingsGaugesVerificationPressure: `unknown` subs past paid-through are
// a live pressure reading (count + max age), not findings; resolving one drops
// the gauge. Nonzero pressure is allowed — max_age trending up is the alert.
func TestFindingsGaugesVerificationPressure(t *testing.T) {
	fx := newFindingsFixture(t)
	product2, price2 := fx.seedSecondProduct()

	seedUnknown := func(productID, priceID uuid.UUID, pastBy time.Duration) uuid.UUID {
		subID := uuid.New()
		now := time.Now().UTC()
		fx.exec(`INSERT INTO openrails.subscriptions
		          (id, price_id, product_id, status, rail, rail_subscription_id,
		           current_period_starts_at, current_period_ends_at, started_at, customer_id, merchant_id)
		        VALUES ($1, $2, $3, 'unknown', 'nmi', $4, $5, $6, $5, $7, $8)`,
			subID, priceID, productID, "vp-"+uuid.NewString()[:8],
			now.Add(-30*24*time.Hour), now.Add(-pastBy), fx.customer, fx.merchant)
		return subID
	}
	oldest := seedUnknown(fx.product, fx.price, 3*time.Hour)
	seedUnknown(product2, price2, 1*time.Hour)

	probe := fx.gauges()
	assert.EqualValues(t, 2, probe.Gauges.VerificationPressure.Count)
	assert.InDelta(t, 3*3600, probe.Gauges.VerificationPressure.MaxAgeSeconds, 120, "max age tracks the OLDEST lapsed paid-through")
	// A pressure reading, not an error: no findings, other gauges untouched.
	assert.Zero(t, probe.Gauges.Freeloaders)
	assert.Zero(t, probe.Gauges.TotalOpen)

	// Verification resolves the oldest sub (renewed) -> count and max age drop.
	fx.exec(`UPDATE openrails.subscriptions SET status = 'active', current_period_ends_at = now() + interval '30 days' WHERE id = $1`, oldest)
	probe = fx.gauges()
	assert.EqualValues(t, 1, probe.Gauges.VerificationPressure.Count)
	assert.InDelta(t, 3600, probe.Gauges.VerificationPressure.MaxAgeSeconds, 120)
}

// TestFindingsDuplicateOwnershipDetectorEndToEnd: the cross-month ownership
// double-purchase is DETECTED (critical, cancel_and_refund naming the later
// payment), approving executes the refund-only recommendation (subscription_id
// is optional post-#690), and the refund nets the duplicate out — the next
// sweep confirms instead of reopening.
func TestFindingsDuplicateOwnershipDetectorEndToEnd(t *testing.T) {
	fx := newFindingsFixture(t)
	pay1, pay2 := uuid.New(), uuid.New()
	now := time.Now().UTC()
	seedPay := func(id uuid.UUID, txn string, at time.Time) {
		fx.exec(`INSERT INTO openrails.payments
		          (id, price_id, rail, transaction_id, amount, list_amount, currency, status, purchased_at, customer_id, merchant_id)
		        VALUES ($1, $2, 'nmi', $3, 10000000, 10000000, 'usd', 'completed', $4, $5, $6)`,
			id, fx.price, txn, at, fx.customer, fx.merchant)
	}
	// Two months apart: invisible to the month-scoped provider_charge check.
	seedPay(pay1, "dup-1-"+uuid.NewString()[:8], now.Add(-65*24*time.Hour))
	seedPay(pay2, "dup-2-"+uuid.NewString()[:8], now.Add(-24*time.Hour))
	gl := grants.New(gen.New(fx.dbi.Pool()), fx.merchant)
	for _, p := range []uuid.UUID{pay1, pay2} {
		pid := p
		_, err := gl.Grant(fx.ctx, grants.GrantInput{
			Customer: fx.customer, Kind: grants.Ownership, Product: &fx.product,
			Source: grants.Purchase, SourceID: pid.String(), Payment: &pid,
		})
		require.NoError(t, err)
	}

	fx.sweep()

	body := fx.list("?finding_type=consistency.duplicate.ownership")
	require.Len(t, body.Items, 1, "cross-month double purchase detected")
	item := body.Items[0]
	assert.Equal(t, "critical", item.Severity, "#690: duplicates outrank freeloaders")
	require.NotNil(t, item.Recommendation)
	assert.Equal(t, recommend.ActionCancelAndRefund, item.Recommendation.Action)
	assert.Equal(t, pay2.String(), item.Recommendation.Params["refund_payment_id"], "later purchase is the refund target")
	_, hasSub := item.Recommendation.Params["subscription_id"]
	assert.False(t, hasSub, "pure one-off duplicate: refund-only recommendation")
	assert.EqualValues(t, 1, body.Gauges.DuplicateCoverage)

	// Approve: refund-only cancel_and_refund (the #690 executor relaxation).
	rec := fx.do(AdminResolveFinding, http.MethodPost, "/findings/"+item.ID+"/resolve",
		map[string]any{"outcome": "approve", "notes": "double purchase confirmed"}, item.ID)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resolved resolveBody
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resolved))
	assert.Equal(t, "completed", resolved.Execution["refund"])
	_, cancelled := resolved.Execution["cancel"]
	assert.False(t, cancelled, "no subscription leg: refund executes alone")
	assert.EqualValues(t, 1, fx.fake.refundCalls.Load())

	// Next sweep: the refund netted the duplicate out — confirmed, not reopened.
	fx.sweep()
	after := fx.gauges()
	assert.Zero(t, after.Gauges.DuplicateCoverage, "gauge back to zero after the refund")
	row, err := gen.New(fx.dbi.Pool()).GetReconciliationFinding(fx.ctx, uuid.MustParse(item.ID))
	require.NoError(t, err)
	assert.Equal(t, "fixed", row.Status)
}

// TestFindingsCancelAndRefundRequiresATarget: the relaxed executor still
// refuses an empty cancel_and_refund — approve is a 400 and the finding stays
// open.
func TestFindingsCancelAndRefundRequiresATarget(t *testing.T) {
	fx := newFindingsFixture(t)
	findingID := fx.seedFinding("consistency.duplicate.ownership", "subject-"+uuid.NewString()[:8], "critical",
		"duplicate with no mechanical target", &recommend.Recommendation{
			Action: recommend.ActionCancelAndRefund,
			Params: map[string]any{},
		})
	rec := fx.do(AdminResolveFinding, http.MethodPost, "/findings/"+findingID.String()+"/resolve",
		map[string]any{"outcome": "approve", "notes": "try"}, findingID.String())
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Equal(t, "requires_review", string(fx.findingRow(findingID).Status))
}
