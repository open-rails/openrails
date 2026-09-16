//go:build integration

package checkout

import (
	"context"
	"errors"
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
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/pkg/api"
)

// fakeNMIUpgradeGateway scripts successor creation, proration submission,
// delayed charge visibility, and a deliberately non-authoritative roster.
type fakeNMIUpgradeGateway struct {
	railCustomerRef string
	planID          string
	subID           string

	saleCalls   atomic.Int64
	saleMode    atomic.Value
	saleVisible atomic.Bool
	saleTxn     string
	createCalls atomic.Int64
	createMode  atomic.Value // "approve" | "ambiguousLanded" | "ambiguousLost"
	lastOrder   atomic.Value // string
	subExists   atomic.Bool
	subDeletes  atomic.Int64
}

func newFakeNMIUpgradeGateway(t *testing.T, railCustomerRef, planID string) (*fakeNMIUpgradeGateway, *nmi.NMIClient) {
	t.Helper()
	f := &fakeNMIUpgradeGateway{
		railCustomerRef: railCustomerRef, planID: planID,
		subID: "rsub-upg-" + uuid.NewString()[:8],
	}
	f.createMode.Store("approve")
	f.saleMode.Store("approve")
	f.saleTxn = "upg-sale-" + uuid.NewString()
	f.lastOrder.Store("")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/subscriptions"):
			if f.subExists.Load() {
				fmt.Fprintf(w, `{"subscriptions":[{"object":"subscription","id":"%s","customer_vault_id":"%s","delayed_condition":"active","plan":{"id":"%s"}}],"next_cursor":null,"has_more":false}`,
					f.subID, f.railCustomerRef, f.planID)
				return
			}
			fmt.Fprint(w, `{"subscriptions":[],"next_cursor":null,"has_more":false}`)
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/subscriptions/"):
			f.subDeletes.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			_ = r.ParseForm()
			if r.Form.Get("recurring") == "add_subscription" {
				f.createCalls.Add(1)
				f.lastOrder.Store(r.Form.Get("orderid"))
				switch f.createMode.Load().(string) {
				case "ambiguousLanded":
					// The create LANDED but the response was lost.
					f.subExists.Store(true)
					w.WriteHeader(http.StatusBadGateway)
				case "ambiguousLost":
					// The response was lost and the create did NOT land.
					w.WriteHeader(http.StatusBadGateway)
				default:
					f.subExists.Store(true)
					fmt.Fprintf(w, "response=1&responsetext=SUCCESS&subscription_id=%s&authcode=OK", f.subID)
				}
				return
			}
			if r.Form.Get("type") == "sale" {
				f.saleCalls.Add(1)
				switch f.saleMode.Load().(string) {
				case "decline":
					fmt.Fprint(w, "response=2&responsetext=DECLINED&response_code=202")
				case "ambiguousHidden":
					w.WriteHeader(http.StatusBadGateway)
				default:
					f.saleVisible.Store(true)
					fmt.Fprintf(w, "response=1&responsetext=SUCCESS&transactionid=%s&authcode=OK", f.saleTxn)
				}
				return
			}
			if f.saleVisible.Load() {
				fmt.Fprintf(w, `<nm_response><transaction><transaction_id>%s</transaction_id><order_id>%s</order_id><action><action_type>sale</action_type><success>1</success></action></transaction></nm_response>`, f.saleTxn, r.Form.Get("order_id"))
				return
			}
			fmt.Fprint(w, `<nm_response></nm_response>`)
		}
	}))
	t.Cleanup(srv.Close)

	client, err := nmi.NewClient("nmi", &config.NMIProviderSettings{
		SecurityKey: "test_security_key", WebhookSecret: "test_secret",
	}, true)
	require.NoError(t, err)
	client.V5BaseURL = srv.URL
	client.QueryURL = srv.URL
	client.DirectPostURL = srv.URL
	return f, client
}

type upgradeAdoptFixture struct {
	db          *db.DB
	svc         *CheckoutService
	gateway     *fakeNMIUpgradeGateway
	req         *CheckoutRequest
	user        *UserIdentity
	newPrice    *models.Price
	newProduct  *models.Product
	existingSub *models.Subscription
	target      railTarget
	ctx         context.Context
}

func newUpgradeAdoptFixture(t *testing.T) *upgradeAdoptFixture {
	t.Helper()
	dbi := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	pool := dbi.Pool()
	dbtest.EnsureTestMerchant(context.Background(), t, pool)
	ctx := checkoutFixtureCtx(t, pool, "nmi")

	now := time.Now().UTC().Truncate(time.Second)
	userID := uuid.New().String()
	customerID := dbtest.EnsureCustomerIDPgx(ctx, t, pool, userID)
	sfx := uuid.NewString()[:8]

	// Old tier: expensive; new tier: cheap ⇒ Model-B first charge clamps to 0,
	// keeping the test focused on the successor-create leg.
	oldProductID, oldPriceID := uuid.New(), uuid.New()
	newProductID, newPriceID := uuid.New(), uuid.New()
	hours := 720
	insertProductAndPrice(ctx, t, pool, &models.Product{
		ID: oldProductID, Key: "upg-old-" + sfx, DisplayName: "Upgrade Old",
		Archived: false, CreatedAt: now, UpdatedAt: now,
	}, &models.Price{
		ID: oldPriceID, ProductID: oldProductID, Archived: false,
		Amount: 50_000_000, Currency: "USD", AutoRenew: true, AccessDurationHours: &hours,
		CreatedAt: now, UpdatedAt: now,
	})
	insertProductAndPrice(ctx, t, pool, &models.Product{
		ID: newProductID, Key: "upg-new-" + sfx, DisplayName: "Upgrade New",
		Archived: false, CreatedAt: now, UpdatedAt: now,
	}, &models.Price{
		ID: newPriceID, ProductID: newProductID, Archived: false,
		Amount: 5_000_000, Currency: "USD", AutoRenew: true, AccessDurationHours: &hours,
		CreatedAt: now, UpdatedAt: now,
	})

	railCustomerRef := "vault-upg-" + sfx
	planID := "plan-upg-" + sfx
	gateway, client := newFakeNMIUpgradeGateway(t, railCustomerRef, planID)

	clock := clockwork.NewFakeClockAt(now)

	pspID := dbtest.EnsureTestPSP(ctx, t, pool, dbtest.TestMerchantID.UUID(), "nmi")

	// Stored payment method the upgrade charges against.
	pm := &models.PaymentMethod{
		ID:                   uuid.New(),
		CustomerID:           customerID,
		Rail:                 models.Rail("nmi"),
		PspID:                pspID,
		RailCustomerRef:      railCustomerRef,
		RailMethodRef:        "bill-upg-" + sfx,
		RebillDriver:         models.RebillDriverProvider,
		InitialTransactionID: "txn-" + sfx,
		CreatedAt:            now, UpdatedAt: now,
	}
	require.NoError(t, paymentmethods.NewPaymentMethodRepo(dbi).Create(ctx, pm))

	// Existing active subscription on the old tier.
	oldSubID := uuid.New()
	periodStart := now.Add(-24 * time.Hour)
	periodEnd := now.Add(29 * 24 * time.Hour)
	_, err := pool.Exec(ctx, `INSERT INTO openrails.subscriptions
	        (id, price_id, product_id, status, rail, psp_id, rail_subscription_id,
	         current_period_starts_at, current_period_ends_at, started_at,
	         payment_method_id, customer_id, merchant_id)
	      VALUES ($1, $2, $3, 'active', 'nmi', $4, $5, $6, $7, $6, $8, $9, $10)`,
		oldSubID, oldPriceID, oldProductID, pspID, "rsub-old-"+sfx,
		periodStart, periodEnd, pm.ID, customerID, dbtest.TestMerchantID.UUID())
	require.NoError(t, err)

	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.rail_intents WHERE payload->>'user_id' = $1", userID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.payments WHERE customer_id = $1", customerID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.entitlements WHERE customer_id = $1", customerID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.subscriptions WHERE customer_id = $1", customerID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.payment_methods WHERE id = $1", pm.ID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.prices WHERE id = ANY($1)", []uuid.UUID{oldPriceID, newPriceID})
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.products WHERE id = ANY($1)", []uuid.UUID{oldProductID, newProductID})
	})

	priceSvc := catalog.NewPriceService(dbi)
	productSvc := catalog.NewProductService(dbi)
	paymentSvc := payments.NewPaymentService(dbi, clock)
	entSvc := entitlements.NewEntitlementService(dbi, clock)
	pmSvc := paymentmethods.NewPaymentMethodService(dbi)
	subSvc := subscriptions.NewSubscriptionService(dbi, priceSvc, productSvc, nil, clock)
	svc := NewCheckoutService(subSvc, productSvc, priceSvc, paymentSvc, entSvc,
		pmSvc, nil, nil, nil, nil, nil, clock)
	// #788: the scoped resolver is the ONLY NMI client source; the fixture
	// overrides it with the fake-gateway client.
	svc.ResolveNMIClientOverride = func(context.Context, string) (*nmi.NMIClient, error) { return client, nil }
	svc.SetSubscriptionLifecycleService(subscriptions.NewSubscriptionLifecycleService(dbi, productSvc, priceSvc, entSvc, subscriptions.NewNotificationService(dbi, nil), paymentSvc, clock))
	svc.Intents = &intents.Runner{Store: intents.NewStore(dbi), Registry: intents.NewRegistry(NewNMIUpgradeIntentHandler(svc)), Config: fullModeConfig(), Clock: clock}

	existingSub, err := subscriptions.NewSubscriptionRepo(dbi).GetByID(ctx, oldSubID)
	require.NoError(t, err)
	oldPrice := &models.Price{
		ID: oldPriceID, ProductID: oldProductID, Amount: 50_000_000, Currency: "USD",
		AutoRenew: true, AccessDurationHours: &hours,
	}
	existingSub.Price = oldPrice

	// The NEW price carries the NMI plan link in memory (requireNMIPlanForRail).
	newPrice := &models.Price{
		ID: newPriceID, ProductID: newProductID, Amount: 5_000_000, Currency: "USD",
		AutoRenew: true, AccessDurationHours: &hours,
		PSPLinks: map[string]map[string]string{"nmi": {models.RailKeyRail: "nmi", models.RailKeyPlanID: planID, "psp_id": pspID.String()}},
	}
	newProduct := &models.Product{ID: newProductID, Key: "upg-new-" + sfx, DisplayName: "Upgrade New"}

	return &upgradeAdoptFixture{
		db: dbi, svc: svc, gateway: gateway,
		req: &CheckoutRequest{
			PaymentMethodID: api.FormatPaymentMethodID(pm.ID),
			IdempotencyKey:  "upg-key-" + sfx,
		},
		user:        &UserIdentity{ID: userID},
		newPrice:    newPrice,
		newProduct:  newProduct,
		existingSub: existingSub,
		target: railTarget{
			PSP:  "nmi",
			Rail: "nmi",
			Scope: &merchants.PSPScope{
				ID:   pspID,
				Key:  "nmi",
				Rail: "nmi",
			},
		},
		ctx: ctx,
	}
}

// A positive successor receipt completes the upgrade with one remote create.
func TestUpgradePositiveSuccessorReceipt_CompletesAtomically(t *testing.T) {
	fx := newUpgradeAdoptFixture(t)
	fx.gateway.createMode.Store("approve")

	resp, err := fx.svc.processUpgrade(fx.ctx, fx.req, fx.user, fx.newPrice, fx.newProduct, fx.existingSub, fx.target)
	require.NoError(t, err, "a positive successor receipt completes the upgrade")
	require.Equal(t, "success", resp.Status)
	require.EqualValues(t, 1, fx.gateway.createCalls.Load(), "never a second blind create")

	// The adopted remote subscription is registered locally and active.
	local, lerr := fx.svc.SubscriptionService.GetByPSPSubscriptionID(fx.ctx, "nmi", fx.gateway.subID)
	require.NoError(t, lerr)
	require.Equal(t, models.StatusActive, local.Status)
	require.Equal(t, fx.newPrice.ID, local.PriceID)

	// The old subscription is cancelled locally and marked for the durable
	// verify-then-delete intent. This fixture does not run the intent worker,
	// so no direct provider delete is allowed in the request path.
	old, oerr := subscriptions.NewSubscriptionRepo(fx.db).GetByID(fx.ctx, fx.existingSub.ID)
	require.NoError(t, oerr)
	require.Equal(t, models.StatusCancelled, old.Status)
	require.NotNil(t, old.DeletionScheduledAt)
	require.Zero(t, fx.gateway.subDeletes.Load(), "predecessor delete is owned by the durable intent")
}

// Ambiguous successor create with no immediate roster match: the request
// surfaces ErrCheckoutProcessing and a retry never re-sends the create. An
// empty provider search is not proof that the first write was unsent.
func TestUpgradeAmbiguousCreateUnresolved_NeverRecreates(t *testing.T) {
	fx := newUpgradeAdoptFixture(t)
	fx.gateway.createMode.Store("ambiguousLost")

	_, err := fx.svc.processUpgrade(fx.ctx, fx.req, fx.user, fx.newPrice, fx.newProduct, fx.existingSub, fx.target)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrCheckoutProcessing), "unresolved ambiguity is processing, never a decline: %v", err)
	require.EqualValues(t, 1, fx.gateway.createCalls.Load())
	firstOrder := fx.gateway.lastOrder.Load().(string)
	require.NotEmpty(t, firstOrder)

	// Old subscription untouched by the failed attempt.
	old, oerr := subscriptions.NewSubscriptionRepo(fx.db).GetByID(fx.ctx, fx.existingSub.ID)
	require.NoError(t, oerr)
	require.Equal(t, models.StatusActive, old.Status)

	// Provider search remains empty. The retry (same idempotency key) must stay
	// in processing until an operator/provider reconciliation supplies a
	// positive receipt; it must not issue a second remote create.
	fx.gateway.createMode.Store("approve")
	_, err = fx.svc.processUpgrade(fx.ctx, fx.req, fx.user, fx.newPrice, fx.newProduct, fx.existingSub, fx.target)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrCheckoutProcessing)
	require.EqualValues(t, 1, fx.gateway.createCalls.Load(), "must not resend an unresolved provider write")
	require.Equal(t, firstOrder, fx.gateway.lastOrder.Load().(string), "the immutable order identity is retained")
}
