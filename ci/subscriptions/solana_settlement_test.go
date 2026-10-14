//go:build e2e && integration

package subscriptions_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/failpoint"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	solanasubs "github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/orders/observed"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	riverjobs "github.com/open-rails/openrails/internal/river"
)

func TestSolanaEnrollmentRequiresItsExactPaidOrder(t *testing.T) {
	s := openSolanaShop(t)
	w := s.w
	b, sub := s.subscribed(t)
	other := s.buyer(t, false)
	unpaid := s.order(t, other)
	d, err := db.NewWithPGXPool(w.pool, w.schema)
	require.NoError(t, err)
	require.NoError(t, d.RunInMerchantScope(t.Context(), w.client[embedded].MerchantID(), "verify wallet enrollment first payment", func(ctx context.Context) error {
		got, err := subscriptions.NewSubscriptionRepo(d).GetByID(ctx, sub.UUID())
		if err != nil {
			return err
		}
		terms := subscriptions.InitialMembershipTerms{Rail: models.RailSolana, CollectionPolicy: models.CollectionPolicyEngine,
			SubscriptionID: uuid.New(), CustomerID: uuid.MustParse(other.id), PSPID: got.PspID, ProductID: got.ProductID, PriceID: got.PriceID,
			RecurringAmount: solanaPlanAmount, Currency: "USD", RecurringIntervalHours: new(monthHours), AccessDurationHours: got.AccessDurationHoursSnapshot,
			AcceptedAt: *got.CurrentPeriodStartsAt, PeriodStart: *got.CurrentPeriodStartsAt, PeriodEnd: *got.CurrentPeriodEndsAt}
		for _, order := range []*uuid.UUID{nil, new(uuid.New()), new(unpaid.UUID())} {
			providerRef := solanago.NewWallet().PublicKey().String()
			err := d.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
				life := subscriptions.NewSubscriptionLifecycleService(d, nil, nil, nil, nil, nil, w.clock)
				_, _, err := life.CreateMembershipTx(db.WithPSPID(ctx, got.PspID), d.NewWithPgxTx(tx), &subscriptions.CreateMembershipParams{
					Prepared: &terms, ExistingOrderID: order, UserID: other.id, PriceID: got.PriceID, Rail: models.RailSolana,
					RailSubscriptionID: &providerRef})
				return err
			})
			require.Error(t, err, "missing, nonexistent and unpaid orders cannot authorize a wallet membership")
		}
		return nil
	}))
	require.Len(t, completed(w.payments(embedded, b.id)), 1)
	require.Empty(t, completed(w.payments(embedded, other.id)))
}

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
	require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT count(*) FROM billing.order_attempts a JOIN billing.orders o ON o.merchant_id=a.merchant_id AND o.id=a.order_id WHERE o.subscription_id=$1 AND o.subscription_bill_kind='renewal' AND a.status='succeeded' AND a.payment_id=o.payment_id AND a.id=o.attempt_id`), sub.UUID()).Scan(&attempts))
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

// Another signed pull outside this book's accepted episode is evidence for
// review, not permission to invent a bill or move the local paid boundary.
func TestSolanaAlreadyPaidWithoutAcceptedReceiptKeepsUnknown(t *testing.T) {
	s := openSolanaShop(t)
	w := s.w
	b, sub := s.subscribed(t)
	oldEnd := *w.subscription(embedded, sub).CurrentPeriodEndsAt
	w.advance(monthHours*time.Hour + time.Hour)
	plan, account := s.onChain(t, sub)
	authority, _, err := solanasubs.DeriveSubscriptionAuthority(b.wallet.PublicKey(), s.mint)
	require.NoError(t, err)
	from, _, err := solanasubs.DeriveATA(b.wallet.PublicKey(), s.mint, solanago.TokenProgramID)
	require.NoError(t, err)
	to, _, err := solanasubs.DeriveATA(s.treasury, s.mint, solanago.TokenProgramID)
	require.NoError(t, err)
	events, _, err := solanasubs.DeriveEventAuthority()
	require.NoError(t, err)
	outOfBand := solanasubs.BuildTransferSubscription(solanasubs.TransferSubscriptionParams{SubscriptionPDA: account, PlanPDA: plan, SubscriptionAuthority: authority,
		DelegatorATA: from, ReceiverATA: to, Caller: s.cranker.PublicKey(), Mint: s.mint, TokenProgram: solanago.TokenProgramID, EventAuthority: events,
		Amount: solanaPlanAmount, Delegator: b.wallet.PublicKey()})
	s.land(t, build(t, []solanago.Instruction{solanaint.NewMemoInstruction(solanaint.PurchaseMemo(uuid.New())), outOfBand}, s.cranker), w.clock.Now())
	require.Equal(t, uint64(2*solanaPlanAmount), s.fake.Balance(s.treasury, s.mint))
	s.crankPass(t)
	w.until(func() bool { return string(w.subscription(embedded, sub).Status) == "unverified" }, "unattributed paid state waits for receipt qualification")
	require.Len(t, completed(w.payments(embedded, b.id)), 1, "no guessed second cash payment")
	require.Equal(t, oldEnd, *w.subscription(embedded, sub).CurrentPeriodEndsAt)
	var orders int
	require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT count(*) FROM billing.orders WHERE subscription_id=$1 AND subscription_bill_kind='renewal'`), sub.UUID()).Scan(&orders))
	require.Zero(t, orders, "no invented renewal order")
	require.True(t, b.entitled(s.key), "uncertainty keeps existing access")
	require.Len(t, w.openFindings("consistency.ledger.unbooked"), 1)
	d, err := db.NewWithPGXPool(w.pool, w.schema)
	require.NoError(t, err)
	require.NoError(t, d.RunInMerchantScope(t.Context(), w.client[embedded].MerchantID(), "verify rejected duplicate pull", func(ctx context.Context) error {
		var id uuid.UUID
		if err := w.pool.QueryRow(ctx, w.q(`SELECT id FROM billing.provider_intents WHERE subscription_id=$1 AND intent_type='solana_pull' ORDER BY created_at DESC LIMIT 1`), sub.UUID()).Scan(&id); err != nil {
			return err
		}
		in, err := d.Gen(ctx).GetProviderIntent(ctx, gen.GetProviderIntentParams{MerchantID: w.client[embedded].MerchantID().UUID(), ID: id})
		if err != nil {
			return err
		}
		h := &riverjobs.SolanaPullIntentHandler{Store: intents.NewStore(d), Core: &riverjobs.SolanaCrankWorker{DB: d, Clock: w.clock}, Chain: solanaint.NewRPCClientWithConfig(solanaint.RPCClientConfig{PublicURL: s.fake.URL(), Network: "devnet"})}
		require.Equal(t, intents.OutcomeRetryable, h.Verify(ctx, in).Class, "the exact rejected duplicate really reverted")
		return nil
	}))
	status, body := b.cancel(sub, map[string]any{"reason": "stop renewing"})
	require.Equal(t, 409, status, "%v", body)
	require.Equal(t, "payment_in_progress", body["error"].(map[string]any)["code"], "our reverted duplicate cannot qualify the other paid transaction")
}

// Operational status never proves whether a signed transaction moved money.
// Neither legacy missing metadata nor a parked/terminal status releases a
// cancel while the original signed receipt still awaits qualification.
func TestSolanaCancelKeepsEverySignedUnresolvedStatus(t *testing.T) {
	s := openSolanaShop(t)
	w := s.w
	b, sub := s.subscribed(t)
	remove := failpoint.Set(func(_ context.Context, site failpoint.Site) error {
		if site.Kind == "solana_pull" && site.Subscription == sub.UUID() && site.Point == failpoint.AfterProvider {
			return errors.New("hold receipt qualification")
		}
		return nil
	})
	defer remove()
	w.advance(monthHours*time.Hour + time.Hour)
	job, err := w.jobs.Insert(t.Context(), solanaCrankPass{}, &river.InsertOpts{Queue: openrails.QueueBilling})
	require.NoError(t, err)
	w.waitJob(job.Job.ID)
	var id string
	var payload, evidence []byte
	require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT id::text,payload,result_evidence FROM billing.provider_intents WHERE subscription_id=$1 AND intent_type='solana_pull' ORDER BY created_at DESC LIMIT 1`), sub.UUID()).Scan(&id, &payload, &evidence))
	var ev map[string]any
	require.NoError(t, json.Unmarshal(evidence, &ev))
	s.fake.Forget(ev["transaction_id"].(string), 10000)
	for _, shape := range []string{"accepted", "legacy_missing_agreement", "missing_execution", "mismatched_execution"} {
		var p, e map[string]any
		require.NoError(t, json.Unmarshal(payload, &p))
		require.NoError(t, json.Unmarshal(evidence, &e))
		switch shape {
		case "legacy_missing_agreement":
			delete(p, "agreement")
		case "missing_execution":
			delete(e, "execution")
		case "mismatched_execution":
			e["execution"].(map[string]any)["signature"] = "different-receipt"
		}
		praw, err := json.Marshal(p)
		require.NoError(t, err)
		eraw, err := json.Marshal(e)
		require.NoError(t, err)
		for _, status := range []string{"pending", "in_flight", "unknown_needs_verify", "failed_retryable", "failed_terminal", "superseded", "expired", "succeeded"} {
			_, err = w.pool.Exec(t.Context(), w.q(`UPDATE billing.provider_intents SET status=$2,payload=$3,result_evidence=$4,executed_at=CASE WHEN $2='succeeded' THEN $5::timestamptz ELSE NULL END WHERE id=$1`), id, status, praw, eraw, w.clock.Now())
			require.NoError(t, err)
			code, body := b.cancel(sub, map[string]any{"reason": "stop renewing"})
			require.Equal(t, 409, code, "%s/%s: %v", shape, status, body)
			require.Equal(t, "payment_in_progress", body["error"].(map[string]any)["code"])
		}
	}
	require.Len(t, completed(w.payments(embedded, b.id)), 1)
	require.Equal(t, uint64(2*solanaPlanAmount), s.fake.Balance(s.treasury, s.mint), "the held chain payment is never charged again")
}
