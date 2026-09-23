//go:build integration

package checkout

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/productaccess"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/stretchr/testify/require"
)

func purchaseSessionFixture(t *testing.T) (*saleIntentFixture, *CheckoutSessionService, *CheckoutService, uuid.UUID) {
	t.Helper()
	fx := newSaleIntentFixture(t)
	psp := dbtest.EnsureTestPSP(fx.ctx, t, fx.db.Pool(), fx.merchantID.UUID(), "stripe")
	cfg := &config.Config{ProviderWriteMode: config.ProviderWriteModeFull, TestMode: config.CredentialPostureSandbox, NewSubscriptionCollectionPolicy: "engine"}
	rails := railresolve.FixedSet{"stripe": {Rail: models.RailStripe, AccountID: "acct_test", Stripe: &config.StripeRailConfig{SecretKey: "sk_test_checkout_1051"}}}
	core := &CheckoutService{Config: cfg, Rails: rails, ProviderSecrets: fakePSPCatalog{scopes: []merchants.PSPScope{{ID: psp, Key: "stripe", Rail: "stripe", Environment: "test", AccountID: "acct_test"}}}, PriceService: fx.purchase.PriceService, ProductService: fx.purchase.ProductService, PurchaseService: fx.purchase}
	sessions := NewCheckoutSessionService(fx.db, core.PriceService, core.ProductService, nil, nil, core, nil, nil, nil, nil, cfg, rails)
	return fx, sessions, core, psp
}

func purchaseSessionRequest(fx *saleIntentFixture, key string) *CheckoutSessionCreateRequest {
	return &CheckoutSessionCreateRequest{PriceID: openrails.PriceID(fx.priceID).String(), Payment: CheckoutSessionPaymentRequest{Rail: "stripe"}, IdempotencyKey: key, SuccessURL: "https://blog.example/paid", CancelURL: "https://blog.example/canceled"}
}

func stripeCheckoutOK() *http.Response {
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"id":"cs_test_1051","url":"https://checkout.stripe.test/accepted"}`))}
}

func TestPermanentCheckoutConcurrentKeysCreateOnePayableSession(t *testing.T) {
	fx, service, _, _ := purchaseSessionFixture(t)
	var calls atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	restore := stripeapi.InstallBaseTransport(initialStripeWireUnit(func(r *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			close(entered)
		}
		<-release
		return stripeCheckoutOK(), nil
	}))
	defer restore()
	type result struct {
		response *CheckoutSessionResponse
		err      error
	}
	first := make(chan result, 1)
	go func() {
		response, err := service.CreateSession(fx.ctx, purchaseSessionRequest(fx, "winner"), &UserIdentity{ID: fx.userID})
		first <- result{response, err}
	}()
	select {
	case <-entered:
	case got := <-first:
		t.Fatalf("checkout failed before provider: %v", got.err)
	case <-time.After(20 * time.Second):
		t.Fatal("checkout did not reach fake provider")
	}
	var group sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		group.Add(1)
		go func(i int) {
			defer group.Done()
			_, errs[i] = service.CreateSession(fx.ctx, purchaseSessionRequest(fx, uuid.NewString()), &UserIdentity{ID: fx.userID})
		}(i)
	}
	group.Wait()
	for _, err := range errs {
		require.ErrorIs(t, err, ErrCheckoutSessionConflict)
	}
	require.EqualValues(t, 1, calls.Load())
	once.Do(func() { close(release) })
	winner := <-first
	require.NoError(t, winner.err)
	require.Equal(t, "requires_action", winner.response.Status)
	replay, err := service.CreateSession(fx.ctx, purchaseSessionRequest(fx, "winner"), &UserIdentity{ID: fx.userID})
	require.NoError(t, err)
	require.Equal(t, winner.response.ID, replay.ID)
	require.EqualValues(t, 1, calls.Load())
}

func TestPurchaseDatabaseReplayKeepsOriginalOfferAfterArchive(t *testing.T) {
	fx, service, core, _ := purchaseSessionFixture(t)
	var calls atomic.Int32
	restore := stripeapi.InstallBaseTransport(initialStripeWireUnit(func(r *http.Request) (*http.Response, error) { calls.Add(1); return stripeCheckoutOK(), nil }))
	defer restore()
	key := "post-offer-" + uuid.NewString()
	_, err := fx.db.Pool().Exec(fx.ctx, `UPDATE billing.prices SET key=$1 WHERE id=$2`, key, fx.priceID)
	require.NoError(t, err)
	request := func() *CheckoutSessionCreateRequest {
		req := purchaseSessionRequest(fx, "same-key")
		req.PriceID = ""
		req.PriceKey = key
		return req
	}
	original, err := service.CreateSession(fx.ctx, request(), &UserIdentity{ID: fx.userID})
	require.NoError(t, err)
	_, err = fx.db.Pool().Exec(fx.ctx, `UPDATE billing.prices SET archived=true WHERE id=$1`, fx.priceID)
	require.NoError(t, err)
	_, err = fx.db.Pool().Exec(fx.ctx, `UPDATE billing.products SET archived=true WHERE id=$1`, fx.productID)
	require.NoError(t, err)
	// There is no replay cache. Removing current routing proves DB replay does
	// not need the now-unavailable provider/catalog selection.
	core.ProviderSecrets = fakePSPCatalog{}
	replay, err := service.CreateSession(fx.ctx, request(), &UserIdentity{ID: fx.userID})
	require.NoError(t, err)
	require.Equal(t, original.ID, replay.ID)
	require.Equal(t, original.Amount, replay.Amount)
	require.Equal(t, original.PriceID, replay.PriceID)
	require.EqualValues(t, 1, calls.Load())
	changed := request()
	changed.SuccessURL = "https://blog.example/different"
	_, err = service.CreateSession(fx.ctx, changed, &UserIdentity{ID: fx.userID})
	require.ErrorIs(t, err, ErrCheckoutSessionConflict)
	newAttempt := request()
	newAttempt.IdempotencyKey = "new-after-archive"
	_, err = service.CreateSession(fx.ctx, newAttempt, &UserIdentity{ID: fx.userID})
	require.ErrorIs(t, err, ErrCheckoutSessionValidation)
	_, err = service.CreateSession(fx.ctx, request(), &UserIdentity{ID: uuid.NewString()})
	require.ErrorIs(t, err, ErrCheckoutSessionValidation)
}

func TestUnknownHostedPurchaseRetainsExclusionUntilProviderClosure(t *testing.T) {
	fx, service, _, psp := purchaseSessionFixture(t)
	var calls atomic.Int32
	restore := stripeapi.InstallBaseTransport(initialStripeWireUnit(func(r *http.Request) (*http.Response, error) { calls.Add(1); return nil, io.ErrUnexpectedEOF }))
	defer restore()
	_, err := service.CreateSession(fx.ctx, purchaseSessionRequest(fx, "uncertain"), &UserIdentity{ID: fx.userID})
	require.Error(t, err)
	id := idempotentCheckoutSessionID(fx.merchantID.UUID(), scopeIdempotencyKey(fx.userID, "uncertain"))
	_, err = fx.db.Pool().Exec(fx.ctx, `UPDATE billing.checkout_sessions SET status='expired', expires_at=now()-interval '1 day' WHERE id=$1`, id)
	require.NoError(t, err)
	_, err = service.CreateSession(fx.ctx, purchaseSessionRequest(fx, "uncertain"), &UserIdentity{ID: fx.userID})
	require.Error(t, err)
	_, err = service.CreateSession(fx.ctx, purchaseSessionRequest(fx, "different-key"), &UserIdentity{ID: fx.userID})
	require.ErrorIs(t, err, ErrCheckoutSessionConflict)
	require.EqualValues(t, 1, calls.Load(), "unknown hosted create must never be sent again")
	err = service.MarkProviderCheckoutClosed(db.WithPSPID(fx.ctx, uuid.New()), id, models.CheckoutSessionStatusExpired)
	require.ErrorIs(t, err, ErrCheckoutSessionNotFound)
	err = service.MarkProviderCheckoutClosed(db.WithPSPID(fx.ctx, psp), id, models.CheckoutSessionStatusExpired)
	require.NoError(t, err)
	_, err = service.CreateSession(fx.ctx, purchaseSessionRequest(fx, "after-provider-closed"), &UserIdentity{ID: fx.userID})
	require.Error(t, err) // Synthetic transport still fails, but this is a NEW legitimate attempt.
	require.EqualValues(t, 2, calls.Load())
}

func TestSessionSettlementPreservesAcceptedBenefitsAndLatePayment(t *testing.T) {
	fx, service, _, psp := purchaseSessionFixture(t)
	_, err := fx.db.Pool().Exec(fx.ctx, `UPDATE billing.products SET entitlements_spec='{"accepted_post":null}' WHERE id=$1`, fx.productID)
	require.NoError(t, err)
	restore := stripeapi.InstallBaseTransport(initialStripeWireUnit(func(r *http.Request) (*http.Response, error) { return stripeCheckoutOK(), nil }))
	defer restore()
	original, err := service.CreateSession(fx.ctx, purchaseSessionRequest(fx, "accepted"), &UserIdentity{ID: fx.userID})
	require.NoError(t, err)
	_, err = fx.db.Pool().Exec(fx.ctx, `UPDATE billing.products SET entitlements_spec='{"replacement_post":null}', archived=true WHERE id=$1`, fx.productID)
	require.NoError(t, err)
	_, err = fx.db.Pool().Exec(fx.ctx, `UPDATE billing.checkout_sessions SET status='expired', expires_at=now()-interval '1 day' WHERE id=$1`, original.ID.UUID())
	require.NoError(t, err)
	ctx := db.WithPSPID(fx.ctx, psp)
	req := &payments.RegisterPurchaseRequest{CheckoutSessionID: original.ID.UUID(), UserID: fx.userID, PriceID: fx.priceID, Rail: "stripe", TransactionID: "pi_accepted_1051", Amount: 5_000_000, AmountProvided: true, Currency: "USD"}
	paid, err := fx.purchase.RegisterPurchase(ctx, req)
	require.NoError(t, err)
	stored, err := fx.purchase.PaymentService.GetByID(ctx, paid.PaymentID)
	require.NoError(t, err)
	require.Contains(t, stored.EntitlementsSpecSnapshot, "accepted_post")
	require.NotContains(t, stored.EntitlementsSpecSnapshot, "replacement_post")
	row, err := service.repo.GetByID(ctx, original.ID.UUID())
	require.NoError(t, err)
	require.Equal(t, models.CheckoutSessionStatusSucceeded, row.Status)
	replay, err := service.CreateSession(fx.ctx, purchaseSessionRequest(fx, "accepted"), &UserIdentity{ID: fx.userID})
	require.NoError(t, err)
	require.Equal(t, original.ID, replay.ID)
	require.Equal(t, "succeeded", replay.Status)
	wrong := *req
	wrong.UserID = uuid.NewString()
	dbtest.EnsureCustomerIDPgxFor(fx.ctx, t, fx.db.Pool(), fx.merchantID.UUID(), wrong.UserID)
	_, err = fx.purchase.RegisterPurchase(ctx, &wrong)
	require.Error(t, err)
	wrong = *req
	wrong.Amount++
	_, err = fx.purchase.RegisterPurchase(ctx, &wrong)
	require.Error(t, err)
}

func TestOwnershipOnlyPurchaseRefundAndFiniteEligibility(t *testing.T) {
	fx, service, _, psp := purchaseSessionFixture(t)
	restore := stripeapi.InstallBaseTransport(initialStripeWireUnit(func(r *http.Request) (*http.Response, error) { return stripeCheckoutOK(), nil }))
	defer restore()
	original, err := service.CreateSession(fx.ctx, purchaseSessionRequest(fx, "buy"), &UserIdentity{ID: fx.userID})
	require.NoError(t, err)
	ctx := db.WithPSPID(fx.ctx, psp)
	paid, err := fx.purchase.RegisterPurchase(ctx, &payments.RegisterPurchaseRequest{CheckoutSessionID: original.ID.UUID(), UserID: fx.userID, PriceID: fx.priceID, Rail: "stripe", TransactionID: "pi_ownership_1051", Amount: 5_000_000, AmountProvided: true, Currency: "USD"})
	require.NoError(t, err)
	eligibility, err := fx.purchase.CheckPurchaseEligibility(ctx, fx.userID, fx.priceID)
	require.NoError(t, err)
	require.Equal(t, EligibilityBlocked, eligibility.Status)
	_, err = service.CreateSession(fx.ctx, purchaseSessionRequest(fx, "duplicate"), &UserIdentity{ID: fx.userID})
	require.ErrorIs(t, err, ErrCheckoutSessionConflict)
	price, err := fx.purchase.PriceService.GetByID(ctx, fx.priceID)
	require.NoError(t, err)
	product, err := fx.purchase.ProductService.GetByID(ctx, fx.productID)
	require.NoError(t, err)
	price.AccessDurationHours = new(24)
	require.NoError(t, fx.purchase.checkPermanentOwnership(ctx, fx.userID, price, product), "finite purchases keep their existing policy")
	price.AccessDurationHours = nil
	price.AutoRenew = true
	require.NoError(t, fx.purchase.checkPermanentOwnership(ctx, fx.userID, price, product), "subscriptions keep their own admission rules")
	_, err = productaccess.NewService(fx.db).RevokeProductAccessByPayment(ctx, paid.PaymentID, models.ProductAccessRevokeRefund)
	require.NoError(t, err)
	rebuy, err := service.CreateSession(fx.ctx, purchaseSessionRequest(fx, "rebuy"), &UserIdentity{ID: fx.userID})
	require.NoError(t, err)
	require.NotEqual(t, original.ID, rebuy.ID)
	replay, err := service.CreateSession(fx.ctx, purchaseSessionRequest(fx, "buy"), &UserIdentity{ID: fx.userID})
	require.NoError(t, err)
	require.Equal(t, original.ID, replay.ID)
}

func TestHostedPurchaseAndNMIIntentSharePermanentAdmission(t *testing.T) {
	for _, nmiFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "hosted_first", true: "nmi_first"}[nmiFirst], func(t *testing.T) {
			fx, service, _, _ := purchaseSessionFixture(t)
			restore := stripeapi.InstallBaseTransport(initialStripeWireUnit(func(r *http.Request) (*http.Response, error) { return stripeCheckoutOK(), nil }))
			defer restore()
			params := saleAdmissionParams(fx, uuid.NewString())
			store := intents.NewStore(fx.db)
			if nmiFirst {
				_, err := store.Enqueue(fx.ctx, params)
				require.NoError(t, err)
				_, err = service.CreateSession(fx.ctx, purchaseSessionRequest(fx, "stripe-after-nmi"), &UserIdentity{ID: fx.userID})
				require.ErrorIs(t, err, ErrCheckoutSessionConflict)
			} else {
				_, err := service.CreateSession(fx.ctx, purchaseSessionRequest(fx, "stripe-before-nmi"), &UserIdentity{ID: fx.userID})
				require.NoError(t, err)
				_, err = store.Enqueue(fx.ctx, params)
				require.Error(t, err)
				require.Contains(t, err.Error(), "unresolved")
			}
			require.Zero(t, fx.gateway.saleCalls.Load())
		})
	}
}

func admittedPurchaseFixture(t *testing.T, fx *saleIntentFixture, service *CheckoutSessionService, psp uuid.UUID, key string, rail models.Rail) *models.CheckoutSession {
	t.Helper()
	req := purchaseSessionRequest(fx, key)
	req.Payment.Rail = string(rail)
	now := service.now()
	session := &models.CheckoutSession{ID: idempotentCheckoutSessionID(fx.merchantID.UUID(), scopeIdempotencyKey(fx.userID, key)), CustomerID: fx.customerID, PriceID: &fx.priceID, Mode: models.CheckoutSessionModeOneOff, Rail: rail, PspID: psp, Status: models.CheckoutSessionStatusCreated, Amount: new(int64(5_000_000)), Currency: new("USD"), CreatedAt: now, UpdatedAt: now, RailState: map[string]any{checkoutSessionFingerprintKey: checkoutSessionRequestFingerprintForRail(req, &UserIdentity{ID: fx.userID}, string(rail))}}
	require.NoError(t, service.admitPurchaseSession(fx.ctx, session))
	return session
}

func TestPurchaseCrashBeforeDispatchResumesFrozenArchivedTerms(t *testing.T) {
	fx, service, _, psp := purchaseSessionFixture(t)
	original := admittedPurchaseFixture(t, fx, service, psp, "crash-before-dispatch", models.RailStripe)
	_, err := fx.db.Pool().Exec(fx.ctx, `UPDATE billing.products SET display_name='Changed later', archived=true WHERE id=$1`, fx.productID)
	require.NoError(t, err)
	_, err = fx.db.Pool().Exec(fx.ctx, `UPDATE billing.prices SET archived=true WHERE id=$1`, fx.priceID)
	require.NoError(t, err)
	calls := 0
	restore := stripeapi.InstallBaseTransport(initialStripeWireUnit(func(r *http.Request) (*http.Response, error) {
		calls++
		require.NoError(t, r.ParseForm())
		require.Equal(t, "500", r.Form.Get("line_items[0][price_data][unit_amount]"))
		require.Equal(t, "Sale Intent Test", r.Form.Get("line_items[0][price_data][product_data][name]"))
		return stripeCheckoutOK(), nil
	}))
	defer restore()
	resumed, err := service.CreateSession(fx.ctx, purchaseSessionRequest(fx, "crash-before-dispatch"), &UserIdentity{ID: fx.userID})
	require.NoError(t, err)
	require.Equal(t, original.ID, resumed.ID.UUID())
	require.Equal(t, 1, calls)
}

func TestPurchaseProgressCannotEraseDispatchOrAcceptedTerms(t *testing.T) {
	fx, service, _, psp := purchaseSessionFixture(t)
	stale := admittedPurchaseFixture(t, fx, service, psp, "stale-writer", models.RailStripe)
	originalTerms, err := purchaseTerms(stale)
	require.NoError(t, err)
	n, err := fx.db.Gen(fx.ctx).ClaimHostedPurchaseDispatch(fx.ctx, gen.ClaimHostedPurchaseDispatchParams{MerchantID: fx.merchantID.UUID(), ID: stale.ID})
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
	stale.RailState = map[string]any{"message": "stale progress", acceptedPurchaseTermsKey: "invalid replacement"}
	require.NoError(t, service.repo.Update(fx.ctx, stale))
	current, err := service.repo.GetByID(fx.ctx, stale.ID)
	require.NoError(t, err)
	require.Equal(t, true, current.RailState["purchase_submitted"])
	terms, err := purchaseTerms(current)
	require.NoError(t, err)
	require.Equal(t, originalTerms.PaymentID, terms.PaymentID)
	require.Equal(t, originalTerms.Amount, terms.Amount)
	require.NoError(t, service.MarkProviderCheckoutClosed(db.WithPSPID(fx.ctx, psp), stale.ID, models.CheckoutSessionStatusExpired))
	require.Error(t, service.repo.Update(fx.ctx, stale), "stale progress cannot reopen verified closure")
	current, err = service.repo.GetByID(fx.ctx, stale.ID)
	require.NoError(t, err)
	require.Equal(t, true, current.RailState["provider_closed"])
	n, err = fx.db.Gen(fx.ctx).ClaimHostedPurchaseDispatch(fx.ctx, gen.ClaimHostedPurchaseDispatchParams{MerchantID: fx.merchantID.UUID(), ID: stale.ID})
	require.NoError(t, err)
	require.Zero(t, n)
}

func TestPurchasePreflightFailureDoesNotReserveAProduct(t *testing.T) {
	fx, service, _, _ := purchaseSessionFixture(t)
	calls := 0
	restore := stripeapi.InstallBaseTransport(initialStripeWireUnit(func(r *http.Request) (*http.Response, error) { calls++; return stripeCheckoutOK(), nil }))
	defer restore()
	bad := purchaseSessionRequest(fx, "missing-return-url")
	bad.SuccessURL = ""
	_, err := service.CreateSession(fx.ctx, bad, &UserIdentity{ID: fx.userID})
	require.ErrorContains(t, err, "success_url")
	require.Zero(t, calls)
	_, err = service.CreateSession(fx.ctx, purchaseSessionRequest(fx, "valid-new-attempt"), &UserIdentity{ID: fx.userID})
	require.NoError(t, err)
	require.Equal(t, 1, calls)
	bad = purchaseSessionRequest(fx, "missing-return-url")
	bad.SuccessURL = ""
	_, err = service.CreateSession(fx.ctx, bad, &UserIdentity{ID: fx.userID})
	require.Error(t, err)
	require.Equal(t, 1, calls)
}

func TestPurchaseWebhookWinsBeforeCreateHandlerStoresRedirect(t *testing.T) {
	fx, service, _, psp := purchaseSessionFixture(t)
	restore := stripeapi.InstallBaseTransport(initialStripeWireUnit(func(r *http.Request) (*http.Response, error) {
		require.NoError(t, r.ParseForm())
		id, err := openrails.ParseCheckoutSessionID(r.Form.Get("metadata[checkout_session_id]"))
		require.NoError(t, err)
		_, err = fx.purchase.RegisterPurchase(db.WithPSPID(fx.ctx, psp), &payments.RegisterPurchaseRequest{CheckoutSessionID: id.UUID(), UserID: fx.userID, PriceID: fx.priceID, Rail: "stripe", TransactionID: "pi_fast_webhook", Amount: 5_000_000, AmountProvided: true, Currency: "USD"})
		require.NoError(t, err)
		return stripeCheckoutOK(), nil
	}))
	defer restore()
	result, err := service.CreateSession(fx.ctx, purchaseSessionRequest(fx, "fast-webhook"), &UserIdentity{ID: fx.userID})
	require.NoError(t, err)
	require.Equal(t, "succeeded", result.Status)
	require.NotNil(t, result.PaymentID)
	stored, err := service.repo.GetByID(fx.ctx, result.ID.UUID())
	require.NoError(t, err)
	require.Equal(t, models.CheckoutSessionStatusSucceeded, stored.Status)
	require.Equal(t, true, stored.RailState["purchase_submitted"])
}

func TestPurchaseNMISessionAdmissionSurvivesArchiveBeforeDispatch(t *testing.T) {
	fx, service, _, _ := purchaseSessionFixture(t)
	_, err := fx.db.Pool().Exec(fx.ctx, `UPDATE billing.products SET entitlements_spec='{"accepted_nmi_post":null}' WHERE id=$1`, fx.productID)
	require.NoError(t, err)
	session := admittedPurchaseFixture(t, fx, service, fx.payload.Instrument.PSPID, "nmi-before-archive", models.RailNMI)
	terms, err := purchaseTerms(session)
	require.NoError(t, err)
	_, err = fx.db.Pool().Exec(fx.ctx, `UPDATE billing.products SET entitlements_spec='{"replaced_nmi_post":null}', archived=true WHERE id=$1`, fx.productID)
	require.NoError(t, err)
	_, err = fx.db.Pool().Exec(fx.ctx, `UPDATE billing.prices SET archived=true WHERE id=$1`, fx.priceID)
	require.NoError(t, err)
	sale := fx.runner.Registry.Lookup(payments.TypeNMISale).(*NMISaleIntentHandler).Sale
	sale.Intents = fx.runner
	sale.PaymentMethodResolver = NewCheckoutPaymentMethodResolver(paymentmethods.NewPaymentMethodService(fx.db), sale.RailPaymentMethodService)
	price, product := terms.catalog(fx.merchantID.UUID())
	req := &CheckoutRequest{PriceID: openrails.PriceID(fx.priceID).String(), PaymentMethodID: openrails.PaymentMethodID(fx.payload.PaymentMethodID).String(), Rail: "nmi", CheckoutSessionID: openrails.CheckoutSessionID(session.ID).String(), acceptedPurchase: terms}
	target := railTarget{PSP: "mobius", Rail: "nmi", Scope: &merchants.PSPScope{ID: fx.payload.Instrument.PSPID}}
	result, err := sale.Process(fx.ctx, req, &UserIdentity{ID: fx.userID}, price, product, "checkout_native_session:"+session.ID.String(), target)
	require.NoError(t, err)
	require.Equal(t, "success", result.Status)
	require.Equal(t, terms.PaymentID, *result.PaymentID)
	require.EqualValues(t, 1, fx.gateway.saleCalls.Load())
	paid, err := fx.purchase.PaymentService.GetByID(fx.ctx, *result.PaymentID)
	require.NoError(t, err)
	require.Contains(t, paid.EntitlementsSpecSnapshot, "accepted_nmi_post")
	require.NotContains(t, paid.EntitlementsSpecSnapshot, "replaced_nmi_post")
}
