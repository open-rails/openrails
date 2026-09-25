//go:build greenfield && integration

package idempotency_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jonboulle/clockwork"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/idempotency"
	"github.com/open-rails/openrails/internal/modules/webhooks"
	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/open-rails/openrails/pkg/merchant"
)

const (
	ttl   = time.Hour
	lease = 20 * time.Second
)

// env is one schema shared by two replicas, each with its own connection
// pool, over one engine clock.
type env struct {
	t        *testing.T
	admin    *pgxpool.Pool
	schema   string
	clock    *clockwork.FakeClock
	replicas [2]*db.DB
	merchant uuid.UUID
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("OPENRAILS_GREENFIELD_DSN"))
	if dsn == "" {
		t.Fatal("OPENRAILS_GREENFIELD_DSN must point at a disposable PostgreSQL database")
	}
	admin, err := pgxpool.New(t.Context(), dsn)
	require.NoError(t, err)
	e := &env{t: t, admin: admin, schema: "gf_idem_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16],
		clock: clockwork.NewFakeClockAt(time.Now().UTC().Truncate(time.Second))}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = admin.Exec(ctx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{e.schema}.Sanitize()+" CASCADE")
		admin.Close()
	})
	require.NoError(t, embed.ApplyMigrations(t.Context(), admin, embed.MigrationOptions{Schema: e.schema, River: embed.RiverFromHost(), RuntimePool: admin}))
	e.merchant = e.newMerchant()
	for i := range e.replicas {
		config, err := pgxpool.ParseConfig(dsn)
		require.NoError(t, err)
		config.MaxConns = 10
		pool, err := pgxpool.NewWithConfig(t.Context(), config)
		require.NoError(t, err)
		t.Cleanup(pool.Close)
		e.replicas[i], err = db.NewWithPGXPool(pool, e.schema)
		require.NoError(t, err)
	}
	return e
}

func (e *env) q(sql string) string {
	return strings.ReplaceAll(sql, "openrails.", pgx.Identifier{e.schema}.Sanitize()+".")
}

func (e *env) newMerchant() uuid.UUID {
	var id uuid.UUID
	require.NoError(e.t, e.admin.QueryRow(e.t.Context(), e.q(`INSERT INTO openrails.merchants (slug) VALUES ($1) RETURNING id`), "idem-"+uuid.NewString()[:8]).Scan(&id))
	return id
}

func (e *env) store(replica int) *idempotency.Store {
	s, err := idempotency.NewStore(e.replicas[replica], e.clock, ttl, lease)
	require.NoError(e.t, err)
	return s
}

func (e *env) ctx() context.Context { return e.ctxFor(e.merchant) }

func (e *env) ctxFor(id uuid.UUID) context.Context {
	return merchant.WithID(e.t.Context(), merchant.ID(id))
}

func (e *env) rows() int {
	var n int
	require.NoError(e.t, e.admin.QueryRow(e.t.Context(), e.q(`SELECT count(*) FROM openrails.idempotency_keys`)).Scan(&n))
	return n
}

type outcome struct {
	claim *idempotency.Claim
	rec   *idempotency.Record
	err   error
}

// race calls Begin for key from n goroutines alternating over both replicas,
// released together.
func (e *env) race(n int, op, key string) []outcome {
	stores := [2]*idempotency.Store{e.store(0), e.store(1)}
	out := make([]outcome, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			out[i].claim, out[i].rec, out[i].err = stores[i%2].Begin(e.ctx(), op, key)
		}()
	}
	close(start)
	wg.Wait()
	return out
}

func owners(t *testing.T, out []outcome) []*idempotency.Claim {
	t.Helper()
	var won []*idempotency.Claim
	for _, o := range out {
		require.NoError(t, o.err)
		if o.claim != nil {
			require.Nil(t, o.rec)
			won = append(won, o.claim)
		} else {
			require.NotNil(t, o.rec)
		}
	}
	return won
}

// Sixteen callers on two replicas race one key, round after round: exactly one
// owns it; the rest see it processing, then its stored result.
func TestBeginRaceHasOneWinner(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	for round := range 40 {
		key := fmt.Sprintf("race-%d-%s", round, uuid.NewString())
		won := owners(t, e.race(16, "checkout", key))
		require.Len(t, won, 1, "round %d", round)
		require.False(t, won[0].Reclaimed)

		for _, o := range e.race(16, "checkout", key) {
			require.NoError(t, o.err)
			require.Nil(t, o.claim, "a live claim is never shared")
			require.Equal(t, idempotency.StatusProcessing, o.rec.Status)
		}
		require.NoError(t, won[0].Complete(e.ctx(), json.RawMessage(fmt.Sprintf(`{"round":%d}`, round))))
		for _, o := range e.race(16, "checkout", key) {
			require.NoError(t, o.err)
			require.Nil(t, o.claim)
			require.Equal(t, idempotency.StatusSucceeded, o.rec.Status)
			require.JSONEq(t, fmt.Sprintf(`{"round":%d}`, round), string(o.rec.Result))
		}
	}
}

// Keys are scoped per merchant and operation.
func TestKeysAreScopedByMerchantAndOperation(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	other := e.newMerchant()
	s := e.store(0)
	a, _, err := s.Begin(e.ctx(), "checkout", "k")
	require.NoError(t, err)
	require.NotNil(t, a)
	b, _, err := s.Begin(e.ctx(), "custodian_sale", "k")
	require.NoError(t, err)
	require.NotNil(t, b, "another operation is another key")
	c, _, err := s.Begin(e.ctxFor(other), "checkout", "k")
	require.NoError(t, err)
	require.NotNil(t, c, "another merchant is another key")
	_, _, err = s.Begin(e.t.Context(), "checkout", "k")
	require.Error(t, err, "a claim needs a merchant")
}

// A completed claim replays its stored payload on either replica, and a
// replay never reopens it.
func TestReplayAfterComplete(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	claim, _, err := e.store(0).Begin(e.ctx(), "checkout", "replay")
	require.NoError(t, err)
	require.NoError(t, claim.Complete(e.ctx(), json.RawMessage(`{"session":"cs_1","status":"succeeded"}`)))
	for replica := range 2 {
		again, rec, err := e.store(replica).Begin(e.ctx(), "checkout", "replay")
		require.NoError(t, err)
		require.Nil(t, again)
		require.Equal(t, idempotency.StatusSucceeded, rec.Status)
		require.JSONEq(t, `{"session":"cs_1","status":"succeeded"}`, string(rec.Result))
		require.EqualValues(t, 1, rec.Claims)
	}
	require.ErrorIs(t, claim.Complete(e.ctx(), nil), idempotency.ErrClaimLost, "a completed claim is final")
	require.ErrorIs(t, claim.Fail(e.ctx(), errors.New("late")), idempotency.ErrClaimLost)
}

// Fail releases the key: exactly one retry reclaims it, and the failed
// owner can no longer write.
func TestFailThenRetry(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	first, _, err := e.store(0).Begin(e.ctx(), "custodian_sale", "retry")
	require.NoError(t, err)
	require.NoError(t, first.Fail(e.ctx(), errors.New("gateway timeout")))
	rec, err := e.store(1).Get(e.ctx(), "custodian_sale", "retry")
	require.NoError(t, err)
	require.Equal(t, idempotency.StatusFailed, rec.Status)
	require.Equal(t, "gateway timeout", rec.Error)

	won := owners(t, e.race(16, "custodian_sale", "retry"))
	require.Len(t, won, 1)
	require.True(t, won[0].Reclaimed)
	require.ErrorIs(t, first.Complete(e.ctx(), json.RawMessage(`{}`)), idempotency.ErrClaimLost)
	require.NoError(t, won[0].Complete(e.ctx(), json.RawMessage(`{"ok":true}`)))
	rec, err = e.store(0).Get(e.ctx(), "custodian_sale", "retry")
	require.NoError(t, err)
	require.Equal(t, idempotency.StatusSucceeded, rec.Status)
	require.EqualValues(t, 2, rec.Claims)
	require.Empty(t, rec.Error)
}

// A held claim survives any silence shorter than its lease; a dead owner's
// claim is reclaimed by exactly one caller once the lease lapses, and the
// dead owner is fenced out.
func TestStaleProcessing(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	owner, _, err := e.store(0).Begin(e.ctx(), "checkout", "stale")
	require.NoError(t, err)
	stop := owner.Hold(e.ctx())
	require.NoError(t, e.clock.BlockUntilContext(t.Context(), 1))
	for range 8 {
		before, err := e.store(1).Get(e.ctx(), "checkout", "stale")
		require.NoError(t, err)
		e.clock.Advance(lease / 4)
		require.Eventually(t, func() bool {
			rec, err := e.store(1).Get(e.ctx(), "checkout", "stale")
			return err == nil && rec.LeaseExpiresAt.After(before.LeaseExpiresAt)
		}, 5*time.Second, 5*time.Millisecond, "the owner renews its lease")
		for _, o := range e.race(4, "checkout", "stale") {
			require.NoError(t, o.err)
			require.Nil(t, o.claim, "a slow owner keeps its claim")
		}
	}
	stop() // the owner dies

	e.clock.Advance(lease - time.Second)
	require.Empty(t, owners(t, e.race(16, "checkout", "stale")), "not before the lease lapses")
	e.clock.Advance(time.Second)
	won := owners(t, e.race(16, "checkout", "stale"))
	require.Len(t, won, 1, "exactly one reclaims a lapsed claim")
	require.True(t, won[0].Reclaimed)
	require.ErrorIs(t, owner.Renew(e.ctx()), idempotency.ErrClaimLost)
	require.ErrorIs(t, owner.Complete(e.ctx(), json.RawMessage(`{"stale":true}`)), idempotency.ErrClaimLost)
	require.NoError(t, won[0].Complete(e.ctx(), json.RawMessage(`{"fresh":true}`)))
	rec, err := e.store(0).Get(e.ctx(), "checkout", "stale")
	require.NoError(t, err)
	require.JSONEq(t, `{"fresh":true}`, string(rec.Result))
}

// Rows past their expiry are claimable afresh, and the GC job deletes them in
// bounded batches, leaving live rows alone.
func TestExpiryAndGC(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	s := e.store(0)
	done, _, err := s.Begin(e.ctx(), "checkout", "done")
	require.NoError(t, err)
	require.NoError(t, done.Complete(e.ctx(), json.RawMessage(`{"n":1}`)))
	_, _, err = s.Begin(e.ctx(), "checkout", "abandoned")
	require.NoError(t, err)
	// A backlog larger than several batches, across two merchants.
	other := e.newMerchant()
	now := e.clock.Now()
	for _, m := range []uuid.UUID{e.merchant, other} {
		_, err := e.admin.Exec(t.Context(), e.q(`INSERT INTO openrails.idempotency_keys
			(merchant_id, operation, idempotency_key, status, result, lease_expires_at, expires_at, created_at, updated_at)
			SELECT $1, 'webhook.stripe.test', 'evt_' || g, 'succeeded', '{}'::jsonb, $2, $3, $2, $2 FROM generate_series(1, 1300) g`),
			m, now, now.Add(ttl))
		require.NoError(t, err)
	}
	require.Equal(t, 2602, e.rows())

	gc := riverjobs.IdempotencyGCWorker{DB: e.replicas[1], Clock: e.clock}
	require.NoError(t, gc.Work(t.Context(), &river.Job[riverjobs.IdempotencyGCArgs]{}))
	require.Equal(t, 2602, e.rows(), "nothing has expired")

	e.clock.Advance(ttl)
	again, _, err := s.Begin(e.ctx(), "checkout", "done")
	require.NoError(t, err)
	require.NotNil(t, again, "an expired result is not replayed; the key is claimable afresh")
	deleted, err := gc.Sweep(t.Context())
	require.NoError(t, err)
	require.EqualValues(t, 2601, deleted, "every expired row, across batches and merchants")
	require.Equal(t, 1, e.rows(), "the fresh claim stays")
	require.NoError(t, again.Complete(e.ctx(), json.RawMessage(`{"n":2}`)))
}

// Two replicas' dedup services see one delivery storm for one event: its
// effects run once and every duplicate answers the owner's outcome. A
// retryable failure releases the event to the next redelivery on any replica;
// a crash after the effects committed is recognised from webhook_events once
// the dead claim lapses.
func TestWebhookDedupeAcrossReplicas(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	var dedup [2]*webhooks.DeduplicationService
	for i := range dedup {
		claims, err := idempotency.NewStore(e.replicas[i], e.clock, webhooks.WebhookClaimTTL, webhooks.WebhookClaimLease)
		require.NoError(t, err)
		dedup[i], err = webhooks.NewDeduplicationService(claims, e.replicas[i])
		require.NoError(t, err)
	}
	source := models.RailStripe.EventSource()
	deliver := func(replica int, event string, effect func(ctx context.Context) error) error {
		return dedup[replica].ProcessWebhook(e.ctx(), event, "payment_intent.succeeded", source, effect)
	}

	var applied atomic.Int32
	slow := func(ctx context.Context) error {
		applied.Add(1)
		time.Sleep(100 * time.Millisecond)
		return nil
	}
	start := make(chan struct{})
	errs := make([]error, 16)
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs[i] = deliver(i%2, "evt_storm", slow)
		}()
	}
	close(start)
	wg.Wait()
	require.EqualValues(t, 1, applied.Load(), "one delivery applies the event")
	for _, err := range errs {
		require.NoError(t, err, "a concurrent duplicate waits for the owner and answers its outcome")
	}
	for replica := range 2 {
		require.NoError(t, deliver(replica, "evt_storm", slow))
	}
	require.EqualValues(t, 1, applied.Load(), "late redeliveries change nothing")

	// A retryable failure on one replica; the redelivery lands on the other.
	var tries atomic.Int32
	flaky := func(ctx context.Context) error {
		if tries.Add(1) == 1 {
			return errors.New("database hiccup")
		}
		return nil
	}
	require.Error(t, deliver(0, "evt_flaky", flaky))
	require.NoError(t, deliver(1, "evt_flaky", flaky))
	require.NoError(t, deliver(0, "evt_flaky", flaky))
	require.EqualValues(t, 2, tries.Load())

	// A replica dies after committing the effects and the webhook_events mark
	// in the handler's transaction, before completing its claim.
	var crashed atomic.Int32
	claims, err := idempotency.NewStore(e.replicas[0], e.clock, webhooks.WebhookClaimTTL, webhooks.WebhookClaimLease)
	require.NoError(t, err)
	dead, _, err := claims.Begin(e.ctx(), webhookOp(source), "evt_crash")
	require.NoError(t, err)
	require.NotNil(t, dead)
	_, err = e.admin.Exec(t.Context(), e.q(`INSERT INTO openrails.webhook_events (merchant_id, op, event_id) VALUES ($1, $2, 'evt_crash')`), e.merchant, webhookOp(source))
	require.NoError(t, err)
	count := func(ctx context.Context) error { crashed.Add(1); return nil }
	e.clock.Advance(webhooks.WebhookClaimLease)
	require.NoError(t, deliver(1, "evt_crash", count))
	require.Zero(t, crashed.Load(), "the applied fact wins over the reclaimed delivery")
	rec, err := e.store(1).Get(e.ctx(), webhookOp(source), "evt_crash")
	require.NoError(t, err)
	require.Equal(t, idempotency.StatusSucceeded, rec.Status)
}

func webhookOp(source models.EventSource) string {
	return fmt.Sprintf("webhook.%s.%s", source, "payment_intent.succeeded")
}
