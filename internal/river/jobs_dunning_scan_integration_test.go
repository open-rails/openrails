//go:build integration

package riverjobs

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/riverqueue/river"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedScanSubscription inserts a subscription row shaped for the dunning scan
// filter matrix. pspID must already exist for (merchant, rail). Returns the
// subscription id.
func seedScanSubscription(ctx context.Context, t *testing.T, q *gen.Queries, productID, priceID, customerID, pspID uuid.UUID, rail string, status models.SubscriptionStatus, nextRetryAt *time.Time) uuid.UUID {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	periodEnd := now.Add(-24 * time.Hour)
	periodStart := periodEnd.Add(-30 * 24 * time.Hour)
	id := uuid.New()
	params := gen.CreateSubscriptionParams{
		ID: id, MerchantID: dbtest.TestMerchantID.UUID(), CustomerID: customerID, ProductID: productID, PriceID: &priceID,
		Status: string(status), Rail: rail,
		PspID:                 pspID,
		RailSubscriptionID:    "sub_scan_" + uuid.New().String(),
		CurrentPeriodStartsAt: &periodStart, CurrentPeriodEndsAt: &periodEnd, StartedAt: periodStart,
		NextRetryAt: nextRetryAt, CreatedAt: now, UpdatedAt: now,
	}
	if status == models.StatusCancelled {
		// chk_cancelled_no_retry_schedule: the schema itself already makes a
		// cancelled row with a retry schedule unrepresentable — the strongest
		// possible pin that terminal rows can never re-enter the work list.
		cancelType := string(models.CancelTypeUser)
		params.CancelledAt = &now
		params.EndedAt = &now
		params.CancelType = &cancelType
		params.NextRetryAt = nil
	}
	_, err := q.CreateSubscription(ctx, params)
	require.NoError(t, err)
	return id
}

// TestDunningScan_DueQueryFilters pins the dunning work-list filters
// (ListDueDunningSubscriptions) against real rows: ONLY a past_due
// subscription on a requested rail whose next_retry_at is due is returned —
// future retries, other statuses, other rails and retry-less rows are all
// excluded. Replaces the tests/ dunning_worker filter/skip tests (#694), which
// asserted the same filters indirectly through no-op worker runs.
func TestDunningScan_DueQueryFilters(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)
	ctx := dbtest.WithTestMerchant(context.Background())
	dbi := dbtest.OpenAppDB(t, dsn)
	// RLS-enforcing harness: every row here is the test merchant's own; the
	// merchants row itself is control plane.
	pool := dbtest.SharedMerchantPool(t, dbtest.TestMerchantID.UUID())
	q := dbtest.Queries(pool)
	dbtest.EnsureTestMerchant(ctx, t, dbtest.OpenAppDB(t, dbtest.SharedSuperuserDSN(t)).Pool())
	now := time.Now().UTC().Truncate(time.Second)

	productID, priceID := uuid.New(), uuid.New()
	description := "Scan filters"
	_, err := q.CreateProduct(ctx, gen.CreateProductParams{
		ID: productID, Key: "scan_product_" + uuid.New().String(), DisplayName: "Scan Product",
		MerchantID:  dbtest.TestMerchantID.UUID(),
		Description: &description, Archived: false, CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(t, err)
	billingHours := int32(720)
	_, err = q.CreatePrice(ctx, gen.CreatePriceParams{
		ID: priceID, ProductID: productID, Amount: 999, Currency: "USD",
		MerchantID: dbtest.TestMerchantID.UUID(),
		Archived:   false, AccessDurationHours: &billingHours, AutoRenew: true, CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(t, err)

	// One customer per row: live statuses share the one-live-sub-per-
	// (customer, product) lifecycle slot (uq_subscriptions_customer_product_lifecycle).
	newCustomer := func() uuid.UUID { return dbtest.EnsureCustomerIDPgx(ctx, t, pool, uuid.New().String()) }
	pastRetry := now.Add(-time.Hour)
	futureRetry := now.Add(24 * time.Hour)
	nmiPspID := dbtest.EnsureTestPSP(ctx, t, pool, dbtest.TestMerchantID.UUID(), string(models.RailNMI))
	ccbillPspID := dbtest.EnsureTestPSP(ctx, t, pool, dbtest.TestMerchantID.UUID(), string(models.RailCCBill))

	due := seedScanSubscription(ctx, t, q, productID, priceID, newCustomer(), nmiPspID, string(models.RailNMI), models.StatusPastDue, &pastRetry)
	notDueYet := seedScanSubscription(ctx, t, q, productID, priceID, newCustomer(), nmiPspID, string(models.RailNMI), models.StatusPastDue, &futureRetry)
	active := seedScanSubscription(ctx, t, q, productID, priceID, newCustomer(), nmiPspID, string(models.RailNMI), models.StatusActive, &pastRetry)
	cancelled := seedScanSubscription(ctx, t, q, productID, priceID, newCustomer(), nmiPspID, string(models.RailNMI), models.StatusCancelled, &pastRetry)
	otherRail := seedScanSubscription(ctx, t, q, productID, priceID, newCustomer(), ccbillPspID, string(models.RailCCBill), models.StatusPastDue, &pastRetry)
	noRetryAt := seedScanSubscription(ctx, t, q, productID, priceID, newCustomer(), nmiPspID, string(models.RailNMI), models.StatusPastDue, nil)
	seeded := map[uuid.UUID]string{
		due: "due", notDueYet: "notDueYet", active: "active",
		cancelled: "cancelled", otherRail: "otherRail", noRetryAt: "noRetryAt",
	}
	t.Cleanup(func() {
		for id := range seeded {
			_, _ = pool.Exec(ctx, "DELETE FROM billing.subscriptions WHERE id = $1", id)
		}
		_, _ = pool.Exec(ctx, "DELETE FROM billing.prices WHERE id = $1", priceID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.products WHERE id = $1", productID)
	})

	got := map[uuid.UUID]bool{}
	require.NoError(t, dbi.RunInMerchantConn(ctx, func(mctx context.Context) error {
		rows, qerr := subscriptions.NewSubscriptionRepo(dbi).ListDueDunningSubscriptions(mctx, []string{string(models.RailNMI)}, time.Now().UTC(), false)
		if qerr != nil {
			return qerr
		}
		for _, s := range rows {
			if _, mine := seeded[s.ID]; mine {
				got[s.ID] = true
			}
		}
		return nil
	}))
	assert.Equal(t, map[uuid.UUID]bool{due: true}, got,
		"exactly the due past_due NMI row is in the work list (no future retries, other statuses, other rails, or retry-less rows)")

	// Nil-clients guard: a deployment without NMI clients (e.g. Stripe-only)
	// skips the run cleanly instead of erroring the River job.
	worker := &DunningWorker{DB: dbi}
	require.NoError(t, worker.Work(context.Background(), &river.Job[DunningArgs]{Args: DunningArgs{}}))
}

func TestDunningWorker_ReadOnlyScansWithoutMutating(t *testing.T) {
	ctx := dbtest.WithTestMerchant(context.Background())
	dbi := dbtest.OpenAppDB(t, dbtest.SharedPostgresDSN(t))
	pool := dbtest.SharedMerchantPool(t, dbtest.TestMerchantID.UUID())
	q := dbtest.Queries(pool)
	dbtest.EnsureTestMerchant(ctx, t, dbtest.OpenAppDB(t, dbtest.SharedSuperuserDSN(t)).Pool())
	now := time.Now().UTC().Truncate(time.Second)

	productID, priceID := uuid.New(), uuid.New()
	description := "Readonly dunning"
	_, err := q.CreateProduct(ctx, gen.CreateProductParams{
		ID: productID, Key: "readonly_dunning_" + uuid.NewString(), DisplayName: "Readonly Dunning",
		MerchantID: dbtest.TestMerchantID.UUID(), Description: &description, CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(t, err)
	billingHours := int32(720)
	_, err = q.CreatePrice(ctx, gen.CreatePriceParams{
		ID: priceID, ProductID: productID, Amount: 999, Currency: "USD",
		MerchantID: dbtest.TestMerchantID.UUID(), AccessDurationHours: &billingHours, AutoRenew: true,
		CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(t, err)
	customerID := dbtest.EnsureCustomerIDPgx(ctx, t, pool, uuid.NewString())
	pspID := dbtest.EnsureTestPSP(ctx, t, pool, dbtest.TestMerchantID.UUID(), string(models.RailNMI))
	nextRetryAt := now.Add(-time.Hour)
	subID := seedScanSubscription(ctx, t, q, productID, priceID, customerID, pspID, string(models.RailNMI), models.StatusPastDue, &nextRetryAt)

	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM billing.rail_intents WHERE subscription_id = $1", subID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.subscriptions WHERE id = $1", subID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.prices WHERE id = $1", priceID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.products WHERE id = $1", productID)
	})

	var beforeStatus string
	var beforeNextRetryAt *time.Time
	var beforeUpdatedAt time.Time
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT status, next_retry_at, updated_at FROM billing.subscriptions WHERE id=$1`, subID).
		Scan(&beforeStatus, &beforeNextRetryAt, &beforeUpdatedAt))

	hook := logtest.NewGlobal()
	t.Cleanup(hook.Reset)
	worker := &DunningWorker{
		DB:          dbi,
		NMIResolver: fakeDunningNMIResolver{},
		Config:      &config.Config{ProviderWriteMode: config.ProviderWriteModeReadOnly},
	}
	require.NoError(t, worker.Work(context.Background(), &river.Job[DunningArgs]{Args: DunningArgs{}}))

	foundScan := false
	for _, entry := range hook.AllEntries() {
		if entry.Message == "Readonly mode: found due subscriptions but skipping dunning mutations" {
			foundScan = true
			break
		}
	}
	require.True(t, foundScan, "read-only mode must still enumerate and scan due subscriptions")

	var afterStatus string
	var afterNextRetryAt *time.Time
	var afterUpdatedAt time.Time
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT status, next_retry_at, updated_at FROM billing.subscriptions WHERE id=$1`, subID).
		Scan(&afterStatus, &afterNextRetryAt, &afterUpdatedAt))
	require.Equal(t, beforeStatus, afterStatus)
	require.Equal(t, beforeNextRetryAt, afterNextRetryAt)
	require.Equal(t, beforeUpdatedAt, afterUpdatedAt)

	var intentCount int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM billing.rail_intents WHERE subscription_id=$1`, subID).Scan(&intentCount))
	require.Zero(t, intentCount, "read-only scan must not materialize a charge intent")
}
