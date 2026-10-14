//go:build e2e && integration

package subscriptions_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/failpoint"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/orders/observed"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	riverjobs "github.com/open-rails/openrails/internal/river"
)

// The cranker owns collection; the generic card collector must never admit a
// Solana renewal, including after the ownership migration.
func TestSolanaEngineCollectionUsesOnlyItsCranker(t *testing.T) {
	s := openSolanaShop(t)
	w := s.w
	b, sub := s.subscribed(t)
	got := w.subscription(embedded, sub)
	require.Equal(t, "engine", string(got.CollectionPolicy))
	require.Nil(t, got.PaymentMethodID)
	w.advance(monthHours*time.Hour + time.Hour)
	w.runRenewals()
	var cardCollections int
	require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT count(*) FROM billing.provider_intents WHERE subscription_id=$1 AND intent_type='subscription_collection'`), sub.UUID()).Scan(&cardCollections))
	require.Zero(t, cardCollections)
	require.Equal(t, uint64(solanaPlanAmount), s.fake.Balance(s.treasury, s.mint))
	s.crankPass(t)
	w.until(func() bool { return len(completed(w.payments(embedded, b.id))) == 2 }, "the pull settles a second real payment")
	require.Equal(t, uint64(2*solanaPlanAmount), s.fake.Balance(s.treasury, s.mint))
	require.True(t, b.entitled(s.key))
	var orders, attempts int
	require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT count(*) FROM billing.orders WHERE subscription_id=$1 AND subscription_bill_kind='renewal' AND collection_owner='engine' AND status='complete' AND total=$2`), sub.UUID(), solanaPlanAmount).Scan(&orders))
	require.Equal(t, 1, orders)
	require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT count(*) FROM billing.order_attempts a JOIN billing.orders o ON o.merchant_id=a.merchant_id AND o.id=a.order_id WHERE o.subscription_id=$1 AND o.subscription_bill_kind='renewal' AND a.status='succeeded' AND a.payment_id=o.payment_id AND a.id=o.active_attempt_id`), sub.UUID()).Scan(&attempts))
	require.Equal(t, 1, attempts)
}

// A crash after the actual signed payment, before ledger/access settlement,
// repairs from the accepted episode and its original receipt after a restart.
// Replaying it later keeps the same order, attempt, payment and paid period.
func TestSolanaPullRestartRepairsOneAcceptedEpisode(t *testing.T) {
	s := openSolanaShop(t)
	w := s.w
	b, sub := s.subscribed(t)
	var once sync.Once
	remove := failpoint.Set(func(_ context.Context, site failpoint.Site) error {
		if site.Kind != "solana_pull" || site.Subscription != sub.UUID() || site.Point != failpoint.AfterProvider {
			return nil
		}
		crash := false
		once.Do(func() { crash = true })
		if crash {
			return errors.New("crash after the signed pull landed")
		}
		return nil
	})
	defer remove()
	w.advance(monthHours*time.Hour + time.Hour)
	paidAt := w.clock.Now().UTC()
	job, err := w.jobs.Insert(t.Context(), solanaCrankPass{}, &river.InsertOpts{Queue: openrails.QueueBilling})
	require.NoError(t, err)
	w.waitJob(job.Job.ID)
	require.Equal(t, uint64(2*solanaPlanAmount), s.fake.Balance(s.treasury, s.mint), "the chain payment already happened")
	require.Len(t, completed(w.payments(embedded, b.id)), 1, "no fictional local renewal before the receipt settles")
	var in gen.BillingProviderIntent
	require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT id, merchant_id, psp_id, subscription_id, payload, result_evidence FROM billing.provider_intents WHERE subscription_id=$1 AND intent_type='solana_pull' ORDER BY created_at DESC LIMIT 1`), sub.UUID()).Scan(&in.ID, &in.MerchantID, &in.PspID, &in.SubscriptionID, &in.Payload, &in.ResultEvidence))
	var evidence struct {
		TransactionID string              `json:"transaction_id"`
		Execution     *observed.Execution `json:"execution"`
	}
	require.NoError(t, json.Unmarshal(in.ResultEvidence, &evidence))
	require.NotNil(t, evidence.Execution)
	require.NotEmpty(t, evidence.TransactionID)
	attempt := evidence.Execution.AttemptID
	subBefore := w.subscription(embedded, sub)
	_, err = w.client[embedded].UpdateProduct(t.Context(), subBefore.ProductID, billing.UpdateProductParams{DisplayName: catalog.Value("Renamed after the signed pull")})
	require.NoError(t, err)
	remove()
	w.advance(12 * time.Hour)
	w.restart()
	w.wake()
	w.until(func() bool { return len(completed(w.payments(embedded, b.id))) == 2 }, "restart settles the existing signature")
	paid := w.subscription(embedded, sub)
	require.Equal(t, paidAt, paid.CurrentPeriodStartsAt.UTC())
	require.Equal(t, paidAt.Add(monthHours*time.Hour), paid.CurrentPeriodEndsAt.UTC(), "repair time grants no additional period")
	renewals := completed(w.payments(embedded, b.id))
	var renewal billing.Payment
	for _, p := range renewals {
		if p.TransactionID == evidence.TransactionID {
			renewal = p
		}
	}
	require.EqualValues(t, solanaPlanAmount, renewal.Amount)
	require.Equal(t, "USD", renewal.Currency)
	require.NotNil(t, renewal.OrderID)
	var orderID, paymentID string
	var count int
	require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT order_id::text, payment_id::text FROM billing.order_attempts WHERE id=$1 AND status='succeeded' AND transaction_id=$2 AND created_at=$3`), attempt, evidence.TransactionID, evidence.Execution.SubmittedAt).Scan(&orderID, &paymentID))
	require.Equal(t, renewal.OrderID.UUID().String(), orderID)
	require.Equal(t, renewal.ID.UUID().String(), paymentID)
	var description string
	require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT l.description FROM billing.order_lines l WHERE order_id=$1`), renewal.OrderID.UUID()).Scan(&description))
	require.NotEqual(t, "Renamed after the signed pull", description, "the bill keeps the accepted description")
	d, err := db.NewWithPGXPool(w.pool, w.schema)
	require.NoError(t, err)
	require.NoError(t, d.RunInMerchantScope(t.Context(), billing.MerchantID(in.MerchantID), "replay verified Solana pull", func(ctx context.Context) error {
		life := subscriptions.NewSubscriptionLifecycleService(d, nil, nil, nil, nil, nil, w.clock)
		h := &riverjobs.SolanaPullIntentHandler{Store: intents.NewStore(d), Core: &riverjobs.SolanaCrankWorker{DB: d, Clock: w.clock, Lifecycle: life}, Chain: solanaint.NewRPCClientWithConfig(solanaint.RPCClientConfig{PublicURL: s.fake.URL(), Network: "devnet"})}
		w.advance(time.Hour)
		require.Equal(t, intents.OutcomeSucceeded, h.Verify(ctx, in).Class)
		var payload riverjobs.SolanaPullPayload
		require.NoError(t, json.Unmarshal(in.Payload, &payload))
		payload.Agreement.Amount++
		bad := in
		bad.Payload, err = json.Marshal(payload)
		if err != nil {
			return err
		}
		require.Equal(t, intents.OutcomeAmbiguous, h.Verify(ctx, bad).Class, "the recorded execution cannot settle different financial terms")
		return nil
	}))
	require.Len(t, completed(w.payments(embedded, b.id)), 2)
	require.Equal(t, paidAt.Add(monthHours*time.Hour), w.subscription(embedded, sub).CurrentPeriodEndsAt.UTC())
	require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT count(*) FROM billing.order_attempts a JOIN billing.orders o ON o.merchant_id=a.merchant_id AND o.id=a.order_id WHERE o.subscription_id=$1 AND o.subscription_bill_kind='renewal'`), sub.UUID()).Scan(&count))
	require.Equal(t, 1, count)
	require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT count(*) FROM billing.provider_bills WHERE source_kind='solana_pull' AND source_key=$1`), in.ID.String()).Scan(&count))
	require.Equal(t, 1, count)
}
