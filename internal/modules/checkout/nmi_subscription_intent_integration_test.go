//go:build integration

package checkout

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/pkg/merchant"
)

// stubIdemStore satisfies checkoutIdempotencyStore; the intent ledger is the
// durable truth in these tests.
type stubIdemStore struct{ completes atomic.Int64 }

func (s *stubIdemStore) Begin(context.Context, string, string) (*IdempotencyRecord, bool, error) {
	return nil, false, nil
}
func (s *stubIdemStore) Fail(context.Context, string, string, error) error { return nil }
func (s *stubIdemStore) Complete(context.Context, string, string, json.RawMessage) error {
	s.completes.Add(1)
	return nil
}

// fakeNMISubGateway scripts the three surfaces the create intent touches:
// classic add_subscription (direct post), the v5 subscription roster (verify
// scan), and the classic query search (verify sale probe).
type fakeNMISubGateway struct {
	createCalls atomic.Int64
	createMode  atomic.Value // "approve" | "ambiguous500"
	createForm  atomic.Value // url.Values: full form of the last create (#297 wire assertions)
	// remote state
	subExists       atomic.Bool
	railCustomerRef string
	planID          string
	subID           string
	txnID           string
	charged         atomic.Bool
}

func newFakeNMISubGateway(t *testing.T, railCustomerRef, planID string) (*fakeNMISubGateway, *nmi.NMIClient) {
	t.Helper()
	f := &fakeNMISubGateway{
		railCustomerRef: railCustomerRef, planID: planID,
		subID: "rsub-" + uuid.NewString()[:8],
		txnID: "txn-sub-" + uuid.NewString()[:8],
	}
	f.createMode.Store("approve")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/subscriptions") {
			// v5 roster
			if f.subExists.Load() {
				fmt.Fprintf(w, `{"subscriptions":[{"object":"subscription","id":"%s","customer_vault_id":"%s","delayed_condition":"active","plan":{"id":"%s"}}],"next_cursor":null,"has_more":false}`,
					f.subID, f.railCustomerRef, f.planID)
				return
			}
			fmt.Fprint(w, `{"subscriptions":[],"next_cursor":null,"has_more":false}`)
			return
		}
		_ = r.ParseForm()
		if r.Form.Get("recurring") == "add_subscription" {
			f.createCalls.Add(1)
			f.createForm.Store(r.Form)
			switch f.createMode.Load().(string) {
			case "ambiguous500":
				// The create LANDED but the response was lost.
				f.subExists.Store(true)
				f.charged.Store(true)
				w.WriteHeader(http.StatusBadGateway)
			default:
				f.subExists.Store(true)
				f.charged.Store(true)
				fmt.Fprintf(w, "response=1&responsetext=SUCCESS&subscription_id=%s&transactionid=%s&authcode=OK", f.subID, f.txnID)
			}
			return
		}
		// classic query.php transaction search
		orderID := r.Form.Get("order_id")
		if f.charged.Load() {
			fmt.Fprintf(w, `<nm_response><transaction><transaction_id>%s</transaction_id><order_id>%s</order_id><action><action_type>sale</action_type><success>1</success></action></transaction></nm_response>`, f.txnID, orderID)
			return
		}
		fmt.Fprint(w, `<nm_response></nm_response>`)
	}))
	t.Cleanup(srv.Close)

	client, err := nmi.NewClient("mobius", &config.NMIProviderSettings{
		SecurityKey: "test_security_key", WebhookSecret: "test_secret",
	}, true)
	require.NoError(t, err)
	client.V5BaseURL = srv.URL
	client.QueryURL = srv.URL
	client.DirectPostURL = srv.URL
	return f, client
}

type subIntentFixture struct {
	db      *db.DB
	runner  *intents.Runner
	gateway *fakeNMISubGateway
	svc     *CheckoutService
	payload NMISubscriptionCreatePayload
	priceID uuid.UUID
	ctx     context.Context
}

func newSubIntentFixture(t *testing.T) *subIntentFixture {
	t.Helper()
	dbi := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	pool := dbi.Pool()
	dbtest.EnsureTestMerchant(context.Background(), t, pool)
	ctx := merchant.WithID(context.Background(), dbtest.TestMerchantID)

	now := time.Now().UTC().Truncate(time.Second)
	userID := uuid.New().String()
	customerID := dbtest.EnsureCustomerIDPgx(ctx, t, pool, userID)
	productID, priceID := uuid.New(), uuid.New()
	insertProductAndPrice(ctx, t, pool, &models.Product{
		ID: productID, Key: "sub-intent-" + uuid.NewString()[:8], DisplayName: "Sub Intent Test",
		Archived: false, CreatedAt: now, UpdatedAt: now,
	}, &models.Price{
		ID: priceID, ProductID: productID, Archived: false,
		Amount: 9_990_000, Currency: "USD", AutoRenew: true, AccessDurationHours: intPtr(720),
		CreatedAt: now, UpdatedAt: now,
	})
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.rail_intents WHERE intent_type = 'nmi_subscription_create' AND price_id = $1", priceID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.entitlements WHERE customer_id = $1", customerID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.payments WHERE customer_id = $1", customerID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.notification_queue WHERE customer_id = $1", customerID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.subscriptions WHERE customer_id = $1", customerID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.prices WHERE id = $1", priceID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.products WHERE id = $1", productID)
	})

	railCustomerRef := "vault-" + uuid.NewString()[:8]
	planID := "plan-" + uuid.NewString()[:8]
	gateway, client := newFakeNMISubGateway(t, railCustomerRef, planID)
	clock := clockwork.NewRealClock()
	priceSvc := catalog.NewPriceService(dbi)
	productSvc := catalog.NewProductService(dbi)
	paymentSvc := payments.NewPaymentService(dbi, clock)
	entSvc := entitlements.NewEntitlementService(dbi, clock)
	subSvc := subscriptions.NewSubscriptionService(dbi, priceSvc, productSvc, nil, clock)
	svc := NewCheckoutService(subSvc, productSvc, priceSvc, paymentSvc, entSvc,
		nil, nil, &stubIdemStore{}, nil, nil, nil, clock)
	// #788: the scoped resolver is the ONLY NMI client source; the fixture
	// overrides it with the fake-gateway client.
	svc.ResolveNMIClientOverride = func(context.Context, string) (*nmi.NMIClient, error) { return client, nil }

	runner := &intents.Runner{
		Store:    intents.NewStore(dbi),
		Registry: intents.NewRegistry(NewNMISubscriptionCreateIntentHandler(svc)),
		// or#865: an unstated mode parks every intent — say "full" (see main_test.go).
		Config: fullModeConfig(),
	}
	return &subIntentFixture{
		db: dbi, runner: runner, gateway: gateway, svc: svc,
		payload: NMISubscriptionCreatePayload{
			Provider:               "mobius",
			PlanID:                 planID,
			CustomerVaultID:        railCustomerRef,
			AmountMicros:           9_990_000,
			Currency:               "USD",
			UserID:                 userID,
			PriceID:                priceID,
			LocalSubscriptionID:    uuid.New(),
			CheckoutIdempotencyKey: "sub-key-" + uuid.NewString()[:8],
			FirstName:              "T", LastName: "User", Address1: "N/A",
			City: "N/A", State: "N/A", Zip: "00000", Country: "US",
		},
		priceID: priceID, ctx: ctx,
	}
}

func (fx *subIntentFixture) enqueueAndExecute(t *testing.T) gen.OpenrailsRailIntent {
	t.Helper()
	pspID := dbtest.EnsureTestPSP(fx.ctx, t, fx.db.Pool(), dbtest.TestMerchantID.UUID(), "mobius")
	intent, err := fx.runner.EnqueueAndExecute(fx.ctx, intents.EnqueueParams{
		MerchantID:     dbtest.TestMerchantID.UUID(),
		Provider:       "mobius",
		IntentType:     TypeNMISubscriptionCreate,
		PriceID:        &fx.priceID,
		PspID:          pspID,
		Payload:        fx.payload,
		IdempotencyKey: NMISubscriptionCreateIdempotencyKey(fx.payload.CheckoutIdempotencyKey),
		NextAttemptAt:  time.Now().UTC(),
		Origin:         intents.OriginUser,
		OriginReason:   "test subscription create",
	})
	require.NoError(t, err)
	return intent
}

func (fx *subIntentFixture) localSub(t *testing.T) (*models.Subscription, bool) {
	t.Helper()
	sub, err := fx.svc.SubscriptionService.GetByRailSubscriptionID(fx.ctx, "mobius", fx.gateway.subID)
	if err != nil {
		return nil, false
	}
	return sub, true
}

// Happy path: one remote create, local subscription registered + activated,
// payment row recorded; replay answers from the durable row.
func TestNMISubscriptionIntent_HappyPathAndReplay(t *testing.T) {
	fx := newSubIntentFixture(t)

	intent := fx.enqueueAndExecute(t)
	require.Equal(t, intents.StatusSucceeded, intent.Status)
	require.EqualValues(t, 1, fx.gateway.createCalls.Load())
	sub, ok := fx.localSub(t)
	require.True(t, ok, "local subscription registered")
	require.Equal(t, models.StatusActive, sub.Status)

	replay := fx.enqueueAndExecute(t)
	require.Equal(t, intent.ID, replay.ID)
	require.Equal(t, intents.StatusSucceeded, replay.Status)
	require.EqualValues(t, 1, fx.gateway.createCalls.Load(), "replay never re-creates")
}

// THE orphan case (#674): timeout after NMI created the subscription. The old
// flow marked the attempt failed and told the user to retry with a NEW key —
// leaving a live remote subscription billing every cycle with no local row.
// Now: the intent parks ambiguous; the verifier re-finds the orphan via the
// vault+plan roster scan (and the first charge via the order-id sale search)
// and completes the local registration.
func TestNMISubscriptionIntent_OrphanedRemoteCreateIsRepaired(t *testing.T) {
	fx := newSubIntentFixture(t)
	fx.gateway.createMode.Store("ambiguous500")

	intent := fx.enqueueAndExecute(t)
	require.Equal(t, intents.StatusUnknownNeedsVerify, intent.Status, "lost response is never a decline")
	require.EqualValues(t, 1, fx.gateway.createCalls.Load())
	_, ok := fx.localSub(t)
	require.False(t, ok, "nothing registered yet")

	fx.runner.Clock = clockwork.NewFakeClockAt(time.Now().UTC().Add(2 * time.Minute))
	_, err := fx.runner.RunVerifyOnce(fx.ctx)
	require.NoError(t, err)

	final, err := intents.NewStore(fx.db).Get(fx.ctx, intent.ID)
	require.NoError(t, err)
	require.Equal(t, intents.StatusSucceeded, final.Status)
	require.EqualValues(t, 1, fx.gateway.createCalls.Load(), "repair never re-creates")
	sub, ok := fx.localSub(t)
	require.True(t, ok, "orphaned remote subscription registered locally")
	require.Equal(t, models.StatusActive, sub.Status)
}
