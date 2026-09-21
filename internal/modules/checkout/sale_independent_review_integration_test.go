//go:build integration

package checkout

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/productaccess"
	"github.com/stretchr/testify/require"
)

func TestIndependentSaleWebhookFirstKeepsAcceptedAccessWindow(t *testing.T) {
	for _, contradictory := range []bool{false, true} {
		name := "matching_original_window"
		if contradictory {
			name = "contradictory_original_window"
		}
		t.Run(name, func(t *testing.T) {
			fx := newSaleIntentFixture(t)
			now := time.Now().UTC().Truncate(time.Second)
			accepted := now.Add(-48 * time.Hour)
			hours := 24
			end := accepted.Add(24 * time.Hour)
			fx.payload.AcceptedAt, fx.payload.EntitlementStart, fx.payload.OwnershipStart = accepted, accepted, accepted
			fx.payload.OwnershipEnd, fx.payload.AccessDurationHours = &end, &hours
			fx.payload.Entitlements = map[string]*int{"sale_review_feature": nil}
			_, err := fx.db.Pool().Exec(fx.ctx, `UPDATE billing.prices SET access_duration_hours=24 WHERE id=$1;`, fx.priceID)
			require.NoError(t, err)
			_, err = fx.db.Pool().Exec(fx.ctx, `UPDATE billing.products SET entitlements_spec='{"sale_review_feature":null}'::jsonb WHERE id=$1`, fx.productID)
			require.NoError(t, err)
			fx.gateway.saleMode.Store("ambiguous500")
			row := fx.enqueueAndExecute(t, uuid.NewString())
			require.Equal(t, intents.StatusUnknownNeedsVerify, row.Status)
			require.Zero(t, fx.paymentCount(t))
			// The ordinary observed-payment writer sees the same approved transaction
			// before autonomous intent verification. Its present clock is later.
			observedAt := accepted
			if contradictory {
				observedAt = now
			}
			fx.purchase.clock = clockwork.NewFakeClockAt(observedAt)
			fx.purchase.SetProductAccessService(productaccess.NewService(fx.db, fx.purchase.clock))
			ctx := db.WithPSPID(fx.ctx, *row.PspID)
			observed, err := fx.purchase.RegisterPurchase(ctx, &payments.RegisterPurchaseRequest{
				UserID: fx.userID, PriceID: fx.priceID, Rail: "nmi", TransactionID: fx.gateway.txnID,
				Amount: 5_000_000, AmountProvided: true, Currency: "USD", PurchasedAt: &accepted,
			})
			require.NoError(t, err)
			require.NotEqual(t, uuid.Nil, observed.PaymentID)
			outcome := fx.runner.Registry.Lookup(payments.TypeNMISale).Verify(ctx, row)
			if contradictory {
				require.Equal(t, intents.OutcomeAmbiguous, outcome.Class)
			} else {
				require.Equal(t, intents.OutcomeSucceeded, outcome.Class, outcome.Reason)
			}
			current, err := intents.NewStore(fx.db).Get(ctx, row.ID)
			require.NoError(t, err)
			if contradictory {
				require.Equal(t, intents.StatusUnknownNeedsVerify, current.Status)
			} else {
				require.Equal(t, intents.StatusSucceeded, current.Status)
			}
			var starts, ends time.Time
			require.NoError(t, fx.db.Pool().QueryRow(fx.ctx, `SELECT starts_at,ends_at FROM billing.grants WHERE customer_id=$1 AND product_id=$2 AND kind='ownership' AND event='grant'`, fx.customerID, fx.productID).Scan(&starts, &ends))
			require.True(t, observedAt.Equal(starts), "recovery never rewrites an original observed grant")
			require.True(t, observedAt.Add(24*time.Hour).Equal(ends))
			require.Equal(t, 1, fx.paymentCount(t))
			var events int
			require.NoError(t, fx.db.Pool().QueryRow(ctx, `SELECT count(*) FROM billing.host_outbox WHERE payment_id=$1`, observed.PaymentID).Scan(&events))
			require.Equal(t, 1, events)

		})
	}
}

func TestIndependentSaleSameKeyWaiterReplaysBeforeEligibility(t *testing.T) {
	fx := newSaleIntentFixture(t)
	_, err := fx.db.Pool().Exec(fx.ctx, `UPDATE billing.products SET entitlements_spec='{"sale_review_permanent":null}'::jsonb WHERE id=$1`, fx.productID)
	require.NoError(t, err)
	sale := fx.runner.Registry.Lookup(payments.TypeNMISale).(*NMISaleIntentHandler).Sale
	sale.Intents = fx.runner
	sale.PaymentMethodResolver = NewCheckoutPaymentMethodResolver(paymentmethods.NewPaymentMethodService(fx.db), sale.RailPaymentMethodService)
	oldResolve := sale.ResolveNMIClient
	type pauseKey struct{}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	sale.ResolveNMIClient = func(ctx context.Context, key string) (*nmi.NMIClient, error) {
		if ctx.Value(pauseKey{}) == true {
			once.Do(func() { close(entered); <-release })
		}
		return oldResolve(ctx, key)
	}
	price, err := fx.purchase.PriceService.GetByID(fx.ctx, fx.priceID)
	require.NoError(t, err)
	product, err := fx.purchase.ProductService.GetByID(fx.ctx, fx.productID)
	require.NoError(t, err)
	req := &CheckoutRequest{PriceID: openrails.PriceID(fx.priceID).String(), PaymentMethodID: openrails.PaymentMethodID(fx.payload.PaymentMethodID).String(), Rail: "nmi"}
	user := &UserIdentity{ID: fx.userID}
	target := railTarget{PSP: "mobius", Rail: "nmi", Scope: &merchants.PSPScope{ID: fx.payload.Instrument.PSPID}}
	key := uuid.NewString()
	type result struct {
		response *CheckoutResponse
		err      error
	}
	done := make(chan result, 1)
	go func() {
		response, err := sale.Process(context.WithValue(fx.ctx, pauseKey{}, true), req, user, price, product, key, target)
		done <- result{response, err}
	}()
	<-entered // B has missed canonical lookup and is inside preflight.
	winner, winnerErr := sale.Process(fx.ctx, req, user, price, product, key, target)
	close(release)
	waiter := <-done
	require.NoError(t, winnerErr)
	require.Equal(t, "success", winner.Status)
	require.NoError(t, waiter.err, "same-key waiter must replay accepted winner before current eligibility")
	require.Equal(t, winner.PaymentID, waiter.response.PaymentID)
	require.EqualValues(t, 1, fx.gateway.saleCalls.Load())
}

func TestIndependentSalePublicCheckoutReplayPrecedesCurrentCoverage(t *testing.T) {
	fx := newSaleIntentFixture(t)
	_, err := fx.db.Pool().Exec(fx.ctx, `UPDATE billing.products SET entitlements_spec='{"sale_review_public":null}'::jsonb WHERE id=$1`, fx.productID)
	require.NoError(t, err)
	sale := fx.runner.Registry.Lookup(payments.TypeNMISale).(*NMISaleIntentHandler).Sale
	sale.Intents = fx.runner
	sale.PaymentMethodResolver = NewCheckoutPaymentMethodResolver(paymentmethods.NewPaymentMethodService(fx.db), sale.RailPaymentMethodService)
	price, err := fx.purchase.PriceService.GetByID(fx.ctx, fx.priceID)
	require.NoError(t, err)
	product, err := fx.purchase.ProductService.GetByID(fx.ctx, fx.productID)
	require.NoError(t, err)
	req := &CheckoutRequest{PriceID: openrails.PriceID(fx.priceID).String(), PaymentMethodID: openrails.PaymentMethodID(fx.payload.PaymentMethodID).String(), Rail: "nmi", IdempotencyKey: uuid.NewString()}
	user := &UserIdentity{ID: fx.userID}
	target := railTarget{PSP: "mobius", Rail: "nmi", Scope: &merchants.PSPScope{ID: fx.payload.Instrument.PSPID}}
	first, err := sale.Process(fx.ctx, req, user, price, product, req.IdempotencyKey, target)
	require.NoError(t, err)
	require.Equal(t, "success", first.Status)
	checkout := &CheckoutService{PriceService: fx.purchase.PriceService, ProductService: fx.purchase.ProductService, PurchaseService: fx.purchase, NMISaleService: sale}
	replay, err := checkout.Checkout(fx.ctx, req, user)
	require.NoError(t, err)
	require.Equal(t, "success", replay.Status, "real checkout entrypoint must replay canonical purchase before refusing current access")
	require.Equal(t, first.PaymentID, replay.PaymentID)
	require.EqualValues(t, 1, fx.gateway.saleCalls.Load())
}
