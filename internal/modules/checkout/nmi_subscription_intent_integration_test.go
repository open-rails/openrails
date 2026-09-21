//go:build integration

package checkout

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
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
	recurringAmount string
	observedAmount  string
	observedAt      time.Time
	beforeResponse  func() error
}

func newFakeNMISubGateway(t *testing.T, railCustomerRef, planID string) (*fakeNMISubGateway, *nmi.NMIClient) {
	t.Helper()
	f := &fakeNMISubGateway{
		railCustomerRef: railCustomerRef, planID: planID,
		subID: "rsub-" + uuid.NewString()[:8],
		txnID: "txn-sub-" + uuid.NewString()[:8],
	}
	f.createMode.Store("approve")
	f.recurringAmount = "9.99"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/customers/") {
			fmt.Fprintf(w, `{"object":"customer","id":"%s","billing":[{"id":"billing-native","priority":1}]}`, f.railCustomerRef)
			return
		}
		form, _ := f.createForm.Load().(url.Values)
		amount := form.Get("amount")
		if f.observedAmount != "" {
			amount = f.observedAmount
		}
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/payments/") {
			if f.txnID == "" || !f.charged.Load() {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			fmt.Fprintf(w, `{"object":"transaction","id":"%s","response":"1","amount":"%s","currency":"USD","customer_vault_id":"%s","actions":[{"id":"%s","type":"sale","success":true,"amount":"%s"}]}`, f.txnID, amount, f.railCustomerRef, f.txnID, amount)
			return
		}

		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/subscriptions/") {
			if !strings.HasSuffix(r.URL.Path, "/subscriptions/"+f.subID) || !f.subExists.Load() {
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprint(w, `{"type":"notFound","error_code":"E_NOT_FOUND","message":"not found"}`)
				return
			}
			fmt.Fprintf(w, `{"object":"subscription","id":"%s","customer_vault_id":"%s","delayed_condition":"active","paused_subscription":"0","next_billing_date":"%s","plan":{"id":"%s","plan_amount":"%s","day_frequency":"30","plan_payments":"0"}}`, f.subID, f.railCustomerRef, form.Get("start_date")[:4]+"-"+form.Get("start_date")[4:6]+"-"+form.Get("start_date")[6:], f.planID, f.recurringAmount)
			return
		}
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
		if r.Form.Get("report_type") == "recurring" {
			start := form.Get("start_date")
			fmt.Fprintf(w, `<nm_response><subscription id="%s"><subscription_id>%s</subscription_id><plan><plan_id>%s</plan_id></plan><orderid>%s</orderid><ponumber>%s</ponumber><next_charge_date>%s</next_charge_date></subscription></nm_response>`, f.subID, f.subID, f.planID, form.Get("orderid"), form.Get("ponumber"), start[:4]+"-"+start[4:6]+"-"+start[6:])
			return
		}
		if r.Form.Get("recurring") == "add_subscription" {
			f.createCalls.Add(1)
			f.createForm.Store(r.Form)
			switch f.createMode.Load().(string) {
			case "decline":
				fmt.Fprint(w, "response=2&response_code=200&responsetext=DECLINED")
			case "ambiguous500":
				// The create LANDED but the response was lost.
				f.subExists.Store(true)
				f.charged.Store(r.Form.Get("type") == "sale" && f.txnID != "")
				w.WriteHeader(http.StatusBadGateway)
			default:
				f.subExists.Store(true)
				f.charged.Store(r.Form.Get("type") == "sale" && f.txnID != "")
				if f.beforeResponse != nil {
					if err := f.beforeResponse(); err != nil {
						http.Error(w, err.Error(), http.StatusInternalServerError)
						return
					}
				}
				fmt.Fprintf(w, "response=1&responsetext=SUCCESS&subscription_id=%s&transactionid=%s&authcode=OK", f.subID, f.txnID)
			}
			return
		}
		// classic query.php transaction search
		orderID := r.Form.Get("order_id")
		if f.charged.Load() && orderID == form.Get("orderid") {
			at := f.observedAt
			if at.IsZero() {
				at = time.Now().UTC()
			}
			fmt.Fprintf(w, `<nm_response><transaction><transaction_id>%s</transaction_id><order_id>%s</order_id><currency>USD</currency><action><action_type>sale</action_type><success>1</success><amount>%s</amount><date>%s</date></action></transaction></nm_response>`, f.txnID, orderID, amount, at.UTC().Format("20060102150405"))
			return
		}
		fmt.Fprint(w, `<nm_response></nm_response>`)
	}))
	t.Cleanup(srv.Close)

	client, err := nmi.NewAccountClient(dbtest.TestMerchantID.UUID(), dbtest.TestPSPID(dbtest.TestMerchantID.UUID(), "mobius"), "mobius", &config.NMIProviderSettings{
		SecurityKey: "test_security_key", WebhookSecret: "test_secret",
	}, true)
	require.NoError(t, err)
	client.V5BaseURL = srv.URL
	client.QueryURL = srv.URL
	client.DirectPostURL = srv.URL
	require.NoError(t, validateInitialFixtureDestinations(client))
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
		_, _ = pool.Exec(ctx, "DELETE FROM billing.rail_intents WHERE intent_type = 'nmi_subscription_create' AND price_id = $1", priceID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.entitlements WHERE customer_id = $1", customerID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.payments WHERE customer_id = $1", customerID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.notifications WHERE customer_id = $1", customerID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.subscriptions WHERE customer_id = $1", customerID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.prices WHERE id = $1", priceID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.products WHERE id = $1", productID)
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
	svc.SetSubscriptionLifecycleService(subscriptions.NewSubscriptionLifecycleService(
		dbi, productSvc, priceSvc, entSvc, subscriptions.NewNotificationService(dbi, nil), paymentSvc, clock))
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
			Provider:               string(models.RailNMI),
			PSP:                    "mobius",
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
	if fx.payload.Terms.SubscriptionID == uuid.Nil {
		price, err := fx.svc.PriceService.GetByID(fx.ctx, fx.priceID)
		require.NoError(t, err)
		product, err := fx.svc.ProductService.GetByID(fx.ctx, price.ProductID)
		require.NoError(t, err)
		method := models.PaymentMethod{ID: uuid.New(), CustomerID: uuid.MustParse(fx.payload.UserID), PspID: pspID, Rail: "nmi", Custodian: "psp", RailCustomerRef: fx.payload.CustomerVaultID, RailMethodRef: "billing-native"}
		require.NoError(t, paymentmethods.NewPaymentMethodRepo(fx.db).Create(db.WithPSPID(fx.ctx, pspID), &method))
		fx.payload.PaymentMethodID = &method.ID
		fx.payload.BillingID = method.RailMethodRef
		fx.payload.Instrument = charge.FrozenInstrument{PSPID: pspID, Custodian: "psp", RailCustomerRef: method.RailCustomerRef, RailMethodRef: method.RailMethodRef}
		now := fx.svc.now().UTC().Truncate(time.Microsecond)
		start := now
		if fx.payload.DelayedStart != nil {
			start = fx.payload.DelayedStart.UTC()
			fx.payload.AmountMicros = 0
		}
		end := start.Add(720 * time.Hour)
		if fx.payload.StartDate == "" {
			fx.payload.StartDate = end.Format("20060102")
		}
		payment := uuid.Nil
		if fx.payload.AmountMicros > 0 {
			payment = uuid.New()
		}
		benefits := models.CloneEntitlementsSpec(product.EntitlementsSpec)
		if benefits == nil {
			benefits = map[string]*int{}
		}
		fx.payload.Terms = subscriptions.InitialMembershipTerms{CollectionPolicy: models.CollectionPolicyProvider, SubscriptionID: fx.payload.LocalSubscriptionID, PaymentID: payment, CustomerID: method.CustomerID, PSPID: pspID, ProductID: product.ID, PriceID: price.ID, PaymentMethodID: method.ID, ProductName: product.DisplayName, Amount: fx.payload.AmountMicros, RecurringAmount: price.Amount, Currency: price.Currency, AcceptedAt: now, PeriodStart: start, PeriodEnd: end, Pending: fx.payload.DelayedStart != nil, Entitlements: benefits}
		fx.payload.DayFrequency = 30
		fx.payload.RequestFingerprint = "fixture-" + fx.payload.CheckoutIdempotencyKey
		if price.Amount == 0 {
			fx.gateway.recurringAmount = "0.00"
		}
	}

	intent, err := fx.runner.EnqueueAndExecute(fx.ctx, intents.EnqueueParams{
		MerchantID:     dbtest.TestMerchantID.UUID(),
		Provider:       string(models.RailNMI),
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
	sub, err := fx.svc.SubscriptionService.GetByPSPSubscriptionID(db.WithPSPID(fx.ctx, dbtest.TestPSPID(dbtest.TestMerchantID.UUID(), "mobius")), string(models.RailNMI), fx.gateway.subID)
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

// THE orphan case (#674): timeout after NMI created the subscription. The
// roster shows a subscription on the same vault and plan, but the roster does
// not carry the enrollment's order reference, so that similarity is only an
// operator candidate. The operation stays unknown across restart until the
// exact subscription id, read back on vault and plan, resolves it; the create
// is never re-sent and registration happens once.
func TestNMISubscriptionIntent_OrphanedRemoteCreateNeedsExactReceipt(t *testing.T) {
	fx := newSubIntentFixture(t)
	fx.gateway.createMode.Store("ambiguous500")

	intent := fx.enqueueAndExecute(t)
	require.Equal(t, intents.StatusUnknownNeedsVerify, intent.Status, "lost response is never a decline")
	require.EqualValues(t, 1, fx.gateway.createCalls.Load())

	fx.runner.Store = intents.NewStore(fx.db)
	fx.runner.Clock = clockwork.NewFakeClockAt(time.Now().UTC().Add(2 * time.Minute))
	_, err := fx.runner.RunVerifyOnce(fx.ctx)
	require.NoError(t, err)
	pending, err := intents.NewStore(fx.db).Get(fx.ctx, intent.ID)
	require.NoError(t, err)
	require.Equal(t, intents.StatusUnknownNeedsVerify, pending.Status, "roster similarity is not a receipt")
	require.Contains(t, string(pending.ResultEvidence), fx.gateway.subID, "the candidate is surfaced for the operator")
	_, ok := fx.localSub(t)
	require.False(t, ok)
	resumed := pending
	resumed.Attempts = 2
	outcome := fx.runner.Registry.Lookup(TypeNMISubscriptionCreate).Execute(fx.ctx, resumed)
	require.Equal(t, intents.OutcomeAmbiguous, outcome.Class)

	_, err = fx.runner.Resolve(fx.ctx, intent.ID, intents.Resolution{NotExecuted: true, Actor: "ops", Reason: "wrong"})
	require.ErrorIs(t, err, intents.ErrResolutionRejected, "the enrollment charge contradicts non-execution")
	_, err = fx.runner.Resolve(fx.ctx, intent.ID, intents.Resolution{ProviderReference: "rsub-missing", Actor: "ops", Reason: "wrong"})
	require.ErrorIs(t, err, intents.ErrResolutionRejected)
	resolved, err := fx.runner.Resolve(fx.ctx, intent.ID, intents.Resolution{ProviderReference: fx.gateway.subID, Actor: "ops@example.test", Reason: "NMI subscription detail shows order"})
	require.NoError(t, err)
	require.Equal(t, intents.StatusSucceeded, resolved.Status)
	sub, ok := fx.localSub(t)
	require.True(t, ok, "resolved remote subscription registered locally")
	require.Equal(t, models.StatusActive, sub.Status)

	replay := fx.enqueueAndExecute(t)
	require.Equal(t, intents.StatusSucceeded, replay.Status)
	_, err = fx.runner.RunVerifyOnce(fx.ctx)
	require.NoError(t, err)
	require.EqualValues(t, 1, fx.gateway.createCalls.Load(), "resolution never re-creates")
	var count int
	require.NoError(t, fx.db.Pool().QueryRow(fx.ctx, `SELECT count(*) FROM billing.subscriptions WHERE rail_subscription_id=$1`, fx.gateway.subID).Scan(&count))
	require.Equal(t, 1, count)
}

func TestNMISubscriptionIntent_ImmediateActivationIsOneMembership(t *testing.T) {
	fx := newSubIntentFixture(t)
	pool := fx.db.Pool()
	_, err := pool.Exec(fx.ctx, `UPDATE billing.products SET entitlements_spec = '{"premium": null}'
		WHERE id = (SELECT product_id FROM billing.prices WHERE id = $1)`, fx.priceID)
	require.NoError(t, err)

	intent := fx.enqueueAndExecute(t)
	require.Equal(t, intents.StatusSucceeded, intent.Status)
	sub, ok := fx.localSub(t)
	require.True(t, ok)
	require.Equal(t, fx.payload.LocalSubscriptionID, sub.ID)
	require.Equal(t, models.StatusActive, sub.Status)
	require.NotNil(t, sub.CurrentPeriodStartsAt)
	require.NotNil(t, sub.CurrentPeriodEndsAt)
	require.Equal(t, 720*time.Hour, sub.CurrentPeriodEndsAt.Sub(*sub.CurrentPeriodStartsAt))

	assertOneMembership := func() {
		t.Helper()
		var windows int
		require.NoError(t, pool.QueryRow(fx.ctx, `SELECT count(*) FROM billing.entitlements
			WHERE source_type = 'subscription' AND source_id = $1 AND entitlement = 'premium'`, sub.ID).Scan(&windows))
		require.Equal(t, 1, windows)
		var txnID, orderID string
		require.NoError(t, pool.QueryRow(fx.ctx, `SELECT transaction_id, metadata->>'order_id' FROM billing.payments
			WHERE subscription_id = $1 AND status = 'completed'`, sub.ID).Scan(&txnID, &orderID))
		require.Equal(t, fx.gateway.txnID, txnID)
		require.NotEmpty(t, orderID)
		rows, err := pool.Query(fx.ctx, `SELECT to_status::text FROM billing.subscription_status_transitions
			WHERE subscription_id = $1 ORDER BY occurred_at, id`, sub.ID)
		require.NoError(t, err)
		defer rows.Close()
		var transitions []string
		for rows.Next() {
			var s string
			require.NoError(t, rows.Scan(&s))
			transitions = append(transitions, s)
		}
		require.Equal(t, []string{"pending", "active"}, transitions)
	}
	assertOneMembership()

	replay := fx.enqueueAndExecute(t)
	require.Equal(t, intents.StatusSucceeded, replay.Status)
	assertOneMembership()
}

// Check every configured NMI surface, including readback URLs, before a fixture
// can run. This catches a missing override without attempting the real URL.
func validateInitialFixtureDestinations(client *nmi.NMIClient) error {
	for _, raw := range []string{client.DirectPostURL, client.QueryURL, client.V5BaseURL} {
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "http" || net.ParseIP(u.Hostname()) == nil || !net.ParseIP(u.Hostname()).IsLoopback() {
			return fmt.Errorf("initial fixture refuses non-loopback NMI destination")
		}
	}
	return nil
}

func TestInitialFixtureRejectsRealProviderDestination(t *testing.T) {
	_, client := newFakeNMISubGateway(t, "vault", "plan")
	client.QueryURL = "https://secure.nmi.com/api/query.php"
	require.Error(t, validateInitialFixtureDestinations(client))
}
