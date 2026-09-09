//go:build integration

package riverjobs

import (
	"context"
	"fmt"
	"testing"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/solana/solanasubs"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/riverqueue/river"
)

// smPresubmitCranker simulates the production CrankService presubmit path:
// sign (fixed signature) → presubmit write-ahead → submit (scripted outcome).
// err (when set) fires AFTER presubmit ran — the "crash between submit and
// record" window.
type smPresubmitCranker struct {
	sig   string
	err   error
	calls int
}

func (c *smPresubmitCranker) Crank(ctx context.Context, m merchant.ID, sub *models.SolanaSubscription, amt uint64) (string, error) {
	return c.CrankWithPresubmit(ctx, m, sub, amt, uuid.Nil, nil)
}

func (c *smPresubmitCranker) CrankWithPresubmit(_ context.Context, _ merchant.ID, _ *models.SolanaSubscription, _ uint64, _ uuid.UUID, presubmit func(string) error) (string, error) {
	c.calls++
	if presubmit != nil {
		if perr := presubmit(c.sig); perr != nil {
			return "", perr
		}
	}
	if c.err != nil {
		return "", c.err
	}
	return c.sig, nil
}

// fakeChain scripts the verify leg's GetTransaction read.
type fakeChain struct {
	landed  bool
	readErr error
}

func (f *fakeChain) GetTransaction(_ context.Context, _ solanago.Signature) (*rpc.GetTransactionResult, error) {
	if f.readErr != nil {
		return nil, f.readErr
	}
	if !f.landed {
		return nil, rpc.ErrNotFound
	}
	return &rpc.GetTransactionResult{}, nil
}

type solanaPullFixture struct {
	db      *db.DB
	runner  *intents.Runner
	handler *SolanaPullIntentHandler
	life    *fakeLifecycle
	crank   *smPresubmitCranker
	chain   *fakeChain
	row     *models.SolanaSubscription
	pspID   uuid.UUID
	priceID uuid.UUID
	userID  uuid.UUID
	ctx     context.Context
}

func newSolanaPullFixture(t *testing.T) *solanaPullFixture {
	t.Helper()
	// The fixture stands in for the River worker: it seeds solana_subscriptions
	// and drives the handler directly, so it must supply the app.merchant_id the
	// worker supplies in production. An unpinned handle trips the FORCEd RLS
	// WITH CHECK on solana_subscriptions.
	dbi := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	pool := dbtest.SharedMerchantPool(t, dbtest.TestMerchantID.UUID())
	dbtest.EnsureTestMerchant(context.Background(), t, pool)
	ctx := dbtest.WithTestMerchant(context.Background())

	now := time.Now().UTC().Truncate(time.Second)
	userID := dbtest.EnsureCustomerIDPgx(ctx, t, pool, uuid.NewString())
	productID, priceID, subID := uuid.New(), uuid.New(), uuid.New()
	suffix := uuid.NewString()[:8]
	tenantID := dbtest.TestMerchantID.UUID()
	exec := func(sql string, args ...any) {
		t.Helper()
		_, err := pool.Exec(ctx, sql, args...)
		require.NoError(t, err)
	}
	exec(`INSERT INTO openrails.products (id, key, display_name, merchant_id) VALUES ($1, $2, $2, $3)`,
		productID, "solpull-prod-"+suffix, tenantID)
	exec(`INSERT INTO openrails.prices (id, product_id, amount, currency, access_duration_hours, auto_renew, merchant_id)
	      VALUES ($1, $2, 5000000, 'USD', 720, true, $3)`, priceID, productID, tenantID)
	pspID := dbtest.EnsureTestPSP(ctx, t, pool, tenantID, "solana")
	exec(`INSERT INTO openrails.subscriptions
	        (id, price_id, product_id, status, rail, rail_subscription_id, current_period_starts_at,
	         current_period_ends_at, started_at, customer_id, merchant_id, psp_id)
	      VALUES ($1, $2, $3, 'active', 'solana', $4, $5, $6, $5, $7, $8, $9)`,
		subID, priceID, productID, "subpda-"+suffix, now.Add(-720*time.Hour), now, userID, tenantID, pspID)

	row := &models.SolanaSubscription{
		ID:               uuid.New(),
		MerchantID:       tenantID,
		SubscriptionID:   subID,
		SubscriberWallet: "wallet-" + suffix,
		AuthorityPDA:     "auth-" + suffix,
		SubscriptionPDA:  "subpda-" + suffix,
		PlanPDA:          "plan-" + suffix,
		MerchantAddress:  "merchant-" + suffix,
		Mint:             "mint-" + suffix,
		NextPullAt:       now.Add(-time.Minute),
		Status:           models.SolanaSubscriptionActive,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	require.NoError(t, solanasubs.NewSolanaSubscriptionRepo(dbi).Upsert(ctx, row))
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.rail_intents WHERE subscription_id = $1", subID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.solana_subscriptions WHERE subscription_id = $1", subID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.subscriptions WHERE id = $1", subID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.prices WHERE id = $1", priceID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.products WHERE id = $1", productID)
	})

	life := &fakeLifecycle{}
	crank := &smPresubmitCranker{sig: smValidSig}
	core := &SolanaCrankWorker{
		DB:        dbi,
		Clock:     clockwork.NewFakeClockAt(now),
		Cranker:   crank,
		Lifecycle: life,
		resolvePlanFn: func(_ context.Context, _ *models.SolanaSubscription) (resolvedPlan, error) {
			return resolvedPlan{amountBaseUnits: 5_000_000, periodHours: 720, fiatAmount: 5_000_000, currency: "USD", cycleHours: 720}, nil
		},
	}
	chain := &fakeChain{}
	handler := NewSolanaPullIntentHandler(core, intents.NewStore(dbi), chain)
	// or#865: an unstated mode parks every intent — say "full" (see main_test.go).
	runner := &intents.Runner{Store: intents.NewStore(dbi), Registry: intents.NewRegistry(handler), Config: fullModeConfig()}
	return &solanaPullFixture{
		db: dbi, runner: runner, handler: handler, life: life, crank: crank, chain: chain,
		row: row, pspID: pspID, priceID: priceID, userID: userID, ctx: ctx,
	}
}

func (fx *solanaPullFixture) worker(database *db.DB) *SolanaCrankWorker {
	return &SolanaCrankWorker{
		DB:        database,
		Clock:     clockwork.NewFakeClockAt(fx.row.NextPullAt.Add(time.Minute)),
		Cranker:   fx.crank,
		Lifecycle: fx.life,
		resolvePlanFn: func(_ context.Context, _ *models.SolanaSubscription) (resolvedPlan, error) {
			return resolvedPlan{amountBaseUnits: 5_000_000, periodHours: 720, fiatAmount: 5_000_000, currency: "USD", cycleHours: 720}, nil
		},
	}
}

func (fx *solanaPullFixture) enqueueAndExecute(t *testing.T) (uuid.UUID, string) {
	t.Helper()
	intent, err := fx.runner.EnqueueAndExecute(fx.ctx, intents.EnqueueParams{
		MerchantID:     dbtest.TestMerchantID.UUID(),
		Provider:       string(models.RailSolana),
		PspID:          fx.pspID,
		IntentType:     TypeSolanaPull,
		SubscriptionID: &fx.row.SubscriptionID,
		Payload: SolanaPullPayload{
			SubscriptionPDA: fx.row.SubscriptionPDA,
			RowID:           fx.row.ID,
			NextPullAt:      fx.row.NextPullAt.UTC(),
		},
		IdempotencyKey: SolanaPullIdempotencyKey(fx.row.ID, fx.row.NextPullAt),
		NextAttemptAt:  time.Now().UTC(),
		Origin:         intents.OriginSystem,
		OriginReason:   "test solana pull",
	})
	require.NoError(t, err)
	return intent.ID, intent.Status
}

func (fx *solanaPullFixture) intentStatus(t *testing.T, id uuid.UUID) string {
	t.Helper()
	row, err := intents.NewStore(fx.db).Get(fx.ctx, id)
	require.NoError(t, err)
	return row.Status
}

func (fx *solanaPullFixture) advance(d time.Duration) {
	fx.runner.Clock = clockwork.NewFakeClockAt(time.Now().UTC().Add(d))
}

// smValidSig is a syntactically valid base58 signature (64 one-bytes) so the
// verify leg's SignatureFromBase58 accepts it.
var smValidSig = solanago.SignatureFromBytes(make([]byte, 64)).String()

// Happy path: intent-driven crank pulls once, renews once, advances the row;
// the SolanaCrankWorker re-run enqueues a fresh key only for the NEXT period.
func TestSolanaPullIntent_HappyPath(t *testing.T) {
	fx := newSolanaPullFixture(t)
	id, status := fx.enqueueAndExecute(t)
	require.Equal(t, intents.StatusSucceeded, status)
	require.Equal(t, 1, fx.crank.calls)
	require.Equal(t, 1, fx.life.renewals)

	// Same-period replay: superseded (period advanced), NOT a second pull.
	id2, status2 := fx.enqueueAndExecute(t)
	require.Equal(t, id, id2)
	require.Equal(t, intents.StatusSucceeded, status2, "conflict returns the durable succeeded row")
	require.Equal(t, 1, fx.crank.calls)
}

// The River entry point starts with no merchant on its context. Prove the
// worker itself pins the row's merchant before the FORCE-RLS intent insert;
// module-level tests use an already-pinned fixture and cannot catch that seam.
func TestSolanaCrankWorker_RLSScopeCreatesIntent(t *testing.T) {
	fx := newSolanaPullFixture(t)
	workerDB := dbtest.OpenAppDB(t, dbtest.SharedPostgresDSN(t))
	worker := fx.worker(workerDB)
	handler := NewSolanaPullIntentHandler(worker, intents.NewStore(workerDB), fx.chain)
	worker.Intents = &intents.Runner{
		Store:    intents.NewStore(workerDB),
		Registry: intents.NewRegistry(handler),
		Config:   fullModeConfig(),
	}

	require.NoError(t, worker.Work(context.Background(), &river.Job[SolanaCrankArgs]{}))

	var count int
	err := fx.db.Qx(fx.ctx).QueryRow(fx.ctx,
		`SELECT count(*) FROM openrails.rail_intents WHERE subscription_id = $1`,
		fx.row.SubscriptionID,
	).Scan(&count)
	require.NoError(t, err)
	require.Equal(t, 1, count, "the unprivileged worker must persist exactly one merchant-owned intent")
}

type scopeCheckingFailStore struct {
	*intents.Store
	database    *db.DB
	calls       int
	scopedCalls int
}

func (s *scopeCheckingFailStore) Enqueue(ctx context.Context, _ intents.EnqueueParams) (gen.OpenrailsRailIntent, error) {
	s.calls++
	if err := s.database.AssertMerchantScope(ctx, "test solana crank enqueue"); err != nil {
		return gen.OpenrailsRailIntent{}, err
	}
	s.scopedCalls++
	return gen.OpenrailsRailIntent{}, fmt.Errorf("forced enqueue failure")
}

// A row failure is batch-isolated, but the job must still fail after the pass
// so River retries and worker health cannot report a false success.
func TestSolanaCrankWorker_RowFailureFailsJob(t *testing.T) {
	fx := newSolanaPullFixture(t)
	_ = newSolanaPullFixture(t) // a second due row proves the first failure does not abort the batch
	workerDB := dbtest.OpenAppDB(t, dbtest.SharedPostgresDSN(t))
	store := &scopeCheckingFailStore{Store: intents.NewStore(workerDB), database: workerDB}
	worker := fx.worker(workerDB)
	worker.Intents = &intents.Runner{Store: store}

	err := worker.Work(context.Background(), &river.Job[SolanaCrankArgs]{})
	require.ErrorContains(t, err, "pull intents failed")
	require.GreaterOrEqual(t, store.calls, 2)
	require.Equal(t, store.calls, store.scopedCalls, "every attempted row must run on an asserted merchant scope")
}

// Reconciliation also starts unscoped. A recorded pull must find its payment,
// while the same row without that payment must create exactly one repair alert
// inside the row's merchant scope.
func TestSolanaReconcileWorker_RLSScopeFindsRecordedPayment(t *testing.T) {
	fx := newSolanaPullFixture(t)
	sig := "reconcile-" + uuid.NewString()
	_, err := fx.db.Qx(fx.ctx).Exec(fx.ctx,
		`UPDATE openrails.solana_subscriptions SET last_signature = $1 WHERE id = $2`,
		sig, fx.row.ID,
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = fx.db.Qx(fx.ctx).Exec(fx.ctx,
			`DELETE FROM openrails.notification_queue WHERE data->>'transaction_id' = $1`, sig)
	})

	subID := fx.row.SubscriptionID
	paymentID := uuid.New()
	require.NoError(t, payments.NewPaymentRepo(fx.db).Create(fx.ctx, &models.Payment{
		ID: paymentID, CustomerID: fx.userID, PriceID: fx.priceID, SubscriptionID: &subID,
		Rail: models.RailSolana, TransactionID: sig, Amount: 5_000_000, ListAmount: 5_000_000,
		Currency: "USD", Status: "completed", PspID: &fx.pspID, MoneyMovement: models.MoneyMovementRail,
		PurchasedAt: time.Now().UTC(), CreatedAt: time.Now().UTC(),
	}))

	workerDB := dbtest.OpenAppDB(t, dbtest.SharedPostgresDSN(t))
	worker := &SolanaReconcileWorker{DB: workerDB, Clock: clockwork.NewFakeClockAt(time.Now().UTC())}
	job := &river.Job[SolanaReconcileArgs]{}
	require.NoError(t, worker.Work(context.Background(), job))
	require.Equal(t, 0, repairAlertCount(t, fx, sig), "a recorded pull must not raise a false alert")

	require.NoError(t, payments.NewPaymentRepo(fx.db).Delete(fx.ctx, paymentID))
	require.NoError(t, worker.Work(context.Background(), job))
	require.Equal(t, 1, repairAlertCount(t, fx, sig), "a genuinely missing payment must raise one scoped alert")
}

func repairAlertCount(t *testing.T, fx *solanaPullFixture, signature string) int {
	t.Helper()
	var count int
	err := fx.db.Qx(fx.ctx).QueryRow(fx.ctx, `
		SELECT count(*)
		  FROM openrails.notification_queue
		 WHERE event_type = 'system_alert'
		   AND data->>'operation' = 'solana_crank_unrecorded_pull'
		   AND data->>'transaction_id' = $1`, signature,
	).Scan(&count)
	require.NoError(t, err)
	return count
}

// THE #674 solana case: crash between submit and record. The signature was
// durably written ahead of the submit; the intent parks ambiguous; the
// verifier confirms the tx landed and runs the renewal repair — the subscriber
// who paid on-chain GETS the renewal, with zero re-pulls.
func TestSolanaPullIntent_CrashAfterSubmit_SignatureRepairsRenewal(t *testing.T) {
	fx := newSolanaPullFixture(t)
	// Operational transport error (classified so by ClassifyCrankError).
	fx.crank.err = fmt.Errorf("solana: submit/confirm transaction: %w", context.DeadlineExceeded)

	id, status := fx.enqueueAndExecute(t)
	require.Equal(t, intents.StatusUnknownNeedsVerify, status,
		"submit failure after the signature write-ahead must verify, never blind-retry")
	require.Equal(t, 0, fx.life.renewals, "no renewal yet")

	// The tx actually landed on-chain.
	fx.chain.landed = true
	fx.advance(2 * time.Minute)
	_, err := fx.runner.RunVerifyOnce(fx.ctx)
	require.NoError(t, err)
	require.Equal(t, intents.StatusSucceeded, fx.intentStatus(t, id))
	require.Equal(t, 1, fx.life.renewals, "renewal repair ran off the recorded signature")
	require.Equal(t, 1, fx.crank.calls, "never re-pulled")

	// Row advanced past the period.
	row, err := solanasubs.NewSolanaSubscriptionRepo(fx.db).GetBySubscriptionPDA(fx.ctx, fx.row.SubscriptionPDA)
	require.NoError(t, err)
	require.True(t, row.NextPullAt.After(fx.row.NextPullAt), "AdvanceAfterPull applied")
	require.NotNil(t, row.LastSignature)
	require.Equal(t, smValidSig, *row.LastSignature)
}

// Verified NOT landed: the executor re-cranks under the intent (the on-chain
// period guard backstops), exactly one renewal.
func TestSolanaPullIntent_NotLandedRetriesThenLands(t *testing.T) {
	fx := newSolanaPullFixture(t)
	fx.crank.err = fmt.Errorf("solana: submit/confirm transaction: %w", context.DeadlineExceeded)

	id, status := fx.enqueueAndExecute(t)
	require.Equal(t, intents.StatusUnknownNeedsVerify, status)

	// Chain says: never landed (blockhash expired). Verify → retryable.
	fx.chain.landed = false
	fx.advance(2 * time.Minute)
	_, err := fx.runner.RunVerifyOnce(fx.ctx)
	require.NoError(t, err)
	require.Equal(t, intents.StatusFailedRetryable, fx.intentStatus(t, id))

	// Cranker healthy again; the executor re-pulls THIS intent.
	fx.crank.err = nil
	fx.advance(10 * time.Minute)
	_, err = fx.runner.RunExecuteOnce(fx.ctx)
	require.NoError(t, err)
	require.Equal(t, intents.StatusSucceeded, fx.intentStatus(t, id))
	require.Equal(t, 2, fx.crank.calls)
	require.Equal(t, 1, fx.life.renewals)
}
