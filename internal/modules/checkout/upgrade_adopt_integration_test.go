//go:build integration

package checkout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/pkg/api"
	"github.com/open-rails/openrails/pkg/merchant"
)

// statefulIdemStub is a checkoutIdempotencyStore that remembers records across
// calls so a Fail followed by a retried Begin exercises the
// retryAfterFailure path of processUpgrade.
type statefulIdemStub struct {
	mu   sync.Mutex
	recs map[string]*IdempotencyRecord
}

func newStatefulIdemStub() *statefulIdemStub {
	return &statefulIdemStub{recs: map[string]*IdempotencyRecord{}}
}

func (s *statefulIdemStub) Begin(_ context.Context, op, key string) (*IdempotencyRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec, ok := s.recs[op+":"+key]; ok {
		return rec, true, nil
	}
	rec := &IdempotencyRecord{Status: IdempotencyStatusPending, CreatedAt: time.Now()}
	s.recs[op+":"+key] = rec
	return rec, false, nil
}

func (s *statefulIdemStub) Fail(_ context.Context, op, key string, opErr error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec, ok := s.recs[op+":"+key]; ok {
		rec.Status = IdempotencyStatusFailed
		if opErr != nil {
			rec.Error = opErr.Error()
		}
	}
	return nil
}

func (s *statefulIdemStub) Complete(_ context.Context, op, key string, result json.RawMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec, ok := s.recs[op+":"+key]; ok {
		rec.Status = IdempotencyStatusSuccess
		rec.Result = result
	}
	return nil
}

// fakeNMIUpgradeGateway scripts the surfaces processUpgrade touches: classic
// add_subscription (successor create), the v5 subscription roster (adopt
// scan), v5 DELETE /subscriptions/{id} (old-sub cancel + rollback), and the
// classic query search (unused here — proration is zero).
type fakeNMIUpgradeGateway struct {
	railCustomerRef string
	planID  string
	subID   string

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
			// classic query.php: no sale ever lands in these tests.
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
	ctx         context.Context
}

func newUpgradeAdoptFixture(t *testing.T) *upgradeAdoptFixture {
	t.Helper()
	dbi := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	pool := dbi.Pool()
	dbtest.EnsureTestMerchant(context.Background(), t, pool)
	ctx := merchant.WithID(context.Background(), dbtest.TestMerchantID)

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

	clock := clockwork.NewRealClock()

	// Stored payment method the upgrade charges against.
	pm := &models.PaymentMethod{
		ID:                   uuid.New(),
		CustomerID:           customerID,
		Rail:                 models.Rail("nmi"),
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
	        (id, price_id, product_id, status, rail, rail_subscription_id,
	         current_period_starts_at, current_period_ends_at, started_at,
	         payment_method_id, customer_id, merchant_id)
	      VALUES ($1, $2, $3, 'active', 'nmi', $4, $5, $6, $5, $7, $8, $9)`,
		oldSubID, oldPriceID, oldProductID, "rsub-old-"+sfx,
		periodStart, periodEnd, pm.ID, customerID, dbtest.TestMerchantID.UUID())
	require.NoError(t, err)

	t.Cleanup(func() {
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
		pmSvc, nil, newStatefulIdemStub(), nil, nil, nil, clock)
	// #788: the scoped resolver is the ONLY NMI client source; the fixture
	// overrides it with the fake-gateway client.
	svc.ResolveNMIClientOverride = func(context.Context, string) (*nmi.NMIClient, error) { return client, nil }

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
		PSPLinks: map[string]map[string]string{"nmi": {models.RailKeyRail: "nmi", models.RailKeyPlanID: planID}},
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
		ctx:         ctx,
	}
}

// Ambiguous successor create whose create actually LANDED: the roster scan
// adopts the orphan inline — the upgrade completes with ONE remote create and
// no live remote subscription is ever abandoned as failed (#674 tail).
func TestUpgradeAmbiguousCreateLanded_AdoptsInline(t *testing.T) {
	fx := newUpgradeAdoptFixture(t)
	fx.gateway.createMode.Store("ambiguousLanded")

	resp, err := fx.svc.processUpgrade(fx.ctx, fx.req, fx.user, fx.newPrice, fx.newProduct, fx.existingSub, railTarget{PSP: "nmi", Rail: "nmi"})
	require.NoError(t, err, "landed-but-lost create must be adopted, not failed")
	require.Equal(t, "success", resp.Status)
	require.EqualValues(t, 1, fx.gateway.createCalls.Load(), "never a second blind create")

	// The adopted remote subscription is registered locally and active.
	local, lerr := fx.svc.SubscriptionService.GetByRailSubscriptionID(fx.ctx, "nmi", fx.gateway.subID)
	require.NoError(t, lerr)
	require.Equal(t, models.StatusActive, local.Status)
	require.Equal(t, fx.newPrice.ID, local.PriceID)

	// The old subscription is cancelled locally and deleted remotely (step 5).
	old, oerr := subscriptions.NewSubscriptionRepo(fx.db).GetByID(fx.ctx, fx.existingSub.ID)
	require.NoError(t, oerr)
	require.Equal(t, models.StatusCancelled, old.Status)
	require.EqualValues(t, 1, fx.gateway.subDeletes.Load(), "old NMI subscription cancelled")
}

// Ambiguous successor create that did NOT land: the request surfaces
// ErrCheckoutProcessing (never a decline); the retry under the SAME
// content-derived key verifies the roster (empty ⇒ safe), re-creates under the
// SAME order id, and completes. Exactly one live remote subscription.
func TestUpgradeAmbiguousCreateLost_RetryRecreatesSameOrderID(t *testing.T) {
	fx := newUpgradeAdoptFixture(t)
	fx.gateway.createMode.Store("ambiguousLost")

	_, err := fx.svc.processUpgrade(fx.ctx, fx.req, fx.user, fx.newPrice, fx.newProduct, fx.existingSub, railTarget{PSP: "nmi", Rail: "nmi"})
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrCheckoutProcessing), "unresolved ambiguity is processing, never a decline: %v", err)
	require.EqualValues(t, 1, fx.gateway.createCalls.Load())
	firstOrder := fx.gateway.lastOrder.Load().(string)
	require.NotEmpty(t, firstOrder)

	// Old subscription untouched by the failed attempt.
	old, oerr := subscriptions.NewSubscriptionRepo(fx.db).GetByID(fx.ctx, fx.existingSub.ID)
	require.NoError(t, oerr)
	require.Equal(t, models.StatusActive, old.Status)

	// Provider recovers; the retry (same idempotency key ⇒ retryAfterFailure)
	// scans the roster first, then re-creates under the ORIGINAL order id.
	fx.gateway.createMode.Store("approve")
	resp, err := fx.svc.processUpgrade(fx.ctx, fx.req, fx.user, fx.newPrice, fx.newProduct, fx.existingSub, railTarget{PSP: "nmi", Rail: "nmi"})
	require.NoError(t, err)
	require.Equal(t, "success", resp.Status)
	require.EqualValues(t, 2, fx.gateway.createCalls.Load())
	require.Equal(t, firstOrder, fx.gateway.lastOrder.Load().(string), "retry reuses the content-derived order id")

	local, lerr := fx.svc.SubscriptionService.GetByRailSubscriptionID(fx.ctx, "nmi", fx.gateway.subID)
	require.NoError(t, lerr)
	require.Equal(t, models.StatusActive, local.Status)
}
