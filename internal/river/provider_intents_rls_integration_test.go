//go:build integration

package riverjobs

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/destructive"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/pkg/merchant"
)

// or#862: the provider-intent executor ran RunExecuteOnce on the BARE River job
// context. rail_intents FORCEs RLS, so under the production openrails_app role
// the claim's predicate was `merchant_id = NULL` — it leased ZERO intents,
// silently and with no error. The whole outbound provider-mutation plane was
// inert, and because the #836 kill switch and the #679 volume breaker only run
// on a CLAIMED intent, both were disarmed with it.
//
// Every assertion here runs on the RLS-enforcing default handle, which is the
// only reason the bug is visible at all: on a superuser connection the old code
// claims fine and every one of these subtests passes against it.
func TestProviderIntentExecutorFansOutPerMerchant(t *testing.T) {
	ctx := context.Background()
	worker := dbtest.OpenAppDB(t, dbtest.SharedPostgresDSN(t)) // UNPINNED, like a real River job
	m := seedIntentMerchant(t)
	seedDueIntent(t, m, intents.TypeNMIDeleteSubscription)

	// The #836 switch is INSTANCE-wide state; restore it or the rest of the
	// package inherits whatever this test left behind.
	restoreDestructiveSwitch(t, worker)

	t.Run("a claim requires an explicit merchant scope", func(t *testing.T) {
		now := time.Now().UTC()
		_, err := intents.NewStore(worker).ClaimDue(ctx, now, now.Add(time.Minute), 50)
		require.ErrorIs(t, err, merchant.ErrNoMerchant)

		// And the merchant IS there to be found, through the sanctioned path.
		ids, err := intents.NewStore(worker).DueExecuteMerchants(ctx, now, 500)
		require.NoError(t, err)
		require.Contains(t, ids, m.id, "0022's work queue must surface the merchant the bare claim could not see")
	})

	t.Run("the executor claims and executes under the merchant's own scope", func(t *testing.T) {
		// #836 default is OFF (a fresh deployment cancels nothing); arm it so
		// this leg measures the claim, not the switch.
		require.NoError(t, destructive.New(worker).SetSwitch(ctx, true, "or862-test", "arm for the execute leg"))
		id := seedDueIntent(t, m, intents.TypeNMIDeleteSubscription)
		h := &recordingIntentHandler{intentType: intents.TypeNMIDeleteSubscription}
		require.NoError(t, ProviderIntentExecuteWorker{
			DB:       worker,
			Config:   &config.Config{ProviderWriteMode: config.ProviderWriteModeFull},
			Registry: intents.NewRegistry(h),
		}.Work(ctx, nil))

		require.NotZero(t, h.executed, "the intent must actually reach its handler")
		require.Equal(t, m.id, h.sawMerchant, "the handler must run under the intent's merchant")
		require.Equal(t, intents.StatusSucceeded, m.statusOf(t, id))
	})

	t.Run("#836 kill switch OFF actually stops the destructive write", func(t *testing.T) {
		gate := destructive.New(worker)
		require.NoError(t, gate.SetSwitch(ctx, false, "or862-test", "prove the switch reaches the background plane"))

		id := seedDueIntent(t, m, intents.TypeNMIDeleteSubscription)
		h := &recordingIntentHandler{intentType: intents.TypeNMIDeleteSubscription}
		require.NoError(t, ProviderIntentExecuteWorker{
			DB:       worker,
			Config:   &config.Config{ProviderWriteMode: config.ProviderWriteModeFull},
			Registry: intents.NewRegistry(h),
		}.Work(ctx, nil))

		require.Zero(t, h.executed, "the kill switch must stop the provider write BEFORE the handler")
		require.Equal(t, intents.StatusPending, m.statusOf(t, id),
			"a switched-off destructive intent parks (stays pending), it does not fail")
	})
}

// TestProviderIntentVerifierFansOutPerMerchant is the verifier half of or#862:
// an ambiguous outcome that is never verified is never retried, so a blind
// verifier plane strands every unknown intent forever.
func TestProviderIntentVerifierFansOutPerMerchant(t *testing.T) {
	ctx := context.Background()
	worker := dbtest.OpenAppDB(t, dbtest.SharedPostgresDSN(t))
	m := seedIntentMerchant(t)
	id := seedDueIntent(t, m, intents.TypeNMIDeleteSubscription)
	m.exec(t, `UPDATE billing.rail_intents SET status = 'unknown_needs_verify' WHERE id = $1`, id)

	now := time.Now().UTC()
	_, err := intents.NewStore(worker).ClaimDueVerify(ctx, now, now.Add(time.Minute), 50)
	require.ErrorIs(t, err, merchant.ErrNoMerchant)

	h := &recordingIntentHandler{intentType: intents.TypeNMIDeleteSubscription}
	require.NoError(t, ProviderIntentVerifyWorker{DB: worker, Registry: intents.NewRegistry(h)}.Work(ctx, nil))
	require.Equal(t, 1, h.verified, "the verifier must reach the handler's read-only Verify")
	require.Equal(t, intents.StatusSucceeded, m.statusOf(t, id))
}

// TestVolumeBreakerRefusesToReadZeros is or#862's second leg: even had the
// breaker been reached, `b.db.Gen(ctx)` on an unpinned context counted 0
// executions against 0 active subscriptions, giving budget = max(25, 1%×0) = 25
// vs executed = 0 — a breaker that can never hold. It now refuses to answer at
// all off a merchant-pinned connection, and Check's contract is fail-closed, so
// the executor parks rather than executing unexamined.
func TestVolumeBreakerRefusesToReadZeros(t *testing.T) {
	ctx := context.Background()
	unpinned := dbtest.OpenAppDB(t, dbtest.SharedPostgresDSN(t))
	m := seedIntentMerchant(t)
	id := seedDueIntent(t, m, intents.TypeNMIDeleteSubscription)

	row, err := dbtest.Queries(m.pool).GetRailIntent(ctx, gen.GetRailIntentParams{MerchantID: m.id, ID: id})
	require.NoError(t, err)

	// The runner pins the intent's merchant as a context VALUE before calling
	// the breaker — and a value is exactly what does NOT scope a database.
	mctx := merchant.WithID(ctx, merchant.ID(m.id))
	_, _, err = intents.NewVolumeBreaker(unpinned).Check(mctx, row, time.Now().UTC())
	require.Error(t, err, "an unscoped breaker check must fail loudly, not report a comfortable zero")

	var unscoped *db.ErrUnscopedMerchantWork
	require.ErrorAs(t, err, &unscoped)
}

// --- fixtures ---------------------------------------------------------------

type intentMerchant struct {
	id   uuid.UUID
	pool *pgxpool.Pool
}

func seedIntentMerchant(t *testing.T) intentMerchant {
	t.Helper()
	m := intentMerchant{id: uuid.New()}
	m.pool = dbtest.SharedMerchantPool(t, m.id)
	m.exec(t, `INSERT INTO billing.merchants (id, slug, status) VALUES ($1, $2, 'active')`,
		m.id, "or862-"+uuid.NewString()[:8])
	// Self-cleaning: an ACTIVE merchant left behind is not inert. Every
	// deployment-wide fan-out (ListActiveMerchantIDs — the worker-health repair
	// alert, the converge sweep) then walks this merchant too, in id order, and
	// the rest of the package inherits whatever that does.
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = m.pool.Exec(ctx, `DELETE FROM billing.rail_intents WHERE merchant_id = $1`, m.id)
		_, _ = m.pool.Exec(ctx, `DELETE FROM billing.customers WHERE merchant_id = $1`, m.id)
		_, _ = m.pool.Exec(ctx, `DELETE FROM billing.merchants WHERE id = $1`, m.id)
	})
	return m
}

// restoreDestructiveSwitch snapshots the instance kill switch and puts it back
// when the test ends.
func restoreDestructiveSwitch(t *testing.T, dbi *db.DB) {
	t.Helper()
	ctx := context.Background()
	before, err := dbi.Gen(ctx).IsDestructiveActionSwitchEnabled(ctx)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, destructive.New(dbi).SetSwitch(
			context.Background(), before, "or862-test", "restore instance state"))
	})
}

func (m intentMerchant) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	_, err := m.pool.Exec(context.Background(), sql, args...)
	require.NoError(t, err)
}

func (m intentMerchant) statusOf(t *testing.T, id uuid.UUID) string {
	t.Helper()
	var status string
	require.NoError(t, m.pool.QueryRow(context.Background(),
		`SELECT status FROM billing.rail_intents WHERE id = $1`, id).Scan(&status))
	return status
}

// seedDueIntent inserts one system-origin, immediately-due intent. Seeded on a
// merchant-pinned pool: the fixture must satisfy the same RLS the product does.
func seedDueIntent(t *testing.T, m intentMerchant, intentType string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	pspID := dbtest.EnsureTestPSP(context.Background(), t, m.pool, m.id, "nmi")
	m.exec(t, `INSERT INTO billing.rail_intents
	             (id, merchant_id, rail, psp_id, intent_type, idempotency_key, status,
	              next_attempt_at, origin, payload)
	           VALUES ($1, $2, 'nmi', $3, $4, $5, 'pending', now() - interval '1 minute', 'system', '{}'::jsonb)`,
		id, m.id, pspID, intentType, "or862-"+uuid.NewString())
	return id
}

// recordingIntentHandler is a handler that records what the runner did to it.
// Deliberately trivial: this test is about whether the plane REACHES a handler
// at all, not about any rail's semantics.
type recordingIntentHandler struct {
	intentType  string
	executed    int
	verified    int
	sawMerchant uuid.UUID
}

func (h *recordingIntentHandler) Type() string { return h.intentType }

func (h *recordingIntentHandler) CheckRelevance(context.Context, gen.OpenrailsRailIntent) (intents.Relevance, error) {
	return intents.Relevance{Applicable: true}, nil
}

func (h *recordingIntentHandler) Execute(_ context.Context, intent gen.OpenrailsRailIntent) intents.Outcome {
	h.executed++
	h.sawMerchant = intent.MerchantID
	return intents.Outcome{Class: intents.OutcomeSucceeded}
}

func (h *recordingIntentHandler) Verify(_ context.Context, intent gen.OpenrailsRailIntent) intents.Outcome {
	h.verified++
	h.sawMerchant = intent.MerchantID
	return intents.Outcome{Class: intents.OutcomeSucceeded}
}

func (h *recordingIntentHandler) Backoff(int32) time.Duration { return time.Minute }
