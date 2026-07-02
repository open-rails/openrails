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
	postgresmigrations "github.com/open-rails/openrails/migrations/postgres"
	"github.com/open-rails/openrails/pkg/merchant"
)

// ============================================================================
// #690 invariant gauges — REAL Postgres, the REAL converge detectors, the
// REAL findings queue endpoints. Freeloaders/duplicate_coverage are counted
// from detector-emitted findings (not seeded rows); verification_pressure is
// read live from the subscriptions table.
// ============================================================================

// gaugesProbe decodes the full gauges object incl. verification_pressure and
// the #690 episode summary (the sibling findingsListBody predates them).
type episodeSummaryProbe struct {
	Total        int64   `json:"total"`
	Open         int64   `json:"open"`
	Unsanctioned int64   `json:"unsanctioned"`
	TotalDays    float64 `json:"total_days"`
}

type gaugesProbe struct {
	Gauges struct {
		OrphanedMembers      int64 `json:"orphaned_members"`
		Freeloaders          int64 `json:"freeloaders"`
		DuplicateCoverage    int64 `json:"duplicate_coverage"`
		VerificationPressure struct {
			Count         int64 `json:"count"`
			MaxAgeSeconds int64 `json:"max_age_seconds"`
		} `json:"verification_pressure"`
		Episodes struct {
			Freeloader episodeSummaryProbe `json:"freeloader"`
			Orphaned   episodeSummaryProbe `json:"orphaned"`
		} `json:"episodes"`
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

	body := fx.list("?finding_type=derive.entitlement.unjustified")
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

// seedGrantableProduct: a product that PROMISES entitlements (non-empty
// entitlements_spec) with a time-boxed price — the shape derive.grant.missing
// and the episode views key on (the fixture's default product is spec-less).
func (fx *findingsFixture) seedGrantableProduct(feature string, hours int) (productID, priceID uuid.UUID) {
	fx.t.Helper()
	productID, priceID = uuid.New(), uuid.New()
	sfx := uuid.NewString()[:8]
	fx.exec(`INSERT INTO openrails.products (id, key, display_name, entitlements_spec, merchant_id)
	         VALUES ($1, $2, $2, jsonb_build_object($3::text, NULL), $4)`,
		productID, "grantable-"+sfx, feature, fx.merchant)
	fx.exec(`INSERT INTO openrails.prices (id, product_id, amount, currency, access_duration_hours, auto_renew, merchant_id)
	         VALUES ($1, $2, 10000000, 'usd', $3, true, $4)`, priceID, productID, hours, fx.merchant)
	return productID, priceID
}

// TestFindingsGaugesOrphanedMembersDetectorEndToEnd (#690 three categories):
// the ORPHANED shape — money collected, access never delivered (a completed
// payment for a grantable product with NO grant) — is detected by the sweep as
// derive.grant.missing at severity CRITICAL (taking money wrongly outranks
// giving content away), moves the orphaned_members gauge, and shows up in the
// orphaned_episodes analytic as an OPEN paying-without-access span. The
// finding is surface-only (no structured recommendation → approve is 422);
// ignoring it clears the gauge while the episode persists (history, not
// queue state).
func TestFindingsGaugesOrphanedMembersDetectorEndToEnd(t *testing.T) {
	fx := newFindingsFixture(t)

	healthy := fx.gauges()
	assert.Zero(t, healthy.Gauges.OrphanedMembers)
	assert.Zero(t, healthy.Gauges.Episodes.Orphaned.Total)
	assert.Zero(t, healthy.Gauges.Episodes.Freeloader.Total)

	_, priceID := fx.seedGrantableProduct("orph-feat-"+uuid.NewString()[:8], 720)
	payID := uuid.New()
	fx.exec(`INSERT INTO openrails.payments
	          (id, price_id, rail, transaction_id, amount, list_amount, currency, status, purchased_at, customer_id, merchant_id)
	        VALUES ($1, $2, 'nmi', $3, 10000000, 10000000, 'usd', 'completed', now() - interval '3 days', $4, $5)`,
		payID, priceID, "orphpay-"+uuid.NewString()[:8], fx.customer, fx.merchant)

	fx.sweep()

	body := fx.list("?finding_type=derive.grant.missing")
	require.Len(t, body.Items, 1, "the sweep detected the undelivered paid grant")
	item := body.Items[0]
	assert.Equal(t, "critical", item.Severity, "#690: orphaned (paying, no access) is critical")
	assert.Equal(t, "requires_review", item.Status, "ADMIN surface-only")
	assert.Nil(t, item.Recommendation, "no mechanical fix: re-granting re-runs derive-1")

	probe := fx.gauges()
	assert.EqualValues(t, 1, probe.Gauges.OrphanedMembers, "orphaned_members counts the open MISSING-side finding")
	assert.Zero(t, probe.Gauges.Freeloaders, "orphaned is not freeloading")
	assert.EqualValues(t, 1, probe.Gauges.Episodes.Orphaned.Total, "paid-but-no-access span in the episode view")
	assert.EqualValues(t, 1, probe.Gauges.Episodes.Orphaned.Open, "still paying (coverage reaches past now): open episode")
	assert.InDelta(t, 3.0, probe.Gauges.Episodes.Orphaned.TotalDays, 0.2, "3 uncovered days so far")

	// approve is unavailable (surface-only finding) …
	rec := fx.do(AdminResolveFinding, http.MethodPost, "/findings/"+item.ID+"/resolve",
		map[string]any{"outcome": "approve", "notes": "try"}, item.ID)
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	// … ignore clears the gauge; the episode is history and persists.
	rec = fx.do(AdminResolveFinding, http.MethodPost, "/findings/"+item.ID+"/resolve",
		map[string]any{"outcome": "ignore", "notes": "operator investigated out-of-band"}, item.ID)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	after := fx.gauges()
	assert.Zero(t, after.Gauges.OrphanedMembers, "gauge reads open findings only")
	assert.EqualValues(t, 1, after.Gauges.Episodes.Orphaned.Total, "the episode analytic is unaffected by queue state")
}

// TestFindingsFreeloaderEpisodes (#690 episode analytics): the freeloader view
// turns access-without-payment into labeled SPANS — sanctioned_dunning
// (past_due with a retry scheduled) and awaiting_verification (`unknown` past
// paid-through, the #691 carve-out) are POLICY labels; unsanctioned is the
// failure class. Open episodes end at now(); a revoked overrun is a CLOSED
// span [paid-through, revoked_at). A paying active sub never appears.
func TestFindingsFreeloaderEpisodes(t *testing.T) {
	fx := newFindingsFixture(t)
	now := time.Now().UTC()
	sfx := uuid.NewString()[:8]

	seedSub := func(productID, priceID uuid.UUID, status string, periodEnd time.Time, endedAt, nextRetry *time.Time) uuid.UUID {
		subID := uuid.New()
		var cancelledAt *time.Time
		var cancelType *string
		if status == "cancelled" {
			ct := "user"
			ca := periodEnd
			cancelledAt, cancelType = &ca, &ct
		}
		fx.exec(`INSERT INTO openrails.subscriptions
		          (id, price_id, product_id, status, rail, rail_subscription_id,
		           current_period_starts_at, current_period_ends_at, started_at, ended_at,
		           next_retry_at, cancelled_at, cancel_type, customer_id, merchant_id)
		        VALUES ($1,$2,$3,$4,'nmi',$5,$6,$7,$6,$8,$9,$10,$11,$12,$13)`,
			subID, priceID, productID, status, "fl-"+uuid.NewString()[:8],
			now.Add(-40*24*time.Hour), periodEnd, endedAt, nextRetry, cancelledAt, cancelType,
			fx.customer, fx.merchant)
		return subID
	}
	seedWindow := func(feature string, subID uuid.UUID, revokedAt *time.Time) {
		var reason *string
		if revokedAt != nil {
			r := "episode test revocation"
			reason = &r
		}
		fx.exec(`INSERT INTO openrails.entitlements
		          (customer_id, entitlement, start_at, end_at, source_id, source_type, revoked_at, revoke_reason, merchant_id)
		        VALUES ($1,$2,$3,NULL,$4,'subscription',$5,$6,$7)`,
			fx.customer, feature, now.Add(-40*24*time.Hour), subID, revokedAt, reason, fx.merchant)
	}
	ts := func(d time.Duration) *time.Time { t := now.Add(d); return &t }

	// a: cancelled sub, closure never landed -> OPEN unsanctioned, ~10 days.
	pA, prA := fx.seedGrantableProduct("fl-a-"+sfx, 720)
	seedWindow("fl-a-"+sfx, seedSub(pA, prA, "cancelled", now.Add(-10*24*time.Hour), ts(-10*24*time.Hour), nil), nil)
	// b: past_due WITH retry scheduled -> OPEN sanctioned_dunning, ~5 days.
	pB, prB := fx.seedGrantableProduct("fl-b-"+sfx, 720)
	seedWindow("fl-b-"+sfx, seedSub(pB, prB, "past_due", now.Add(-5*24*time.Hour), nil, ts(24*time.Hour)), nil)
	// c: unknown past paid-through -> OPEN awaiting_verification, ~3 days.
	pC, prC := fx.seedGrantableProduct("fl-c-"+sfx, 720)
	seedWindow("fl-c-"+sfx, seedSub(pC, prC, "unknown", now.Add(-3*24*time.Hour), nil, nil), nil)
	// d: cancelled sub, overrun later revoked -> CLOSED unsanctioned,
	// [paid-through -20d, revoked -8d) = ~12 days.
	pD, prD := fx.seedGrantableProduct("fl-d-"+sfx, 720)
	seedWindow("fl-d-"+sfx, seedSub(pD, prD, "cancelled", now.Add(-20*24*time.Hour), ts(-20*24*time.Hour), nil), ts(-8*24*time.Hour))
	// e (negative): active, paid through the future -> never an episode.
	pE, prE := fx.seedGrantableProduct("fl-e-"+sfx, 720)
	seedWindow("fl-e-"+sfx, seedSub(pE, prE, "active", now.Add(20*24*time.Hour), nil, nil), nil)

	type episode struct {
		cause string
		open  bool
		days  float64
	}
	got := map[string]episode{}
	rows, err := fx.dbi.Pool().Query(fx.ctx,
		`SELECT entitlement, cause, open, days FROM openrails.freeloader_episodes WHERE merchant_id=$1`, fx.merchant)
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var feature string
		var e episode
		require.NoError(t, rows.Scan(&feature, &e.cause, &e.open, &e.days))
		got[feature] = e
	}
	require.NoError(t, rows.Err())

	require.Len(t, got, 4, "four freeloader spans, the paying active sub excluded")
	assert.Equal(t, "unsanctioned", got["fl-a-"+sfx].cause, "closure never landed: the failure class")
	assert.True(t, got["fl-a-"+sfx].open)
	assert.InDelta(t, 10.0, got["fl-a-"+sfx].days, 0.2)
	assert.Equal(t, "sanctioned_dunning", got["fl-b-"+sfx].cause, "dunning with a scheduled retry is policy, labeled — never counted as failure")
	assert.True(t, got["fl-b-"+sfx].open)
	assert.InDelta(t, 5.0, got["fl-b-"+sfx].days, 0.2)
	assert.Equal(t, "awaiting_verification", got["fl-c-"+sfx].cause, "#691 fail-open carve-out labeled, not failure")
	assert.InDelta(t, 3.0, got["fl-c-"+sfx].days, 0.2)
	assert.Equal(t, "unsanctioned", got["fl-d-"+sfx].cause)
	assert.False(t, got["fl-d-"+sfx].open, "revocation closed the episode")
	assert.InDelta(t, 12.0, got["fl-d-"+sfx].days, 0.2)

	probe := fx.gauges()
	assert.EqualValues(t, 4, probe.Gauges.Episodes.Freeloader.Total)
	assert.EqualValues(t, 3, probe.Gauges.Episodes.Freeloader.Open)
	assert.EqualValues(t, 2, probe.Gauges.Episodes.Freeloader.Unsanctioned, "only the failure class; sanctioned spans are policy")
	assert.InDelta(t, 30.0, probe.Gauges.Episodes.Freeloader.TotalDays, 1.0)
	assert.Zero(t, probe.Gauges.Freeloaders, "no sweep ran: the point-in-time gauge reads findings, the episodes read state")
}

// TestFindingsOrphanedEpisodes (#690 episode analytics, the mirror): spans
// where payment coverage existed but no entitlement window covered the time —
// an active paying sub with NO window (open, still accruing), a runway wrongly
// revoked early (closed span [revoked_at, paid-through)), and a completed
// one-off purchase that never got its window. Covered subs and spec-less
// products never appear.
func TestFindingsOrphanedEpisodes(t *testing.T) {
	fx := newFindingsFixture(t)
	now := time.Now().UTC()
	sfx := uuid.NewString()[:8]
	ts := func(d time.Duration) *time.Time { t := now.Add(d); return &t }

	seedSub := func(productID, priceID uuid.UUID, status string, periodStart, periodEnd time.Time, endedAt *time.Time) uuid.UUID {
		subID := uuid.New()
		var cancelledAt *time.Time
		var cancelType *string
		if status == "cancelled" {
			ct := "user"
			cancelledAt, cancelType = endedAt, &ct
		}
		fx.exec(`INSERT INTO openrails.subscriptions
		          (id, price_id, product_id, status, rail, rail_subscription_id,
		           current_period_starts_at, current_period_ends_at, started_at, ended_at,
		           cancelled_at, cancel_type, customer_id, merchant_id)
		        VALUES ($1,$2,$3,$4,'nmi',$5,$6,$7,$6,$8,$9,$10,$11,$12)`,
			subID, priceID, productID, status, "orph-"+uuid.NewString()[:8],
			periodStart, periodEnd, endedAt, cancelledAt, cancelType, fx.customer, fx.merchant)
		return subID
	}

	// a: active paying sub, NO window at all -> OPEN span from period start.
	pA, prA := fx.seedGrantableProduct("oe-a-"+sfx, 720)
	subA := seedSub(pA, prA, "active", now.Add(-10*24*time.Hour), now.Add(20*24*time.Hour), nil)
	// b: cancelled sub whose window was revoked 10 days BEFORE paid-through ->
	// CLOSED span [revoked -15d, paid-through -5d).
	pB, prB := fx.seedGrantableProduct("oe-b-"+sfx, 720)
	subB := seedSub(pB, prB, "cancelled", now.Add(-30*24*time.Hour), now.Add(-5*24*time.Hour), ts(-5*24*time.Hour))
	fx.exec(`INSERT INTO openrails.entitlements
	          (customer_id, entitlement, start_at, end_at, source_id, source_type, revoked_at, revoke_reason, merchant_id)
	        VALUES ($1,$2,$3,NULL,$4,'subscription',$5,'wrongly revoked early',$6)`,
		fx.customer, "oe-b-"+sfx, now.Add(-30*24*time.Hour), subB, now.Add(-15*24*time.Hour), fx.merchant)
	// c: completed one-off purchase (30-day window promised), no window -> OPEN, ~3 days.
	_, prC := fx.seedGrantableProduct("oe-c-"+sfx, 720)
	fx.exec(`INSERT INTO openrails.payments
	          (id, price_id, rail, transaction_id, amount, list_amount, currency, status, purchased_at, customer_id, merchant_id)
	        VALUES ($1, $2, 'nmi', $3, 10000000, 10000000, 'usd', 'completed', now() - interval '3 days', $4, $5)`,
		uuid.New(), prC, "oe-pay-"+uuid.NewString()[:8], fx.customer, fx.merchant)
	// d (negative): active sub fully covered by a standing window.
	pD, prD := fx.seedGrantableProduct("oe-d-"+sfx, 720)
	subD := seedSub(pD, prD, "active", now.Add(-10*24*time.Hour), now.Add(20*24*time.Hour), nil)
	fx.exec(`INSERT INTO openrails.entitlements (customer_id, entitlement, start_at, end_at, source_id, source_type, merchant_id)
	         VALUES ($1,$2,$3,NULL,$4,'subscription',$5)`,
		fx.customer, "oe-d-"+sfx, now.Add(-10*24*time.Hour), subD, fx.merchant)
	// e (negative): paying sub for a product that promises NOTHING (spec-less
	// fixture product) — no window to miss, never an episode.
	seedSub(fx.product, fx.price, "active", now.Add(-10*24*time.Hour), now.Add(20*24*time.Hour), nil)

	type episode struct {
		sourceType string
		open       bool
		days       float64
	}
	got := map[string]episode{} // keyed by source_id
	rows, err := fx.dbi.Pool().Query(fx.ctx,
		`SELECT source_id, source_type, open, days FROM openrails.orphaned_episodes WHERE merchant_id=$1`, fx.merchant)
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var sourceID uuid.UUID
		var e episode
		require.NoError(t, rows.Scan(&sourceID, &e.sourceType, &e.open, &e.days))
		got[sourceID.String()] = e
	}
	require.NoError(t, rows.Err())

	require.Len(t, got, 3, "covered and spec-less shapes never appear")
	assert.True(t, got[subA.String()].open, "still paying: open span, accruing at now()")
	assert.InDelta(t, 10.0, got[subA.String()].days, 0.2)
	assert.False(t, got[subB.String()].open, "coverage ended in the past: closed span")
	assert.InDelta(t, 10.0, got[subB.String()].days, 0.2, "the uncovered tail [revoked_at, paid-through)")

	probe := fx.gauges()
	assert.EqualValues(t, 3, probe.Gauges.Episodes.Orphaned.Total)
	assert.EqualValues(t, 2, probe.Gauges.Episodes.Orphaned.Open)
	assert.InDelta(t, 23.0, probe.Gauges.Episodes.Orphaned.TotalDays, 1.0)
}

// TestFindingsOrphanRenameMigrationMovesRows (#690): migration 066 rewrites
// pre-rename ledger rows in place (derive.entitlement.orphan ->
// derive.entitlement.unjustified) so stable finding identities keep upserting,
// and the renamed rows count in the freeloaders gauge. Executes the REAL
// migration file; the UPDATE is idempotent, so re-running on an
// already-migrated database is safe.
func TestFindingsOrphanRenameMigrationMovesRows(t *testing.T) {
	fx := newFindingsFixture(t)
	oldID := fx.seedFinding("derive.entitlement.orphan", "entitlement:"+uuid.NewString(), "high",
		"pre-rename freeloader row", nil)
	before := fx.gauges()
	assert.Zero(t, before.Gauges.Freeloaders, "the legacy type name is not in the gauge set")

	migrationSQL, err := postgresmigrations.FS.ReadFile("066_findings_orphan_to_unjustified.up.sql")
	require.NoError(t, err)
	_, err = fx.dbi.Pool().Exec(fx.ctx, string(migrationSQL))
	require.NoError(t, err)

	row := fx.findingRow(oldID)
	assert.Equal(t, "derive.entitlement.unjustified", row.FindingType, "ledger row renamed in place")
	after := fx.gauges()
	assert.EqualValues(t, 1, after.Gauges.Freeloaders, "renamed row counts in the freeloaders gauge")
}
