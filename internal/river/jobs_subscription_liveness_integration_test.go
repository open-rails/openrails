//go:build integration

package riverjobs

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// livenessFixture provisions product/price/payment-method/subscription rows
// plus a fake NMI server. directPostHits counts every request to the
// direct-post (mutation) endpoint — the liveness worker must NEVER charge, so
// every test asserts it stays zero.
type livenessFixture struct {
	dbi             *db.DB
	pool            *pgxpool.Pool
	q               *gen.Queries
	worker          *SubscriptionLivenessWorker
	productID       uuid.UUID
	priceID         uuid.UUID
	paymentMethodID uuid.UUID
	subID           uuid.UUID
	procSubID       string
	userID          string
	directPostHits  *atomic.Int64
	// transactionXML / recurringXML drive the fake Query API responses;
	// queryStatus != 0 forces an HTTP error (unreachable provider).
	transactionXML string
	recurringXML   string
	queryStatus    int
}

func newLivenessFixture(t *testing.T, rail models.Rail, periodEndAgo time.Duration) *livenessFixture {
	t.Helper()
	dsn := dbtest.SharedPostgresDSN(t)
	ctx := context.Background()
	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbi.Pool()
	q := gen.New(pool)
	now := time.Now().UTC().Truncate(time.Second)

	f := &livenessFixture{
		dbi: dbi, pool: pool, q: q,
		productID: uuid.New(), priceID: uuid.New(), paymentMethodID: uuid.New(), subID: uuid.New(),
		procSubID: "psub_live_" + uuid.New().String(), userID: uuid.New().String(),
		directPostHits: &atomic.Int64{},
		transactionXML: `<?xml version="1.0"?><nm_response></nm_response>`,
		recurringXML:   `<?xml version="1.0"?><nm_response></nm_response>`,
	}

	billingDays32 := int32(30)
	description := "Liveness test"
	_, err := q.CreateProduct(ctx, gen.CreateProductParams{
		ID: f.productID, Slug: "live_product_" + uuid.New().String(), DisplayName: "Liveness Product",
		MerchantID:  dbtest.TestMerchantID.UUID(),
		Description: &description, Status: string(models.CatalogStatusActive),
		EntitlementsSpec: []byte(`{"premium": null}`),
		CreatedAt:        now, UpdatedAt: now,
	})
	require.NoError(t, err)
	_, err = q.CreatePrice(ctx, gen.CreatePriceParams{
		ID: f.priceID, ProductID: f.productID, Amount: 999, Currency: "usd",
		MerchantID: dbtest.TestMerchantID.UUID(),
		Status:     string(models.CatalogStatusActive), BillingCycleDays: &billingDays32, CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(t, err)

	tenantSubjectID := dbtest.EnsureCustomerIDPgx(ctx, t, pool, f.userID)
	billingID := "bill_" + uuid.New().String()
	_, err = q.CreatePaymentMethod(ctx, gen.CreatePaymentMethodParams{
		ID: f.paymentMethodID, CustomerID: tenantSubjectID, Rail: string(rail),
		RailCustomerRef: "vault_" + uuid.New().String(), RailMethodRef: billingID,
		InitialTransactionID: "txn_initial_" + uuid.New().String(), CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(t, err)

	// SILENT lapse shape: still 'active', period ended periodEndAgo ago, no
	// retry schedule, no payment recorded for the boundary.
	periodEnd := now.Add(-periodEndAgo)
	periodStart := periodEnd.Add(-30 * 24 * time.Hour)
	_, err = q.CreateSubscription(ctx, gen.CreateSubscriptionParams{
		ID: f.subID, MerchantID: dbtest.TestMerchantID.UUID(), CustomerID: tenantSubjectID, ProductID: f.productID, PriceID: &f.priceID,
		Status: string(models.StatusActive), Rail: string(rail),
		RailSubscriptionID: f.procSubID, PaymentMethodID: &f.paymentMethodID,
		EntitlementsSpecSnapshot: []byte(`{"premium": null}`),
		CurrentPeriodStartsAt:    &periodStart, CurrentPeriodEndsAt: &periodEnd, StartedAt: periodStart,
		CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(t, err)

	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.entitlements WHERE source_id = $1", f.subID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.payments WHERE subscription_id = $1", f.subID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.provider_intents WHERE subscription_id = $1", f.subID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.subscriptions WHERE id = $1", f.subID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.payment_methods WHERE id = $1", f.paymentMethodID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.prices WHERE id = $1", f.priceID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.products WHERE id = $1", f.productID)
	})

	// Fake NMI: query.php answers come from the fixture fields; ANY hit on a
	// non-query (direct-post mutation) shape increments directPostHits.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		reportType := r.Form.Get("report_type")
		if reportType == "" {
			// Direct Post API (mutations: sales, rebills, deletes).
			f.directPostHits.Add(1)
			_, _ = w.Write([]byte("response=1"))
			return
		}
		if f.queryStatus != 0 {
			w.WriteHeader(f.queryStatus)
			return
		}
		switch reportType {
		case "profile":
			_, _ = w.Write([]byte(`<?xml version="1.0"?><nm_response><merchant><company>Live TEST</company><email>live@acme.test</email></merchant></nm_response>`))
		case "transaction":
			_, _ = w.Write([]byte(f.transactionXML))
		case "recurring":
			_, _ = w.Write([]byte(f.recurringXML))
		default:
			_, _ = w.Write([]byte(`<?xml version="1.0"?><nm_response></nm_response>`))
		}
	}))
	t.Cleanup(srv.Close)

	client, err := nmi.NewClient("mobius", &config.NMIProviderSettings{
		SecurityKey: "live_security_key", WebhookSecret: "s",
	}, true)
	require.NoError(t, err)
	client.DirectPostURL = srv.URL
	client.QueryURL = srv.URL

	f.worker = &SubscriptionLivenessWorker{
		DB:         dbi,
		NMIClients: map[string]*nmi.NMIClient{"mobius": client},
	}
	return f
}

func (f *livenessFixture) run(t *testing.T) {
	t.Helper()
	require.NoError(t, f.worker.Work(context.Background(), &river.Job[SubscriptionLivenessArgs]{}))
}

func (f *livenessFixture) subRow(t *testing.T) (status string, periodEnd *time.Time, retryAttempts *int32, nextRetryAt *time.Time) {
	t.Helper()
	require.NoError(t, f.pool.QueryRow(context.Background(),
		`SELECT status, current_period_ends_at, retry_attempts, next_retry_at FROM openrails.subscriptions WHERE id = $1`, f.subID).
		Scan(&status, &periodEnd, &retryAttempts, &nextRetryAt))
	return
}

func (f *livenessFixture) successSaleXML(txnID string, at time.Time) string {
	return fmt.Sprintf(`<?xml version="1.0"?>
<nm_response>
  <transaction>
    <transaction_id>%s</transaction_id>
    <order_id>%s</order_id>
    <action><action_type>sale</action_type><success>1</success><date>%s</date></action>
  </transaction>
</nm_response>`, txnID, f.subID, at.UTC().Format("20060102150405"))
}

// TestLivenessWorker_ChargedRepair (#367): NMI charged the period and the
// webhook was lost — the worker repairs through RenewMembership: period
// advances, the payment is backfilled EXACTLY once (a second pass is a
// no-op), paid + grace entitlement windows land, and no charge is ever sent.
func TestLivenessWorker_ChargedRepair(t *testing.T) {
	f := newLivenessFixture(t, models.RailMobius, 3*24*time.Hour)
	txnID := "txn_repair_" + uuid.New().String()
	f.transactionXML = f.successSaleXML(txnID, time.Now().UTC().Add(-3*24*time.Hour))

	f.run(t)

	status, periodEnd, _, _ := f.subRow(t)
	assert.Equal(t, string(models.StatusActive), status)
	require.NotNil(t, periodEnd)
	assert.True(t, periodEnd.After(time.Now().UTC().Add(20*24*time.Hour)), "period must advance one 30d cycle from the lapsed end")

	var paymentCount int
	require.NoError(t, f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM openrails.payments WHERE subscription_id = $1 AND transaction_id = $2`, f.subID, txnID).Scan(&paymentCount))
	assert.Equal(t, 1, paymentCount, "verified charge must be backfilled as a payment")

	// Entitlements: a paid window for the repaired period plus the #368
	// trailing grace window starting exactly at the new paid end.
	var paidWindows, graceWindows int
	require.NoError(t, f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM openrails.entitlements WHERE source_id = $1 AND source_type = 'subscription' AND revoked_at IS NULL AND deleted_at IS NULL`, f.subID).Scan(&paidWindows))
	require.NoError(t, f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM openrails.entitlements WHERE source_id = $1 AND source_type = 'grace' AND revoked_at IS NULL AND deleted_at IS NULL`, f.subID).Scan(&graceWindows))
	assert.Equal(t, 1, paidWindows, "repair grants the paid window")
	assert.Equal(t, 1, graceWindows, "repair pre-appends the next renewal grace window (#368)")

	// Second pass: period end is now in the future, the cohort query no
	// longer returns the sub — exactly-once repair.
	f.run(t)
	require.NoError(t, f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM openrails.payments WHERE subscription_id = $1`, f.subID).Scan(&paymentCount))
	assert.Equal(t, 1, paymentCount, "a second pass must not duplicate the payment")

	assert.Zero(t, f.directPostHits.Load(), "the liveness worker must never send a provider mutation")
}

// TestLivenessWorker_DeclinedRoutesIntoDunning (#367): a verified soft
// decline moves the subscription to past_due with the #359 retry schedule —
// dunning owns it from here. No charge from this worker.
func TestLivenessWorker_DeclinedRoutesIntoDunning(t *testing.T) {
	f := newLivenessFixture(t, models.RailMobius, 2*24*time.Hour)
	f.transactionXML = fmt.Sprintf(`<?xml version="1.0"?>
<nm_response>
  <transaction>
    <transaction_id>txn_declined</transaction_id>
    <order_id>%s</order_id>
    <action><action_type>sale</action_type><success>0</success><date>%s</date><response_code>202</response_code><response_text>Insufficient funds</response_text></action>
  </transaction>
</nm_response>`, f.subID, time.Now().UTC().Add(-2*24*time.Hour).Format("20060102150405"))

	f.run(t)

	status, _, retryAttempts, nextRetryAt := f.subRow(t)
	assert.Equal(t, string(models.StatusPastDue), status, "verified decline routes into dunning")
	require.NotNil(t, retryAttempts)
	assert.Equal(t, int32(1), *retryAttempts)
	require.NotNil(t, nextRetryAt, "the #359 retry schedule must be set")
	assert.Zero(t, f.directPostHits.Load(), "the liveness worker must never send a provider mutation")
}

// TestLivenessWorker_MisalignmentAdoptsPeriodWithoutAccess (#367): no charge
// attempt, remote recurring record alive with a FUTURE next billing date —
// the remote period end is adopted locally and NO entitlement window is
// granted (adoption alone never grants access).
func TestLivenessWorker_MisalignmentAdoptsPeriodWithoutAccess(t *testing.T) {
	f := newLivenessFixture(t, models.RailMobius, 36*time.Hour)
	nextCharge := time.Now().UTC().Add(48 * time.Hour).Format("2006-01-02")
	f.recurringXML = fmt.Sprintf(`<?xml version="1.0"?>
<nm_response>
  <subscription>
    <subscription_id>%s</subscription_id>
    <next_charge_date>%s</next_charge_date>
  </subscription>
</nm_response>`, f.procSubID, nextCharge)

	f.run(t)

	status, periodEnd, _, _ := f.subRow(t)
	assert.Equal(t, string(models.StatusActive), status)
	require.NotNil(t, periodEnd)
	wantEnd, err := time.ParseInLocation("2006-01-02", nextCharge, time.UTC)
	require.NoError(t, err)
	assert.True(t, periodEnd.Equal(wantEnd), "remote next billing date must be adopted as the local period end (got %s want %s)", periodEnd, wantEnd)

	var entCount int
	require.NoError(t, f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM openrails.entitlements WHERE source_id = $1`, f.subID).Scan(&entCount))
	assert.Zero(t, entCount, "period adoption must never grant access")

	var paymentCount int
	require.NoError(t, f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM openrails.payments WHERE subscription_id = $1`, f.subID).Scan(&paymentCount))
	assert.Zero(t, paymentCount)
	assert.Zero(t, f.directPostHits.Load(), "the liveness worker must never send a provider mutation")
}

// TestLivenessWorker_MissingPeriodStillProbesProvider (#574): legacy imports can
// carry an active provider subscription without trustworthy local period
// evidence. The liveness lane must still ask the provider instead of skipping
// forever; with no period boundary it uses recurring liveness only.
func TestLivenessWorker_MissingPeriodStillProbesProvider(t *testing.T) {
	f := newLivenessFixture(t, models.RailMobius, 36*time.Hour)
	nextCharge := time.Now().UTC().Add(72 * time.Hour).Format("2006-01-02")
	f.recurringXML = fmt.Sprintf(`<?xml version="1.0"?>
<nm_response>
  <subscription>
    <subscription_id>%s</subscription_id>
    <next_charge_date>%s</next_charge_date>
  </subscription>
</nm_response>`, f.procSubID, nextCharge)
	_, err := f.pool.Exec(context.Background(), `UPDATE openrails.subscriptions SET current_period_ends_at = NULL WHERE id = $1`, f.subID)
	require.NoError(t, err)

	f.run(t)

	status, periodEnd, _, _ := f.subRow(t)
	assert.Equal(t, string(models.StatusActive), status)
	require.NotNil(t, periodEnd)
	wantEnd, err := time.ParseInLocation("2006-01-02", nextCharge, time.UTC)
	require.NoError(t, err)
	assert.True(t, periodEnd.Equal(wantEnd), "missing-period legacy row should adopt provider next billing date")
	assert.Zero(t, f.directPostHits.Load(), "the liveness worker must never send a provider mutation")
}

// TestLivenessWorker_RemoteAbsentCancelsAndRevokes (#367): the recurring
// report no longer lists the subscription (at NMI that IS terminal) — cancel
// locally + revoke subscription-sourced entitlements. No remote delete, no
// charge.
func TestLivenessWorker_RemoteAbsentCancelsAndRevokes(t *testing.T) {
	f := newLivenessFixture(t, models.RailMobius, 5*24*time.Hour)
	ctx := context.Background()

	// Give the user a lingering entitlement window from the subscription so
	// the revocation is observable.
	var tenantSubjectID uuid.UUID
	require.NoError(t, f.pool.QueryRow(ctx, `SELECT customer_id FROM openrails.subscriptions WHERE id = $1`, f.subID).Scan(&tenantSubjectID))
	_, err := f.pool.Exec(ctx,
		`INSERT INTO openrails.entitlements (id, entitlement, start_at, end_at, source_id, source_type, customer_id, merchant_id)
		 VALUES ($1, 'premium', now() - interval '40 days', now() + interval '10 days', $2, 'subscription', $3, $4)`,
		uuid.New(), f.subID, tenantSubjectID, dbtest.TestMerchantID.UUID())
	require.NoError(t, err)

	f.run(t)

	status, _, _, _ := f.subRow(t)
	assert.Equal(t, string(models.StatusCancelled), status, "remote-absent must cancel locally")

	var liveEnts int
	require.NoError(t, f.pool.QueryRow(ctx,
		`SELECT count(*) FROM openrails.entitlements WHERE source_id = $1 AND revoked_at IS NULL AND deleted_at IS NULL AND (end_at IS NULL OR end_at > now())`, f.subID).Scan(&liveEnts))
	assert.Zero(t, liveEnts, "entitlements must be revoked with the cancellation")

	var intentCount int
	require.NoError(t, f.pool.QueryRow(ctx,
		`SELECT count(*) FROM openrails.provider_intents WHERE subscription_id = $1`, f.subID).Scan(&intentCount))
	assert.Zero(t, intentCount, "remote-absent needs no delete intent — the remote sub is already gone")
	assert.Zero(t, f.directPostHits.Load(), "the liveness worker must never send a provider mutation")
}

// TestLivenessWorker_UnreachableProviderLeavesStateUntouched (#367): a
// failing provider read changes nothing; the state-scan re-derives the same
// cohort next pass — retry comes free, no durable read-queue.
func TestLivenessWorker_UnreachableProviderLeavesStateUntouched(t *testing.T) {
	f := newLivenessFixture(t, models.RailMobius, 2*24*time.Hour)
	f.queryStatus = http.StatusInternalServerError

	var beforeEnd *time.Time
	statusBefore, beforeEnd, _, _ := f.subRow(t)

	f.run(t)

	statusAfter, afterEnd, retryAttempts, _ := f.subRow(t)
	assert.Equal(t, statusBefore, statusAfter, "unreachable provider must leave status untouched")
	assert.True(t, beforeEnd.Equal(*afterEnd), "unreachable provider must leave the period untouched")
	assert.Nil(t, retryAttempts)

	// Provider back up with an answer: the next pass converges (proves the
	// cohort re-derivation retried the same subscription).
	f.queryStatus = 0
	txnID := "txn_late_" + uuid.New().String()
	f.transactionXML = f.successSaleXML(txnID, time.Now().UTC().Add(-24*time.Hour))
	f.run(t)
	statusFinal, _, _, _ := f.subRow(t)
	assert.Equal(t, string(models.StatusActive), statusFinal)
	var paymentCount int
	require.NoError(t, f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM openrails.payments WHERE subscription_id = $1 AND transaction_id = $2`, f.subID, txnID).Scan(&paymentCount))
	assert.Equal(t, 1, paymentCount)
	assert.Zero(t, f.directPostHits.Load())
}

// TestLivenessWorker_MonthsStaleNeverCharges (#367): the months-stale
// migration shape — a remote record stalled long ago. The liveness worker
// routes it into dunning WITHOUT any charge; dunning's staleness window then
// cancels rather than surprise-charging. This worker's no-charge property is
// structural: zero direct-post requests under every outcome.
func TestLivenessWorker_MonthsStaleNeverCharges(t *testing.T) {
	f := newLivenessFixture(t, models.RailMobius, 120*24*time.Hour)
	// Remote record exists but stalled months ago.
	f.recurringXML = fmt.Sprintf(`<?xml version="1.0"?>
<nm_response>
  <subscription>
    <subscription_id>%s</subscription_id>
    <next_charge_date>%s</next_charge_date>
  </subscription>
</nm_response>`, f.procSubID, time.Now().UTC().Add(-119*24*time.Hour).Format("2006-01-02"))

	f.run(t)

	status, _, _, _ := f.subRow(t)
	assert.Equal(t, string(models.StatusPastDue), status, "stalled remote record routes into dunning (whose staleness window cancels without charging)")
	assert.Zero(t, f.directPostHits.Load(), "months-stale subscriptions must NEVER be charged by the liveness worker")

	var paymentCount int
	require.NoError(t, f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM openrails.payments WHERE subscription_id = $1`, f.subID).Scan(&paymentCount))
	assert.Zero(t, paymentCount)
}

// fakeStripeProber returns a canned record.
type fakeStripeProber struct {
	rec subscriptions.StripeLivenessRecord
	err error
}

func (p *fakeStripeProber) ProbeSubscription(ctx context.Context, id string) (subscriptions.StripeLivenessRecord, error) {
	return p.rec, p.err
}

// TestLivenessWorker_StripeChargedRepair (#367): Stripe advanced the period
// on a paid invoice and invoice.paid was lost — repair with the remote period
// and the webhook-compatible transaction id.
func TestLivenessWorker_StripeChargedRepair(t *testing.T) {
	f := newLivenessFixture(t, models.RailStripe, 3*24*time.Hour)
	remoteStart := time.Now().UTC().Add(-2 * 24 * time.Hour).Truncate(time.Second)
	remoteEnd := remoteStart.Add(30 * 24 * time.Hour)
	f.worker.StripeProber = &fakeStripeProber{rec: subscriptions.StripeLivenessRecord{
		Found: true, Status: "active",
		CurrentPeriodStart: remoteStart, CurrentPeriodEnd: remoteEnd,
		LatestInvoicePaid: true, LatestInvoiceTransactionID: "ch_live_" + uuid.New().String(),
		LatestInvoiceAmountPaid: 999, LatestInvoiceCurrency: "usd",
	}}

	f.run(t)

	status, periodEnd, _, _ := f.subRow(t)
	assert.Equal(t, string(models.StatusActive), status)
	require.NotNil(t, periodEnd)
	assert.True(t, periodEnd.Equal(remoteEnd), "remote stripe period end must be adopted with the repair (got %s want %s)", periodEnd, remoteEnd)

	var paymentCount int
	require.NoError(t, f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM openrails.payments WHERE subscription_id = $1`, f.subID).Scan(&paymentCount))
	assert.Equal(t, 1, paymentCount)
}

// TestLivenessWorker_StripeCanceledRemotely (#367): Stripe reports the
// subscription canceled — converge locally.
func TestLivenessWorker_StripeCanceledRemotely(t *testing.T) {
	f := newLivenessFixture(t, models.RailStripe, 3*24*time.Hour)
	f.worker.StripeProber = &fakeStripeProber{rec: subscriptions.StripeLivenessRecord{Found: true, Status: "canceled"}}

	f.run(t)

	status, _, _, _ := f.subRow(t)
	assert.Equal(t, string(models.StatusCancelled), status)
}

// TestLivenessWorker_ReadonlyModeSkips (#346/#367): readonly boots are pure
// observers — the pass runs no probes and converges nothing.
func TestLivenessWorker_ReadonlyModeSkips(t *testing.T) {
	f := newLivenessFixture(t, models.RailMobius, 2*24*time.Hour)
	f.transactionXML = f.successSaleXML("txn_should_not_apply", time.Now().UTC().Add(-24*time.Hour))
	f.worker.Config = &config.Config{Mode: config.ModeReadOnly}

	f.run(t)

	status, _, _, _ := f.subRow(t)
	assert.Equal(t, string(models.StatusActive), status, "readonly mode must not converge state")
	var paymentCount int
	require.NoError(t, f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM openrails.payments WHERE subscription_id = $1`, f.subID).Scan(&paymentCount))
	assert.Zero(t, paymentCount)
}
