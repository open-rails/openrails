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
	"github.com/open-rails/openrails/internal/modules/productaccess"
	"github.com/open-rails/openrails/pkg/merchant"
)

// fakeNMISaleGateway scripts the sale lanes (classic Direct Post — the #297
// stored-credential lane every checkout sale rides now — plus the legacy v5
// endpoint) and the classic query API the verify leg reads: saleMode controls
// the sale outcome, charged whether the query reports a landed sale for the
// searched order id. saleForm records the last classic sale's full form for
// stored-credential wire assertions.
type fakeNMISaleGateway struct {
	saleCalls   atomic.Int64
	queryCalls  atomic.Int64
	saleMode    atomic.Value // "approve" | "decline" | "ambiguous500"
	charged     atomic.Bool
	hidden      atomic.Bool
	unavailable atomic.Bool
	lastOrder   atomic.Value // string: order id of the last sale attempt
	saleForm    atomic.Value // url.Values: last classic sale form
	vault       atomic.Value // string: customer vault reported by the exact transaction read
	txnID       string
}

func newFakeNMISaleGateway(t *testing.T) (*fakeNMISaleGateway, *nmi.NMIClient) {
	t.Helper()
	f := &fakeNMISaleGateway{txnID: "txn-sale-" + uuid.NewString()[:8]}
	f.saleMode.Store("approve")
	f.lastOrder.Store("")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/payments/") {
			vault, _ := f.vault.Load().(string)
			if !strings.HasSuffix(r.URL.Path, "/payments/"+f.txnID) || !f.charged.Load() {
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprint(w, `{"type":"notFound","error_code":"E_NOT_FOUND","message":"not found"}`)
				return
			}
			fmt.Fprintf(w, `{"object":"transaction","id":"%s","response":"1","amount":"5.00","currency":"USD","customer_vault_id":"%s","actions":[{"id":"%s","type":"sale","success":true,"amount":"5.00"}]}`, f.txnID, vault, f.txnID)
			return
		}
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
			case "hold-response":
				f.charged.Store(true)
				<-r.Context().Done()
			case "timeout-after-accept":
				f.charged.Store(true)
				w.WriteHeader(http.StatusGatewayTimeout)
			case "ambiguous500":
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
			case "processor-uncertain":
				f.charged.Store(true)
				fmt.Fprint(w, "response=3&responsetext=Communication+error&response_code=420")
			case "hold-response":
				f.charged.Store(true)
				<-r.Context().Done()
			case "timeout-after-accept":
				f.charged.Store(true)
				w.WriteHeader(http.StatusGatewayTimeout)
			case "ambiguous500":
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
		if f.unavailable.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if f.charged.Load() && !f.hidden.Load() {
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
	db         *db.DB
	runner     *intents.Runner
	gateway    *fakeNMISaleGateway
	purchase   *CheckoutPurchaseService
	payload    NMISalePayload
	userID     string
	customerID uuid.UUID
	productID  uuid.UUID
	priceID    uuid.UUID
	ctx        context.Context
}

type failOnceProductAccess struct {
	delegate productAccessGranter
	err      error
}

func (f *failOnceProductAccess) GrantProductAccess(ctx context.Context, params productaccess.GrantParams) (*models.ProductAccessGrant, bool, error) {
	if f.err != nil {
		err := f.err
		f.err = nil
		return nil, false, err
	}
	return f.delegate.GrantProductAccess(ctx, params)
}

func newSaleIntentFixture(t *testing.T) *saleIntentFixture {
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
		ID: productID, Key: "sale-intent-" + uuid.NewString()[:8], DisplayName: "Sale Intent Test",
		Archived: false, CreatedAt: now, UpdatedAt: now,
	}, &models.Price{
		ID: priceID, ProductID: productID, Archived: false,
		Amount: 5_000_000, Currency: "USD", CreatedAt: now, UpdatedAt: now,
	})
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM billing.rail_intents WHERE intent_type = 'nmi_sale' AND price_id = $1", priceID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.entitlements WHERE customer_id = $1", customerID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.ledger_transfers WHERE customer_id = $1", customerID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.grants WHERE customer_id = $1", customerID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.payments WHERE customer_id = $1", customerID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.prices WHERE id = $1", priceID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.products WHERE id = $1", productID)
	})

	gateway, client := newFakeNMISaleGateway(t)
	clock := clockwork.NewRealClock()
	purchaseService := NewCheckoutPurchaseService(
		catalog.NewPriceService(dbi),
		catalog.NewProductService(dbi),
		payments.NewPaymentService(dbi, clock),
		entitlements.NewEntitlementService(dbi, clock),
		nil,
		clock,
	)
	saleService := &CheckoutNMISaleService{
		PurchaseService: purchaseService,
		// #788: the scoped resolver is the ONLY NMI client source.
		ResolveNMIClient: func(context.Context, string) (*nmi.NMIClient, error) { return client, nil },
		// RailPaymentMethodService carries the DB handle finalize persists the #297
		// stored-credential anchor through.
		RailPaymentMethodService: &paymentmethods.RailPaymentMethodService{DB: dbi},
	}
	runner := &intents.Runner{
		Store:    intents.NewStore(dbi),
		Registry: intents.NewRegistry(NewNMISaleIntentHandler(saleService)),
		// or#865: an unstated mode parks every intent — say "full" (see main_test.go).
		Config: fullModeConfig(),
	}
	return &saleIntentFixture{
		db: dbi, runner: runner, gateway: gateway, purchase: purchaseService,
		payload: NMISalePayload{
			Provider:        string(models.RailNMI),
			PSP:             "mobius",
			CustomerVaultID: "vault-" + uuid.NewString()[:8],
			AmountMicros:    5_000_000,
			Currency:        "USD",
			Description:     "Purchase: Sale Intent Test",
			UserID:          userID,
			PriceID:         priceID,
		},
		userID: userID, customerID: customerID, productID: productID, priceID: priceID, ctx: ctx,
	}
}

func (fx *saleIntentFixture) enqueueAndExecute(t *testing.T, key string) gen.OpenrailsRailIntent {
	t.Helper()
	pspID := dbtest.EnsureTestPSP(fx.ctx, t, fx.db.Pool(), dbtest.TestMerchantID.UUID(), "mobius")
	intent, err := fx.runner.EnqueueAndExecute(fx.ctx, intents.EnqueueParams{
		MerchantID:     dbtest.TestMerchantID.UUID(),
		Provider:       string(models.RailNMI),
		IntentType:     TypeNMISale,
		PriceID:        &fx.priceID,
		PspID:          pspID,
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
		"SELECT count(*) FROM billing.payments WHERE rail = 'nmi' AND transaction_id = $1", fx.gateway.txnID).Scan(&n))
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

func TestNMISaleIntent_ProductAccessFailureRetriesWithoutRecharging(t *testing.T) {
	fx := newSaleIntentFixture(t)
	injected := errors.New("injected product access failure")
	fx.purchase.SetProductAccessService(&failOnceProductAccess{
		delegate: productaccess.NewService(fx.db),
		err:      injected,
	})
	key := "sale-access-retry-" + uuid.NewString()[:8]
	intent := fx.enqueueAndExecute(t, key)
	require.Equal(t, intents.StatusUnknownNeedsVerify, intent.Status, "an incomplete ownership effect must not complete the sale intent")
	require.EqualValues(t, 1, fx.gateway.saleCalls.Load())
	require.Equal(t, 1, fx.paymentCount(t))
	var accessGrants int
	require.NoError(t, fx.db.Pool().QueryRow(fx.ctx,
		`SELECT count(*) FROM billing.grants WHERE customer_id=$1 AND product_id=$2 AND kind='ownership' AND event='grant'`,
		fx.customerID, fx.productID).Scan(&accessGrants))
	require.Zero(t, accessGrants)

	fx.advanceClock(2 * time.Minute)
	_, err := fx.runner.RunVerifyOnce(fx.ctx)
	require.NoError(t, err)
	final, err := intents.NewStore(fx.db).Get(fx.ctx, intent.ID)
	require.NoError(t, err)
	require.Equal(t, intents.StatusSucceeded, final.Status)
	require.EqualValues(t, 1, fx.gateway.saleCalls.Load(), "effect retry must verify the existing charge, never charge again")
	require.Equal(t, 1, fx.paymentCount(t))
	require.NoError(t, fx.db.Pool().QueryRow(fx.ctx,
		`SELECT count(*) FROM billing.grants WHERE customer_id=$1 AND product_id=$2 AND kind='ownership' AND event='grant'`,
		fx.customerID, fx.productID).Scan(&accessGrants))
	require.Equal(t, 1, accessGrants, "the retried ownership grant must be fulfilled exactly once")
}

// A parsed decline is terminal — clean, no verification, no COMPLETED payment
// row; the attempt lands as a durable FAILED payments row (#796: verbatim
// code + token_type) so approval_rate's denominator sees it.
func TestNMISaleIntent_DeclineIsTerminal(t *testing.T) {
	fx := newSaleIntentFixture(t)
	fx.gateway.saleMode.Store("decline")

	intent := fx.enqueueAndExecute(t, "sale-key-"+uuid.NewString()[:8])
	require.Equal(t, intents.StatusFailedTerminal, intent.Status)
	require.Equal(t, 0, fx.paymentCount(t))
	require.NotNil(t, intent.ResultEvidence)
	require.Contains(t, string(intent.ResultEvidence), `"declined": true`)

	// #796: the decline is a charge attempt — durable failed row, verbatim code.
	var failureCode, failureReason, tokenType, attemptKind string
	require.NoError(t, fx.db.Pool().QueryRow(fx.ctx,
		`SELECT COALESCE(failure_code,''), COALESCE(failure_reason,''), COALESCE(token_type,''), COALESCE(attempt_kind,'')
		 FROM billing.payments WHERE status='failed' AND transaction_id=$1`,
		"nmi_sale_declined:"+intent.ID.String()).Scan(&failureCode, &failureReason, &tokenType, &attemptKind))
	require.Equal(t, "transaction_was_declined_by_processor", failureCode) // NMI 200, verbatim localization id
	require.Equal(t, payments.FailureCardDeclined, failureReason)
	require.Equal(t, "psp_token", tokenType)
	require.Equal(t, payments.AttemptInitial, attemptKind)
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
	require.NoError(t, pool.QueryRow(fx.ctx, "SELECT product_id FROM billing.prices WHERE id = $1", realPriceID).Scan(&productID))
	// A different amount dodges the (product, amount, window) uniqueness; an
	// explicit #774 key dodges the auto-default "<product-key>-onetime"
	// collision with the product's other active price.
	_, err := pool.Exec(fx.ctx, `INSERT INTO billing.prices (id, product_id, amount, currency, merchant_id, key)
		VALUES ($1, $2, 6000000, 'USD', $3, $4)`, missingPriceID, productID, dbtest.TestMerchantID.UUID(), "sale-repair-"+missingPriceID.String())
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(fx.ctx, "DELETE FROM billing.prices WHERE id = $1", missingPriceID)
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

// A timeout response after acceptance plus delayed Query visibility never
// becomes permission to charge again, including restart, expiry and re-enqueue.
func TestNMISaleIntent_DelayedReceiptNeverResubmits(t *testing.T) {
	fx := newSaleIntentFixture(t)
	fx.gateway.saleMode.Store("timeout-after-accept")
	fx.gateway.hidden.Store(true)
	key := "sale-delayed-" + uuid.NewString()[:8]
	intent := fx.enqueueAndExecute(t, key)
	require.Equal(t, intents.StatusUnknownNeedsVerify, intent.Status)
	require.True(t, fx.gateway.charged.Load())
	require.EqualValues(t, 1, fx.gateway.saleCalls.Load())
	_, err := fx.db.Pool().Exec(fx.ctx, `UPDATE billing.rail_intents SET expires_at=now()-interval '1 minute',next_attempt_at=now() WHERE id=$1`, intent.ID)
	require.NoError(t, err)
	// New runner/store objects simulate process restart over the same durable row.
	restart := *fx.runner
	restart.Store = intents.NewStore(fx.db)
	fx.runner = &restart
	fx.advanceClock(2 * time.Minute)
	stats, err := fx.runner.RunVerifyOnce(fx.ctx)
	require.NoError(t, err)
	require.Equal(t, 1, stats.Unknown)
	stats, err = fx.runner.RunExecuteOnce(fx.ctx)
	require.NoError(t, err)
	require.Zero(t, stats.Claimed)
	replay := fx.enqueueAndExecute(t, key)
	require.Equal(t, intent.ID, replay.ID)
	require.Equal(t, intents.StatusUnknownNeedsVerify, replay.Status)
	require.EqualValues(t, 1, replay.Attempts)
	require.EqualValues(t, 1, fx.gateway.saleCalls.Load())
	require.Equal(t, 0, fx.paymentCount(t))
	// Even direct reclaim of the resumed handler is verification-only.
	resumed := replay
	resumed.Attempts = 2
	outcome := fx.runner.Registry.Lookup(TypeNMISale).Execute(fx.ctx, resumed)
	require.Equal(t, intents.OutcomeAmbiguous, outcome.Class)
	require.EqualValues(t, 1, fx.gateway.saleCalls.Load())
	fx.gateway.hidden.Store(false)
	fx.advanceClock(20 * time.Minute)
	_, err = fx.runner.RunVerifyOnce(fx.ctx)
	require.NoError(t, err)
	final, err := intents.NewStore(fx.db).Get(fx.ctx, intent.ID)
	require.NoError(t, err)
	require.Equal(t, intents.StatusSucceeded, final.Status)
	require.EqualValues(t, 1, fx.gateway.saleCalls.Load())
	require.Equal(t, 1, fx.paymentCount(t))
	_, err = fx.runner.RunVerifyOnce(fx.ctx)
	require.NoError(t, err)
	require.Equal(t, 1, fx.paymentCount(t))
}

func TestNMISaleIntent_ProvenPreSendParkCanResume(t *testing.T) {
	fx := newSaleIntentFixture(t)
	handler := fx.runner.Registry.Lookup(TypeNMISale).(*NMISaleIntentHandler)
	resolve := handler.Sale.ResolveNMIClient
	handler.Sale.ResolveNMIClient = func(context.Context, string) (*nmi.NMIClient, error) {
		return nil, errors.New("temporarily unavailable before any request")
	}
	key := "sale-park-" + uuid.NewString()[:8]
	parked := fx.enqueueAndExecute(t, key)
	require.Equal(t, intents.StatusPending, parked.Status)
	require.Zero(t, parked.Attempts)
	require.Zero(t, fx.gateway.saleCalls.Load())
	handler.Sale.ResolveNMIClient = resolve
	resumed := fx.enqueueAndExecute(t, key)
	require.Equal(t, intents.StatusSucceeded, resumed.Status)
	require.EqualValues(t, 1, fx.gateway.saleCalls.Load())
	require.Equal(t, 1, fx.paymentCount(t))
}

func TestNMISaleIntent_ExpiredClaimReconcilesButUnsentQueueExpires(t *testing.T) {
	fx := newSaleIntentFixture(t)
	fx.gateway.saleMode.Store("ambiguous500")
	fx.gateway.hidden.Store(true)
	row := fx.enqueueAndExecute(t, "sale-lease-"+uuid.NewString()[:8])
	_, err := fx.db.Pool().Exec(fx.ctx, `UPDATE billing.rail_intents SET status='in_flight',claimed_until=now()-interval '1 minute',expires_at=now()-interval '1 minute' WHERE id=$1`, row.ID)
	require.NoError(t, err)
	fx.advanceClock(2 * time.Minute)
	stats, err := fx.runner.RunExecuteOnce(fx.ctx)
	require.NoError(t, err)
	require.Equal(t, 1, stats.Unknown)
	recovered, err := intents.NewStore(fx.db).Get(fx.ctx, row.ID)
	require.NoError(t, err)
	require.Equal(t, intents.StatusUnknownNeedsVerify, recovered.Status)
	require.EqualValues(t, 2, recovered.Attempts)
	require.EqualValues(t, 1, fx.gateway.saleCalls.Load())
	// A later pre-send gate can park a reclaimed attempt, but that does not
	// make its original payload mutable or restore expiry/revival semantics.
	_, err = fx.db.Pool().Exec(fx.ctx, `UPDATE billing.rail_intents SET status='pending' WHERE id=$1`, row.ID)
	require.NoError(t, err)
	changed := fx.payload
	changed.AmountMicros += 1_000_000
	existing, err := intents.NewStore(fx.db).Enqueue(fx.ctx, intents.EnqueueParams{MerchantID: row.MerchantID, Provider: row.Rail, IntentType: row.IntentType, PspID: *row.PspID, PriceID: row.PriceID, Payload: changed, IdempotencyKey: row.IdempotencyKey, NextAttemptAt: time.Now(), Origin: intents.OriginUser})
	require.NoError(t, err)
	require.JSONEq(t, string(row.Payload), string(existing.Payload))
	require.EqualValues(t, 2, existing.Attempts)
	fx.advanceClock(10 * time.Minute)
	_, err = fx.runner.RunExecuteOnce(fx.ctx)
	require.NoError(t, err)
	existing, err = intents.NewStore(fx.db).Get(fx.ctx, row.ID)
	require.NoError(t, err)
	require.NotEqual(t, intents.StatusExpired, existing.Status)
	fx.gateway.hidden.Store(false)
	fx.advanceClock(20 * time.Minute)
	_, err = fx.runner.RunVerifyOnce(fx.ctx)
	require.NoError(t, err)
	reconciled, err := intents.NewStore(fx.db).Get(fx.ctx, row.ID)
	require.NoError(t, err)
	require.Equal(t, intents.StatusSucceeded, reconciled.Status)
	require.EqualValues(t, 1, fx.gateway.saleCalls.Load())

	// A queued operation with a proven pre-send park still expires normally.
	handler := fx.runner.Registry.Lookup(TypeNMISale).(*NMISaleIntentHandler)
	handler.Sale.ResolveNMIClient = func(context.Context, string) (*nmi.NMIClient, error) { return nil, errors.New("not armed") }
	queued := fx.enqueueAndExecute(t, "sale-queued-"+uuid.NewString()[:8])
	require.Zero(t, queued.Attempts)
	_, err = fx.db.Pool().Exec(fx.ctx, `UPDATE billing.rail_intents SET expires_at=now()-interval '1 minute' WHERE id=$1`, queued.ID)
	require.NoError(t, err)
	_, err = fx.runner.RunExecuteOnce(fx.ctx)
	require.NoError(t, err)
	expired, err := intents.NewStore(fx.db).Get(fx.ctx, queued.ID)
	require.NoError(t, err)
	require.Equal(t, intents.StatusExpired, expired.Status)
}

// A caller deadline interrupts the provider response but never the ledger
// write: the sale is durably unknown the moment the call returns, the executor
// has nothing to re-run, and the verifier resolves it from provider truth.
func TestNMISaleIntent_DeadlineAfterAcceptanceReconcilesClaim(t *testing.T) {
	fx := newSaleIntentFixture(t)
	fx.gateway.saleMode.Store("hold-response")
	fx.gateway.hidden.Store(true)
	pspID := dbtest.EnsureTestPSP(fx.ctx, t, fx.db.Pool(), dbtest.TestMerchantID.UUID(), "mobius")
	key := NMISaleIdempotencyKey("deadline-" + uuid.NewString())
	deadline, cancel := context.WithTimeout(fx.ctx, time.Second)
	defer cancel()
	_, err := fx.runner.EnqueueAndExecute(deadline, intents.EnqueueParams{MerchantID: dbtest.TestMerchantID.UUID(), Provider: "nmi", IntentType: TypeNMISale, PriceID: &fx.priceID, PspID: pspID, Payload: fx.payload, IdempotencyKey: key, NextAttemptAt: time.Now(), Origin: intents.OriginUser})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.EqualValues(t, 1, fx.gateway.saleCalls.Load())
	require.True(t, fx.gateway.charged.Load())
	var id uuid.UUID
	require.NoError(t, fx.db.Pool().QueryRow(fx.ctx, `SELECT id FROM billing.rail_intents WHERE merchant_id=$1 AND idempotency_key=$2`, dbtest.TestMerchantID.UUID(), key).Scan(&id))
	pending, err := intents.NewStore(fx.db).Get(fx.ctx, id)
	require.NoError(t, err)
	require.Equal(t, intents.StatusUnknownNeedsVerify, pending.Status, "unknown mark must be durable despite the caller deadline")
	fx.advanceClock(5 * time.Minute)
	stats, err := fx.runner.RunExecuteOnce(fx.ctx)
	require.NoError(t, err)
	require.Zero(t, stats.Claimed, "an unknown sale is the verifier's, never re-executed")
	pending, err = intents.NewStore(fx.db).Get(fx.ctx, id)
	require.NoError(t, err)
	require.Equal(t, intents.StatusUnknownNeedsVerify, pending.Status)
	require.EqualValues(t, 1, fx.gateway.saleCalls.Load())
	fx.gateway.hidden.Store(false)
	fx.advanceClock(20 * time.Minute)
	_, err = fx.runner.RunVerifyOnce(fx.ctx)
	require.NoError(t, err)
	finished, err := intents.NewStore(fx.db).Get(fx.ctx, id)
	require.NoError(t, err)
	require.Equal(t, intents.StatusSucceeded, finished.Status)
	require.EqualValues(t, 1, fx.gateway.saleCalls.Load())
	require.Equal(t, 1, fx.paymentCount(t))
}

func TestNMISaleIntent_ProcessorCommunicationResponseRemainsUnknown(t *testing.T) {
	fx := newSaleIntentFixture(t)
	fx.gateway.saleMode.Store("processor-uncertain")
	fx.gateway.hidden.Store(true)
	row := fx.enqueueAndExecute(t, "processor-unknown-"+uuid.NewString())
	require.Equal(t, intents.StatusUnknownNeedsVerify, row.Status)
	fx.advanceClock(2 * time.Minute)
	_, err := fx.runner.RunVerifyOnce(fx.ctx)
	require.NoError(t, err)
	row, err = intents.NewStore(fx.db).Get(fx.ctx, row.ID)
	require.NoError(t, err)
	require.Equal(t, intents.StatusUnknownNeedsVerify, row.Status)
	require.EqualValues(t, 1, fx.gateway.saleCalls.Load())
	require.Zero(t, fx.paymentCount(t))
}
