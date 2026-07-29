//go:build integration

package webhooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/stretchr/testify/require"
)

// #684: NMI signup activation, converged. The recurring.subscription.add /
// transaction.sale.success webhooks are wake-up signals only — activation
// happens off the FETCHED settled charge (query.php by order reference) and
// the FETCHED recurring record (v5 GET), through NMIConvergeService. The
// provider is a real httptest server speaking the NMI wire shapes; direct-post
// mutations must never happen (counter stays zero).

type nmiConvergeFixture struct {
	svc            *NMIConvergeService
	dbi            *db.DB
	clock          *clockwork.FakeClock
	directPostHits *atomic.Int64
	// transactionXML answers query.php; subscriptionJSON answers the v5 GET
	// ("" = 404, gone at NMI); queryStatus != 0 forces an HTTP error.
	transactionXML   string
	subscriptionJSON string
	queryStatus      int

	userID          string
	tenantSubjectID uuid.UUID
	subscriptionID  uuid.UUID
	providerSubID   string
	productID       uuid.UUID
	priceID         uuid.UUID
}

func nmiConvergeSaleXML(txnID, orderID, success string, at time.Time, amount string) string {
	return fmt.Sprintf(`<?xml version="1.0"?>
<nm_response>
  <transaction>
    <transaction_id>%s</transaction_id>
    <order_id>%s</order_id>
    <currency>USD</currency>
    <action><amount>%s</amount><action_type>sale</action_type><success>%s</success><date>%s</date><response_code>202</response_code><response_text>Insufficient funds</response_text></action>
  </transaction>
</nm_response>`, txnID, orderID, amount, success, at.UTC().Format("20060102150405"))
}

func newNMIConvergeFixture(t *testing.T, dsn string, subStatus models.SubscriptionStatus) *nmiConvergeFixture {
	t.Helper()

	ctx := context.Background()
	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbi.Pool()
	q := gen.New(pool)

	// Real "now": the NMI prober classifies roster liveness against the wall
	// clock (next charge before today's boundary = stalled), so a historical
	// fake clock would misclassify a live roster as past_due.
	now := time.Now().UTC().Truncate(time.Second)
	f := &nmiConvergeFixture{
		dbi:            dbi,
		clock:          clockwork.NewFakeClockAt(now),
		directPostHits: &atomic.Int64{},
		transactionXML: `<?xml version="1.0"?><nm_response></nm_response>`,
		userID:         uuid.New().String(),
		subscriptionID: uuid.New(),
		providerSubID:  "nmi_sub_" + uuid.New().String(),
		productID:      uuid.New(),
		priceID:        uuid.New(),
	}
	f.tenantSubjectID = dbtest.EnsureCustomerIDPgx(ctx, t, pool, f.userID)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet { // v5 GET /subscriptions/{id}
			if f.queryStatus != 0 {
				w.WriteHeader(f.queryStatus)
				return
			}
			if f.subscriptionJSON == "" {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"type":"notFound","error_code":"E_NOT_FOUND","message":"subscription not found"}`))
				return
			}
			_, _ = w.Write([]byte(f.subscriptionJSON))
			return
		}
		_ = r.ParseForm()
		if r.Form.Get("report_type") == "" { // Direct Post (mutations)
			f.directPostHits.Add(1)
			_, _ = w.Write([]byte("response=1"))
			return
		}
		if f.queryStatus != 0 {
			w.WriteHeader(f.queryStatus)
			return
		}
		_, _ = w.Write([]byte(f.transactionXML))
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() {
		require.Zero(t, f.directPostHits.Load(), "fetch-and-converge must never send a provider mutation")
	})

	client, err := nmi.NewClient("mobius", &config.NMIProviderSettings{SecurityKey: "k", WebhookSecret: "s"}, true)
	require.NoError(t, err)
	client.DirectPostURL = srv.URL
	client.QueryURL = srv.URL
	client.V5BaseURL = srv.URL

	entitlementsSpec := map[string]*int{"premium": nil}
	entitlementsSpecJSON, err := json.Marshal(entitlementsSpec)
	require.NoError(t, err)

	description := "Test premium product"
	_, err = q.CreateProduct(ctx, gen.CreateProductParams{
		MerchantID:       dbtest.TestMerchantID.UUID(),
		ID:               f.productID,
		Key:              "nmi_converge_" + uuid.New().String(),
		DisplayName:      "Premium Membership",
		Description:      &description,
		EntitlementsSpec: entitlementsSpecJSON,
		Archived:         false,
		CreatedAt:        now,
		UpdatedAt:        now,
	})
	require.NoError(t, err)

	billingCycleHours := int32(720)
	_, err = q.CreatePrice(ctx, gen.CreatePriceParams{
		MerchantID:          dbtest.TestMerchantID.UUID(),
		ID:                  f.priceID,
		ProductID:           f.productID,
		Amount:              23_990_000,
		Currency:            "USD",
		Archived:            false,
		AccessDurationHours: &billingCycleHours,
		AutoRenew:           true,
		CreatedAt:           now,
		UpdatedAt:           now,
	})
	require.NoError(t, err)

	periodStart := now.Add(-30 * 24 * time.Hour)
	periodEnd := now.Add(5 * 24 * time.Hour)
	createParams := gen.CreateSubscriptionParams{
		MerchantID:               dbtest.TestMerchantID.UUID(),
		ID:                       f.subscriptionID,
		CustomerID:               f.tenantSubjectID,
		ProductID:                f.productID,
		PriceID:                  &f.priceID,
		EntitlementsSpecSnapshot: entitlementsSpecJSON,
		Status:                   string(subStatus),
		Rail:                     string(models.RailNMI),
		RailSubscriptionID:       f.providerSubID,
		StartedAt:                now,
		CreatedAt:                now,
		UpdatedAt:                now,
	}
	if subStatus != models.StatusPending {
		createParams.CurrentPeriodStartsAt = &periodStart
		createParams.CurrentPeriodEndsAt = &periodEnd
	}
	_, err = q.CreateSubscription(ctx, createParams)
	require.NoError(t, err)

	t.Cleanup(func() {
		cctx := context.Background()
		_, _ = pool.Exec(cctx, "DELETE FROM openrails.notification_queue WHERE customer_id = $1", f.tenantSubjectID)
		_, _ = pool.Exec(cctx, "DELETE FROM openrails.entitlements WHERE customer_id = $1", f.tenantSubjectID)
		_, _ = pool.Exec(cctx, "DELETE FROM openrails.payments WHERE customer_id = $1", f.tenantSubjectID)
		_, _ = pool.Exec(cctx, "DELETE FROM openrails.subscriptions WHERE id = $1", f.subscriptionID)
		_, _ = pool.Exec(cctx, "DELETE FROM openrails.prices WHERE id = $1", f.priceID)
		_, _ = pool.Exec(cctx, "DELETE FROM openrails.products WHERE id = $1", f.productID)
	})

	priceSvc := catalog.NewPriceService(dbi)
	productSvc := catalog.NewProductService(dbi)
	entitlementSvc := entitlements.NewEntitlementService(dbi, f.clock)
	paymentSvc := payments.NewPaymentService(dbi, f.clock)
	subscriptionSvc := subscriptions.NewSubscriptionService(dbi, priceSvc, productSvc, nil, nil, nil, f.clock)
	lifecycleSvc := subscriptions.NewSubscriptionLifecycleService(dbi, productSvc, priceSvc, entitlementSvc, nil, paymentSvc, f.clock)

	f.svc = &NMIConvergeService{
		DB:                           dbi,
		Clock:                        f.clock,
		Rail:                         string(models.RailNMI),
		NMIClient:                    client,
		PriceService:                 priceSvc,
		SubscriptionService:          subscriptionSvc,
		SubscriptionLifecycleService: lifecycleSvc,
		PaymentService:               paymentSvc,
	}
	return f
}

func (f *nmiConvergeFixture) status(t *testing.T, ctx context.Context) string {
	t.Helper()
	row, err := gen.New(f.dbi.Pool()).GetSubscriptionByID(ctx, f.subscriptionID)
	require.NoError(t, err)
	return string(row.Status)
}

// End-to-end activation: a pending signup + a fetched settled charge for the
// subscription's order reference activates the membership, records the payment
// row, and grants the entitlement — fetch-sourced, never payload-sourced.
// Duplicate wake-ups are no-ops.
func TestNMIConvergeActivatesPendingFromFetchedCharge(t *testing.T) {
	dsn := dbtest.MerchantPinnedDSN(t, dbtest.TestMerchantID.UUID())
	f := newNMIConvergeFixture(t, dsn, models.StatusPending)
	ctx := dbtest.WithTestMerchant(context.Background())
	pool := f.dbi.Pool()

	txnID := "txn_" + uuid.New().String()
	chargedAt := f.clock.Now().UTC().Add(-time.Hour)
	// The order reference NMI carries is the LOCAL subscription id (stamped at
	// signup as orderid/ponumber).
	f.transactionXML = nmiConvergeSaleXML(txnID, f.subscriptionID.String(), "1", chargedAt, "23.99")
	// Remote recurring record alive with a future boundary (for post-activation converges).
	f.subscriptionJSON = fmt.Sprintf(`{"object":"subscription","id":"%s","next_billing_date":"%s"}`,
		f.providerSubID, f.clock.Now().UTC().Add(30*24*time.Hour).Format("2006-01-02"))

	_, err := f.svc.Converge(ctx, f.providerSubID)
	require.NoError(t, err)
	require.Equal(t, string(models.StatusActive), f.status(t, ctx))

	var paymentCustomerID uuid.UUID
	var paymentSubscriptionID *uuid.UUID
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT customer_id, subscription_id FROM openrails.payments WHERE transaction_id = $1",
		txnID).Scan(&paymentCustomerID, &paymentSubscriptionID))
	require.Equal(t, f.tenantSubjectID, paymentCustomerID)
	require.NotNil(t, paymentSubscriptionID)
	require.Equal(t, f.subscriptionID, *paymentSubscriptionID)

	var entitled bool
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT EXISTS (
			SELECT 1 FROM openrails.entitlements
			WHERE customer_id = $1 AND entitlement = 'premium'
			  AND source_type = $2 AND source_id = $3
			  AND revoked_at IS NULL AND deleted_at IS NULL
		)`, f.tenantSubjectID, string(models.EntitlementSourceSubscription), f.subscriptionID).Scan(&entitled))
	require.True(t, entitled)

	// Duplicate wake-up (the sale.success webhook echoing the same charge, in
	// any order): fetches the same truth, changes nothing.
	_, err = f.svc.Converge(ctx, f.providerSubID)
	require.NoError(t, err)
	var count int
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT count(*) FROM openrails.payments WHERE customer_id = $1", f.tenantSubjectID).Scan(&count))
	require.Equal(t, 1, count, "duplicate converge must not duplicate the payment")
	require.Equal(t, string(models.StatusActive), f.status(t, ctx))
}

// A pending signup with NO fetched charge attempt yet (settlement lag) parks
// as retry-later; the subscription stays pending, nothing is fabricated.
func TestNMIConvergePendingWithoutChargeRetriesLater(t *testing.T) {
	dsn := dbtest.MerchantPinnedDSN(t, dbtest.TestMerchantID.UUID())
	f := newNMIConvergeFixture(t, dsn, models.StatusPending)
	ctx := dbtest.WithTestMerchant(context.Background())

	_, err := f.svc.Converge(ctx, f.providerSubID)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrConvergeRetryLater), "settlement lag must snooze, got %v", err)
	require.Equal(t, string(models.StatusPending), f.status(t, ctx))
}

// NMI v5 404 IS provider truth (cancelled records are deleted at NMI): the
// REAL decider turns provider-confirmed-gone into a terminal cancel.
func TestNMIConvergeFetch404IsProviderConfirmedGone(t *testing.T) {
	dsn := dbtest.MerchantPinnedDSN(t, dbtest.TestMerchantID.UUID())
	f := newNMIConvergeFixture(t, dsn, models.StatusActive)
	ctx := dbtest.WithTestMerchant(context.Background())

	// subscriptionJSON stays "" → v5 GET answers 404; no sale rows either.
	_, err := f.svc.Converge(ctx, f.providerSubID)
	require.NoError(t, err)
	require.Equal(t, string(models.StatusCancelled), f.status(t, ctx))
}

// Provider API down: the converge fails retryably and local state (access)
// stays intact; a later converge against healthy truth proceeds.
func TestNMIConvergeProviderDownParks(t *testing.T) {
	dsn := dbtest.MerchantPinnedDSN(t, dbtest.TestMerchantID.UUID())
	f := newNMIConvergeFixture(t, dsn, models.StatusActive)
	ctx := dbtest.WithTestMerchant(context.Background())

	f.queryStatus = http.StatusInternalServerError
	_, err := f.svc.Converge(ctx, f.providerSubID)
	require.Error(t, err, "provider outage must fail the converge for retry")
	require.Equal(t, string(models.StatusActive), f.status(t, ctx), "outage must not move local state")

	f.queryStatus = 0
	f.subscriptionJSON = fmt.Sprintf(`{"object":"subscription","id":"%s","next_billing_date":"%s"}`,
		f.providerSubID, f.clock.Now().UTC().Add(30*24*time.Hour).Format("2006-01-02"))
	_, err = f.svc.Converge(ctx, f.providerSubID)
	require.NoError(t, err)
	require.Equal(t, string(models.StatusActive), f.status(t, ctx))
}
