//go:build integration

package checkout

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/payments/rails/vaultedcard"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/pkg/merchant"
)

// #795 spec-C tests 5-8 with the BT leg served by a fake wire server (we have
// no BT tenant yet — BT_TEST_API_KEY-gated live tests come with the contract).
// The DB side (instrument rows, anchors, payments rows, token_type) is real.

// fakeBTServer speaks the BT wire shapes the checkout flow touches: token
// intents, ephemeral proxy (returning canned NMI classic bodies), token
// conversion, network tokens.
type fakeBTServer struct {
	srv *httptest.Server

	intentID    string
	tokenID     string
	ntID        string
	fingerprint string

	proxyMode   atomic.Value // approve | decline | bt401 | ambiguous408
	ntMode      atomic.Value // ok | ineligible
	intentGone  atomic.Bool
	proxyCalls  atomic.Int64
	txnID       string
	lastProxy   atomic.Value // url.Values of the last proxy form
	convertMode atomic.Value // ok | fail500
}

func newFakeBTServer(t *testing.T) *fakeBTServer {
	t.Helper()
	f := &fakeBTServer{
		intentID:    uuid.NewString(),
		tokenID:     uuid.NewString(),
		ntID:        uuid.NewString(),
		fingerprint: "fp_" + uuid.NewString()[:12],
		txnID:       "vtxn" + uuid.NewString()[:8],
	}
	f.proxyMode.Store("approve")
	f.ntMode.Store("ok")
	f.convertMode.Store("ok")
	cardJSON := `{"bin":"411111","last4":"1111","brand":"visa","funding":"credit","expiration_month":12,"expiration_year":2031}`
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/token-intents/"+f.intentID:
			if f.intentGone.Load() {
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprint(w, `{"title":"Not Found","status":404}`)
				return
			}
			fmt.Fprintf(w, `{"id":"%s","type":"card","fingerprint":"%s","card":%s,"expires_at":"%s"}`,
				f.intentID, f.fingerprint, cardJSON, time.Now().Add(23*time.Hour).UTC().Format(time.RFC3339))
		case r.Method == http.MethodPost && r.URL.Path == "/proxy":
			f.proxyCalls.Add(1)
			_ = r.ParseForm()
			f.lastProxy.Store(r.PostForm)
			switch f.proxyMode.Load().(string) {
			case "decline":
				w.Header().Set("BT-PROXY-DESTINATION-STATUS", "200")
				fmt.Fprint(w, "response=2&responsetext=DECLINED&response_code=202&transactionid=999")
			case "bt401":
				w.WriteHeader(http.StatusUnauthorized)
				fmt.Fprint(w, `{"proxy_error":{"title":"Unauthorized","status":401,"detail":"The BT-API-KEY is invalid"}}`)
			case "ambiguous408":
				w.WriteHeader(http.StatusRequestTimeout)
			default:
				w.Header().Set("BT-PROXY-DESTINATION-STATUS", "200")
				fmt.Fprintf(w, "response=1&responsetext=SUCCESS&authcode=OK&transactionid=%s&response_code=100", f.txnID)
			}
		case r.Method == http.MethodPost && r.URL.Path == "/tokens":
			if f.convertMode.Load().(string) == "fail500" {
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprint(w, `{"title":"boom","status":500}`)
				return
			}
			fmt.Fprintf(w, `{"id":"%s","type":"card","fingerprint":"%s","card":%s}`, f.tokenID, f.fingerprint, cardJSON)
		case r.Method == http.MethodPost && r.URL.Path == "/network-tokens":
			if f.ntMode.Load().(string) == "ineligible" {
				w.WriteHeader(http.StatusUnprocessableEntity)
				fmt.Fprint(w, `{"title":"CARD_NOT_ELIGIBLE","status":422}`)
				return
			}
			fmt.Fprintf(w, `{"id":"%s","tenant_id":"tnt_test","status":"active","par":"Q1J4z0aBc","network_token":{"bin":"411111","last4":"1111","brand":"visa","expiration_month":12,"expiration_year":2031},"token_id":"%s","_extras":{"deduplicated":false}}`, f.ntID, f.tokenID)
		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprintf(w, `{"title":"unhandled fake BT route %s %s","status":404}`, r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeBTServer) lastProxyForm(t *testing.T) url.Values {
	t.Helper()
	v, _ := f.lastProxy.Load().(url.Values)
	require.NotNil(t, v, "no proxy call recorded")
	return v
}

type vaultedCardFixture struct {
	db      *db.DB
	runner  *intents.Runner
	bt      *fakeBTServer
	svc     *CheckoutVaultedCardService
	userID  string
	priceID uuid.UUID
	ctx     context.Context
}

func newVaultedCardFixture(t *testing.T, networkTokens bool) *vaultedCardFixture {
	t.Helper()
	dsn := dbtest.MerchantPinnedDSN(t, dbtest.TestMerchantID.UUID())
	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbi.Pool()
	dbtest.EnsureTestMerchant(context.Background(), t, pool)
	ctx := merchant.WithID(context.Background(), dbtest.TestMerchantID)

	now := time.Now().UTC().Truncate(time.Second)
	userID := uuid.New().String()
	customerID := dbtest.EnsureCustomerIDPgx(ctx, t, pool, userID)
	productID, priceID := uuid.New(), uuid.New()
	insertProductAndPrice(ctx, t, pool, &models.Product{
		ID: productID, Key: "vc-intent-" + uuid.NewString()[:8], DisplayName: "VaultedCard Test",
		Archived: false, CreatedAt: now, UpdatedAt: now,
	}, &models.Price{
		ID: priceID, ProductID: productID, Archived: false,
		Amount: 1_990_000, Currency: "USD", CreatedAt: now, UpdatedAt: now,
	})
	bt := newFakeBTServer(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.rail_intents WHERE intent_type = 'vaulted_card_sale' AND price_id = $1", priceID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.entitlements WHERE customer_id = $1", customerID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.payments WHERE customer_id = $1", customerID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.payment_methods WHERE customer_id = $1", customerID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.prices WHERE id = $1", priceID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.products WHERE id = $1", productID)
	})

	clock := clockwork.NewRealClock()
	rails := railresolve.FixedSet{
		"bt": {
			Rail:      models.RailVaultedCard,
			AccountID: "tnt_test",
			VaultedCard: &config.VaultedCardRailConfig{
				APIKey:             "key_private_test",
				GatewayAccountID:   "579145",
				GatewaySecurityKey: "sk_gateway_test",
				NetworkTokens:      networkTokens,
				APIBaseURL:         bt.srv.URL,
			},
		},
	}
	svc := &CheckoutVaultedCardService{
		DB: dbi,
		PurchaseService: NewCheckoutPurchaseService(
			catalog.NewPriceService(dbi),
			catalog.NewProductService(dbi),
			payments.NewPaymentService(dbi, clock),
			entitlements.NewEntitlementService(dbi, clock),
			nil,
			clock,
		),
		PaymentMethodService: paymentmethods.NewPaymentMethodService(dbi),
		Rails:                rails,
		Config:               nil,
		DisableGatewayVerify: true, // fake BT serves no query.php
	}
	runner := &intents.Runner{
		Store:    intents.NewStore(dbi),
		Registry: intents.NewRegistry(NewVaultedCardSaleIntentHandler(svc)),
	}
	svc.Intents = runner
	return &vaultedCardFixture{db: dbi, runner: runner, bt: bt, svc: svc, userID: userID, priceID: priceID, ctx: ctx}
}

func (fx *vaultedCardFixture) enqueueAndExecute(t *testing.T, key string) gen.OpenrailsRailIntent {
	t.Helper()
	intent, err := fx.runner.EnqueueAndExecute(fx.ctx, intents.EnqueueParams{
		MerchantID: dbtest.TestMerchantID.UUID(),
		Provider:   string(models.RailVaultedCard),
		IntentType: TypeVaultedCardSale,
		PriceID:    &fx.priceID,
		Payload: VaultedCardSalePayload{
			TokenIntentID: fx.bt.intentID,
			AmountMicros:  1_990_000,
			Currency:      "USD",
			Description:   "Purchase: VaultedCard Test",
			UserID:        fx.userID,
			PriceID:       fx.priceID,
		},
		IdempotencyKey: VaultedCardSaleIdempotencyKey(key),
		NextAttemptAt:  time.Now().UTC(),
		Origin:         intents.OriginUser,
		OriginReason:   "test vaulted_card sale",
	})
	require.NoError(t, err)
	return intent
}

// TestVaultedCardSale_CollectChargeConvert is spec C5 (fake BT leg): CIT
// charges the INTENT with CVC + indicator=stored, converts in-request, writes
// the instrument row, anchors the unscheduled sequence, and registers the
// purchase with token_type=pan_via_vault.
func TestVaultedCardSale_CollectChargeConvert(t *testing.T) {
	fx := newVaultedCardFixture(t, false)
	intent := fx.enqueueAndExecute(t, "vc-key-"+uuid.NewString()[:8])
	require.Equal(t, intents.StatusSucceeded, intent.Status)

	// Wire pins: the CIT rode the token INTENT with CVC + stored-credential CIT fields,
	// exact amount 1.99 (1_990_000 micros).
	form := fx.bt.lastProxyForm(t)
	require.Equal(t, "1.99", form.Get("amount"))
	require.Contains(t, form.Get("ccnumber"), "token_intent: "+fx.bt.intentID)
	require.Contains(t, form.Get("cvv"), "token_intent: "+fx.bt.intentID)
	require.Equal(t, "customer", form.Get("initiated_by"))
	require.Equal(t, "stored", form.Get("stored_credential_indicator"))
	require.Equal(t, intent.ID.String(), form.Get("orderid"))
	require.Empty(t, form.Get("initial_transaction_id"))
	require.Equal(t, "sk_gateway_test", form.Get("security_key"))

	// Payments row: completed, token_type pan_via_vault, attempt initial.
	var tokenType, attemptKind string
	require.NoError(t, fx.db.Pool().QueryRow(fx.ctx,
		"SELECT COALESCE(token_type,''), COALESCE(attempt_kind,'') FROM openrails.payments WHERE rail='vaulted_card' AND transaction_id=$1 AND status='completed'",
		fx.bt.txnID).Scan(&tokenType, &attemptKind))
	require.Equal(t, charge.TokenTypePANViaVault, tokenType)
	require.Equal(t, payments.AttemptInitial, attemptKind)

	// Instrument row: BT token id, vault provider, fingerprint, pan_proxy,
	// anchored unscheduled sequence = the NMI transactionid.
	var vaultProvider, fingerprint, chargeVia, anchor, lastFour string
	require.NoError(t, fx.db.Pool().QueryRow(fx.ctx,
		`SELECT vault_provider, vault_fingerprint, charge_via, stored_credential_unscheduled_ref, COALESCE(last_four,'')
		 FROM openrails.payment_methods WHERE rail='vaulted_card' AND rail_method_ref=$1`,
		fx.bt.tokenID).Scan(&vaultProvider, &fingerprint, &chargeVia, &anchor, &lastFour))
	require.Equal(t, vaultedcard.VaultProvider, vaultProvider)
	require.Equal(t, fx.bt.fingerprint, fingerprint)
	require.Equal(t, vaultedcard.ViaPANProxy, chargeVia)
	require.Equal(t, fx.bt.txnID, anchor)
	require.Equal(t, "1111", lastFour)
}

// TestVaultedCardSale_MITRenewalRidesAnchor is the spec-C6 shape against the
// fake wire: after collection, a merchant-initiated charge on the STORED token
// replays the anchor as initial_transaction_id with no CVC — stored-credential
// reuse over the proxy.
func TestVaultedCardSale_MITRenewalRidesAnchor(t *testing.T) {
	fx := newVaultedCardFixture(t, false)
	intent := fx.enqueueAndExecute(t, "vc-key-"+uuid.NewString()[:8])
	require.Equal(t, intents.StatusSucceeded, intent.Status)

	cfg, err := fx.svc.resolveConfig(fx.ctx)
	require.NoError(t, err)
	charger, err := fx.svc.charger(cfg)
	require.NoError(t, err)

	res, err := charger.WithSource(vaultedcard.Source{TokenID: fx.bt.tokenID}).Charge(fx.ctx, charge.Request{
		Instrument:  charge.Instrument{Rail: vaultedcard.Rail, MethodRef: fx.bt.tokenID},
		AmountMinor: 199,
		Currency:    "USD",
		Description: "Renewal",
		OrderRef:    "renewal-" + uuid.NewString()[:8],
		Context:     charge.RecurringMIT(fx.bt.txnID),
	})
	require.NoError(t, err)
	require.False(t, res.Declined)
	require.Equal(t, charge.TokenTypePANViaVault, res.TokenType)
	require.Empty(t, res.CapturedRef, "MIT with a prior ref never re-anchors")

	form := fx.bt.lastProxyForm(t)
	require.Contains(t, form.Get("ccnumber"), "token: "+fx.bt.tokenID)
	require.Empty(t, form.Get("cvv"), "MIT is CVC-less")
	require.Equal(t, "merchant", form.Get("initiated_by"))
	require.Equal(t, "used", form.Get("stored_credential_indicator"))
	require.Equal(t, fx.bt.txnID, form.Get("initial_transaction_id"))
	require.Equal(t, "recurring", form.Get("billing_method"))
}

// TestVaultedCardSale_DeclineWritesFailedRow is spec C7 + #796: a parsed
// decline is terminal AND lands as a failed payments row with the verbatim
// code and token_type.
func TestVaultedCardSale_DeclineWritesFailedRow(t *testing.T) {
	fx := newVaultedCardFixture(t, false)
	fx.bt.proxyMode.Store("decline")

	intent := fx.enqueueAndExecute(t, "vc-key-"+uuid.NewString()[:8])
	require.Equal(t, intents.StatusFailedTerminal, intent.Status)

	var failureCode, failureReason, tokenType string
	require.NoError(t, fx.db.Pool().QueryRow(fx.ctx,
		`SELECT COALESCE(failure_code,''), COALESCE(failure_reason,''), COALESCE(token_type,'')
		 FROM openrails.payments WHERE rail='vaulted_card' AND status='failed' AND transaction_id=$1`,
		"vaulted_card_sale_declined:"+intent.ID.String()).Scan(&failureCode, &failureReason, &tokenType))
	require.Equal(t, "insufficient_funds", failureCode) // NMI 202, verbatim localization id
	require.Equal(t, payments.FailureInsufficientFunds, failureReason)
	require.Equal(t, charge.TokenTypePANViaVault, tokenType)
}

// TestVaultedCardSale_BTFailureIsNotADecline is spec C8: a BT pre-forward
// failure parks the intent — no decline, no failed payments row.
func TestVaultedCardSale_BTFailureIsNotADecline(t *testing.T) {
	fx := newVaultedCardFixture(t, false)
	fx.bt.proxyMode.Store("bt401")

	intent := fx.enqueueAndExecute(t, "vc-key-"+uuid.NewString()[:8])
	require.Equal(t, intents.StatusPending, intent.Status, "BT pre-forward failure parks (operator fix), never a decline")

	var n int
	require.NoError(t, fx.db.Pool().QueryRow(fx.ctx,
		"SELECT count(*) FROM openrails.payments WHERE rail='vaulted_card' AND price_id=$1", fx.priceID).Scan(&n))
	require.Zero(t, n)
}

// TestVaultedCardSale_AmbiguousNeverDeclines: a 408 (BT may have forwarded)
// parks the intent as unknown_needs_verify.
func TestVaultedCardSale_AmbiguousNeverDeclines(t *testing.T) {
	fx := newVaultedCardFixture(t, false)
	fx.bt.proxyMode.Store("ambiguous408")

	intent := fx.enqueueAndExecute(t, "vc-key-"+uuid.NewString()[:8])
	require.Equal(t, intents.StatusUnknownNeedsVerify, intent.Status)
	var n int
	require.NoError(t, fx.db.Pool().QueryRow(fx.ctx,
		"SELECT count(*) FROM openrails.payments WHERE rail='vaulted_card' AND price_id=$1", fx.priceID).Scan(&n))
	require.Zero(t, n, "ambiguity is never recorded as an outcome")
}

// TestVaultedCardSale_ExpiredIntentIsLoud is spec C9: a >24h-old intent is a
// terminal, fabrication-free error.
func TestVaultedCardSale_ExpiredIntentIsLoud(t *testing.T) {
	fx := newVaultedCardFixture(t, false)
	fx.bt.intentGone.Store(true)

	intent := fx.enqueueAndExecute(t, "vc-key-"+uuid.NewString()[:8])
	require.Equal(t, intents.StatusFailedTerminal, intent.Status)
	require.NotNil(t, intent.LastFailureReason)
	require.Contains(t, *intent.LastFailureReason, "expire")
}

// TestVaultedCardSale_NTProvisioning is spec C10 (fake leg): with the flag
// armed, a successful checkout provisions an NT and persists id/status/PAR;
// an NT failure warns and the instrument stays pan_proxy.
func TestVaultedCardSale_NTProvisioning(t *testing.T) {
	t.Run("provisioned", func(t *testing.T) {
		fx := newVaultedCardFixture(t, true)
		intent := fx.enqueueAndExecute(t, "vc-key-"+uuid.NewString()[:8])
		require.Equal(t, intents.StatusSucceeded, intent.Status)

		var ntID, ntStatus, par, chargeVia string
		require.NoError(t, fx.db.Pool().QueryRow(fx.ctx,
			`SELECT network_token_id, network_token_status, network_token_par, charge_via
			 FROM openrails.payment_methods WHERE rail='vaulted_card' AND rail_method_ref=$1`,
			fx.bt.tokenID).Scan(&ntID, &ntStatus, &par, &chargeVia))
		require.Equal(t, fx.bt.ntID, ntID)
		require.Equal(t, "active", ntStatus)
		require.Equal(t, "Q1J4z0aBc", par)
		require.Equal(t, vaultedcard.ViaPANProxy, chargeVia, "NT provisioning never flips charge routing on NMI")
	})
	t.Run("failure is never load-bearing", func(t *testing.T) {
		fx := newVaultedCardFixture(t, true)
		fx.bt.ntMode.Store("ineligible")
		intent := fx.enqueueAndExecute(t, "vc-key-"+uuid.NewString()[:8])
		require.Equal(t, intents.StatusSucceeded, intent.Status, "NT failure must not fail the checkout")

		var ntID string
		require.NoError(t, fx.db.Pool().QueryRow(fx.ctx,
			"SELECT network_token_id FROM openrails.payment_methods WHERE rail='vaulted_card' AND rail_method_ref=$1",
			fx.bt.tokenID).Scan(&ntID))
		require.Empty(t, ntID)
	})
}

// TestVaultedCardCollectionAdapter_ParkedInstrumentFailsClosed: a parked
// instrument refuses MIT collection loudly.
func TestVaultedCardCollectionAdapter_ParkedInstrumentFailsClosed(t *testing.T) {
	fx := newVaultedCardFixture(t, false)
	intent := fx.enqueueAndExecute(t, "vc-key-"+uuid.NewString()[:8])
	require.Equal(t, intents.StatusSucceeded, intent.Status)
	_, err := fx.db.Pool().Exec(fx.ctx,
		"UPDATE openrails.payment_methods SET park_reason='bt_token_deleted', parked_at=now() WHERE rail='vaulted_card' AND rail_method_ref=$1",
		fx.bt.tokenID)
	require.NoError(t, err)

	cfg, err := fx.svc.resolveConfig(fx.ctx)
	require.NoError(t, err)
	charger, err := fx.svc.charger(cfg)
	require.NoError(t, err)

	method, err := fx.db.Gen(fx.ctx).GetPaymentMethodByRailMethodRef(fx.ctx, gen.GetPaymentMethodByRailMethodRefParams{
		Rail:          string(models.RailVaultedCard),
		RailMethodRef: fx.bt.tokenID,
	})
	require.NoError(t, err)
	adapter := money.NewVaultedCardCollectionAdapter(charger)
	_, err = adapter.ChargeSavedMethod(fx.ctx, method, money.ChargeRequest{
		MerchantID:      dbtest.TestMerchantID.UUID(),
		PaymentMethodID: method.ID,
		AmountCents:     199,
		Currency:        "USD",
		IdempotencyKey:  "invoice:test:attempt:0",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "parked")
}

// TestPANFirewall (unit-shaped, kept here for the fixture types): raw PANs are
// rejected loudly; UUID handles pass.
func TestPANFirewall(t *testing.T) {
	req := &CheckoutRequest{BTTokenIntentID: uuid.NewString(), Metadata: map[string]string{}}
	require.NoError(t, RejectPANShapedFields(req))

	req.Metadata["note"] = "4111 1111 1111 1111"
	err := RejectPANShapedFields(req)
	require.Error(t, err)
	require.Contains(t, err.Error(), "card-number-shaped")

	req.Metadata = nil
	req.PaymentToken = "4242424242424242"
	require.Error(t, RejectPANShapedFields(req))

	// Non-Luhn digit runs (order numbers etc.) pass.
	req.PaymentToken = "1234567890123"
	require.NoError(t, RejectPANShapedFields(req))
}
