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
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/pkg/merchant"
)

// fakeNMISaleGateway scripts the sale lanes (classic Direct Post — the #297
// stored-credential lane every checkout sale rides now — plus the legacy v5
// endpoint) and the classic query API the verify leg reads: saleMode controls
// the sale outcome, charged whether the query reports a landed sale for the
// searched order id. saleForm records the last classic sale's full form for
// stored-credential wire assertions.
type fakeNMISaleGateway struct {
	saleCalls  atomic.Int64
	queryCalls atomic.Int64
	saleMode   atomic.Value // "approve" | "decline" | "ambiguous500"
	charged    atomic.Bool
	lastOrder  atomic.Value // string: order id of the last sale attempt
	saleForm   atomic.Value // url.Values: last classic sale form
	txnID      string
}

func newFakeNMISaleGateway(t *testing.T) (*fakeNMISaleGateway, *nmi.NMIClient) {
	t.Helper()
	f := &fakeNMISaleGateway{txnID: "txn-sale-" + uuid.NewString()[:8]}
	f.saleMode.Store("approve")
	f.lastOrder.Store("")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/payments/sale") {
			f.saleCalls.Add(1)
			var body struct {
				OrderDetails struct {
					ID string `json:"id"`
				} `json:"order_details"`
			}
			_ = jsonDecode(r, &body)
			f.lastOrder.Store(body.OrderDetails.ID)
			switch f.saleMode.Load().(string) {
			case "decline":
				fmt.Fprintf(w, `{"id":"%s","response":"2","response_code":"200","response_text":"DECLINED"}`, f.txnID)
			case "ambiguous500":
				// The charge LANDED but the response was lost (5xx).
				f.charged.Store(true)
				w.WriteHeader(http.StatusBadGateway)
			default:
				f.charged.Store(true)
				fmt.Fprintf(w, `{"id":"%s","response":"1","response_code":"100","auth_code":"OK"}`, f.txnID)
			}
			return
		}
		_ = r.ParseForm()
		if r.Form.Get("type") == "sale" {
			// Classic Direct Post sale — the #297 stored-credential lane.
			f.saleCalls.Add(1)
			f.saleForm.Store(r.Form)
			f.lastOrder.Store(r.Form.Get("orderid"))
			switch f.saleMode.Load().(string) {
			case "decline":
				fmt.Fprint(w, "response=2&responsetext=DECLINED&response_code=200")
			case "ambiguous500":
				// The charge LANDED but the response was lost (5xx).
				f.charged.Store(true)
				w.WriteHeader(http.StatusBadGateway)
			default:
				f.charged.Store(true)
				fmt.Fprintf(w, "response=1&responsetext=SUCCESS&authcode=OK&transactionid=%s&response_code=100", f.txnID)
			}
			return
		}
		// classic query.php transaction search
		f.queryCalls.Add(1)
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

func jsonDecode(r *http.Request, out any) error {
	return json.NewDecoder(r.Body).Decode(out)
}

type saleIntentFixture struct {
	db      *db.DB
	runner  *intents.Runner
	gateway *fakeNMISaleGateway
	payload NMISalePayload
	userID  string
	priceID uuid.UUID
	ctx     context.Context
}

func newSaleIntentFixture(t *testing.T) *saleIntentFixture {
	t.Helper()
	dsn := dbtest.SharedPostgresDSN(t)
	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbi.Pool()
	dbtest.EnsureTestMerchant(context.Background(), t, pool)
	ctx := merchant.WithID(context.Background(), dbtest.TestMerchantID)

	now := time.Now().UTC().Truncate(time.Second)
	userID := uuid.New().String()
	customerID := dbtest.EnsureCustomerIDPgx(ctx, t, pool, userID)
	productID, priceID := uuid.New(), uuid.New()
	insertProductAndPrice(ctx, t, pool, &models.Product{
		ID: productID, Key: "sale-intent-" + uuid.NewString()[:8], DisplayName: "Sale Intent Test",
		Archived: false, CreatedAt: now, UpdatedAt: now,
	}, &models.Price{
		ID: priceID, ProductID: productID, Archived: false,
		Amount: 5_000_000, Currency: "USD", CreatedAt: now, UpdatedAt: now,
	})
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.rail_intents WHERE intent_type = 'nmi_sale' AND price_id = $1", priceID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.entitlements WHERE customer_id = $1", customerID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.payments WHERE customer_id = $1", customerID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.prices WHERE id = $1", priceID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.products WHERE id = $1", productID)
	})

	gateway, client := newFakeNMISaleGateway(t)
	clock := clockwork.NewRealClock()
	saleService := &CheckoutNMISaleService{
		PurchaseService: NewCheckoutPurchaseService(
			catalog.NewPriceService(dbi),
			catalog.NewProductService(dbi),
			payments.NewPaymentService(dbi, clock),
			entitlements.NewEntitlementService(dbi, clock),
			nil,
			clock,
		),
		NMIClients: map[string]*nmi.NMIClient{"mobius": client},
		// VaultService carries the DB handle finalize persists the #297
		// stored-credential anchor through.
		VaultService: &paymentmethods.VaultService{DB: dbi},
	}
	runner := &intents.Runner{
		Store:    intents.NewStore(dbi),
		Registry: intents.NewRegistry(NewNMISaleIntentHandler(saleService)),
	}
	return &saleIntentFixture{
		db: dbi, runner: runner, gateway: gateway,
		payload: NMISalePayload{
			Provider:        "mobius",
			CustomerVaultID: "vault-" + uuid.NewString()[:8],
			AmountMicros:    5_000_000,
			Currency:        "USD",
			Description:     "Purchase: Sale Intent Test",
			UserID:          userID,
			PriceID:         priceID,
		},
		userID: userID, priceID: priceID, ctx: ctx,
	}
}

func (fx *saleIntentFixture) enqueueAndExecute(t *testing.T, key string) gen.OpenrailsRailIntent {
	t.Helper()
	intent, err := fx.runner.EnqueueAndExecute(fx.ctx, intents.EnqueueParams{
		MerchantID:     dbtest.TestMerchantID.UUID(),
		Provider:       "mobius",
		IntentType:     TypeNMISale,
		PriceID:        &fx.priceID,
		Payload:        fx.payload,
		IdempotencyKey: NMISaleIdempotencyKey(key),
		NextAttemptAt:  time.Now().UTC(),
		Origin:         intents.OriginUser,
		OriginReason:   "test checkout sale",
	})
	require.NoError(t, err)
	return intent
}

func (fx *saleIntentFixture) paymentCount(t *testing.T) int {
	t.Helper()
	var n int
	require.NoError(t, fx.db.Pool().QueryRow(fx.ctx,
		"SELECT count(*) FROM openrails.payments WHERE rail = 'mobius' AND transaction_id = $1", fx.gateway.txnID).Scan(&n))
	return n
}

func (fx *saleIntentFixture) advanceClock(d time.Duration) {
	fx.runner.Clock = clockwork.NewFakeClockAt(time.Now().UTC().Add(d))
}

// Happy path: one INSERT + inline execute + registered purchase; a replayed
// request (same checkout idempotency key) answers from the durable row without
// touching the provider.
func TestNMISaleIntent_WriteThroughHappyPathAndReplay(t *testing.T) {
	fx := newSaleIntentFixture(t)
	key := "sale-key-" + uuid.NewString()[:8]

	intent := fx.enqueueAndExecute(t, key)
	require.Equal(t, intents.StatusSucceeded, intent.Status)
	require.EqualValues(t, 1, fx.gateway.saleCalls.Load())
	require.Equal(t, 1, fx.paymentCount(t))
	// #674: the NMI order id is derived from the intent id.
	require.Equal(t, intent.ID.String(), fx.gateway.lastOrder.Load().(string))
	// Evidence carries the producer-facing pointers (kept by PrunePolicy).
	cached, err := saleResultFromIntent(intent)
	require.NoError(t, err)
	require.Equal(t, fx.gateway.txnID, cached.TransactionID)
	require.NotEqual(t, uuid.Nil, cached.PaymentID)

	replay := fx.enqueueAndExecute(t, key)
	require.Equal(t, intents.StatusSucceeded, replay.Status)
	require.Equal(t, intent.ID, replay.ID)
	require.EqualValues(t, 1, fx.gateway.saleCalls.Load(), "replay never re-charges")
	require.Equal(t, 1, fx.paymentCount(t))
}

// A parsed decline is terminal — clean, no verification, no payment row.
func TestNMISaleIntent_DeclineIsTerminal(t *testing.T) {
	fx := newSaleIntentFixture(t)
	fx.gateway.saleMode.Store("decline")

	intent := fx.enqueueAndExecute(t, "sale-key-"+uuid.NewString()[:8])
	require.Equal(t, intents.StatusFailedTerminal, intent.Status)
	require.Equal(t, 0, fx.paymentCount(t))
	require.NotNil(t, intent.ResultEvidence)
	require.Contains(t, string(intent.ResultEvidence), `"declined": true`)
}

// Ambiguous timeout (5xx after send — the charge LANDED): the intent parks as
// unknown_needs_verify (never treated as a decline), retries of the request
// never blind-charge, and the verifier lands the sale via the order-id read —
// exactly one external charge, exactly one local payment.
func TestNMISaleIntent_AmbiguousTimeoutVerifiedCharged(t *testing.T) {
	fx := newSaleIntentFixture(t)
	fx.gateway.saleMode.Store("ambiguous500")
	key := "sale-key-" + uuid.NewString()[:8]

	intent := fx.enqueueAndExecute(t, key)
	require.Equal(t, intents.StatusUnknownNeedsVerify, intent.Status)
	require.EqualValues(t, 1, fx.gateway.saleCalls.Load())
	require.Equal(t, 0, fx.paymentCount(t))

	// A client retry maps onto the same in-verify intent: no new charge.
	replay := fx.enqueueAndExecute(t, key)
	require.Equal(t, intent.ID, replay.ID)
	require.Equal(t, intents.StatusUnknownNeedsVerify, replay.Status)
	require.EqualValues(t, 1, fx.gateway.saleCalls.Load())

	// Verifier resolves via the query API and registers the purchase.
	fx.advanceClock(2 * time.Minute)
	_, err := fx.runner.RunVerifyOnce(fx.ctx)
	require.NoError(t, err)
	final, err := intents.NewStore(fx.db).Get(fx.ctx, intent.ID)
	require.NoError(t, err)
	require.Equal(t, intents.StatusSucceeded, final.Status)
	require.EqualValues(t, 1, fx.gateway.saleCalls.Load(), "verification never re-charges")
	require.Equal(t, 1, fx.paymentCount(t))
}

// Die AFTER the provider charge but BEFORE the local registration lands
// (simulated: the price row is missing when finalize runs, so RegisterPurchase
// fails after a SUCCESSFUL charge). The intent goes to verification instead of
// "declined"; once the world is repaired the verifier registers the purchase
// off the original charge. Never charged-but-unrecorded.
func TestNMISaleIntent_ChargedButRegistrationFails_VerifierRepairs(t *testing.T) {
	fx := newSaleIntentFixture(t)
	pool := fx.db.Pool()

	// Sabotage finalize: point the payload at a price id that does not exist
	// yet (the "local write failed after charge" window).
	realPriceID := fx.payload.PriceID
	missingPriceID := uuid.New()
	fx.payload.PriceID = missingPriceID
	intent := fx.enqueueAndExecute(t, "sale-key-"+uuid.NewString()[:8])
	require.Equal(t, intents.StatusUnknownNeedsVerify, intent.Status, "charged + failed local record must verify, never fail")
	require.EqualValues(t, 1, fx.gateway.saleCalls.Load())

	// Repair the world: create the missing price (same product family).
	var productID uuid.UUID
	require.NoError(t, pool.QueryRow(fx.ctx, "SELECT product_id FROM openrails.prices WHERE id = $1", realPriceID).Scan(&productID))
	// A different amount dodges the (product, amount, window) uniqueness.
	_, err := pool.Exec(fx.ctx, `INSERT INTO openrails.prices (id, product_id, amount, currency, merchant_id)
		VALUES ($1, $2, 6000000, 'USD', $3)`, missingPriceID, productID, dbtest.TestMerchantID.UUID())
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(fx.ctx, "DELETE FROM openrails.prices WHERE id = $1", missingPriceID)
	})

	fx.advanceClock(2 * time.Minute)
	_, err = fx.runner.RunVerifyOnce(fx.ctx)
	require.NoError(t, err)
	final, err := intents.NewStore(fx.db).Get(fx.ctx, intent.ID)
	require.NoError(t, err)
	require.Equal(t, intents.StatusSucceeded, final.Status)
	require.EqualValues(t, 1, fx.gateway.saleCalls.Load(), "repair never re-charges")
	require.Equal(t, 1, fx.paymentCount(t))
}

// Provider outage: the attempt is ambiguous (nothing parseable came back),
// verification says "not executed" → the executor re-sends with the ORIGINAL
// intent-derived order id once the provider is healthy.
func TestNMISaleIntent_ProviderOutageRetriesWithOriginalOrderID(t *testing.T) {
	fx := newSaleIntentFixture(t)
	// A 5xx where the charge did NOT land: the query API keeps answering
	// "no sale" (charged is reset right after the attempt below).
	fx.gateway.saleMode.Store("ambiguous500")
	key := "sale-key-" + uuid.NewString()[:8]

	intent := fx.enqueueAndExecute(t, key)
	require.Equal(t, intents.StatusUnknownNeedsVerify, intent.Status)
	firstOrder := fx.gateway.lastOrder.Load().(string)
	// The outage 5xx did not actually charge in this scenario:
	fx.gateway.charged.Store(false)

	// Verify: clean read, no sale found → verified not executed → retryable.
	fx.advanceClock(2 * time.Minute)
	_, err := fx.runner.RunVerifyOnce(fx.ctx)
	require.NoError(t, err)
	mid, err := intents.NewStore(fx.db).Get(fx.ctx, intent.ID)
	require.NoError(t, err)
	require.Equal(t, intents.StatusFailedRetryable, mid.Status)
	require.Equal(t, 0, fx.paymentCount(t))

	// Provider recovers; the scheduled executor re-sends under the SAME order id.
	fx.gateway.saleMode.Store("approve")
	fx.advanceClock(10 * time.Minute)
	_, err = fx.runner.RunExecuteOnce(fx.ctx)
	require.NoError(t, err)
	final, err := intents.NewStore(fx.db).Get(fx.ctx, intent.ID)
	require.NoError(t, err)
	require.Equal(t, intents.StatusSucceeded, final.Status)
	require.EqualValues(t, 2, fx.gateway.saleCalls.Load())
	require.Equal(t, firstOrder, fx.gateway.lastOrder.Load().(string), "retry reuses the ORIGINAL intent-derived order id")
	require.Equal(t, 1, fx.paymentCount(t))
}
