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
	lease = 2 * time.Second
)

// replica is one process: its request pool and its lease-renewal pool.
type replica struct {
	db     *db.DB
	leases *db.DB
}

// env is one schema shared by two replicas.
type env struct {
	t        *testing.T
	admin    *pgxpool.Pool
	schema   string
	replicas [2]replica
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
	e := &env{t: t, admin: admin, schema: "gf_idem_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]}
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
		d, err := db.NewWithPGXPool(pool, e.schema)
		require.NoError(t, err)
		leases, err := d.SeparatePool(t.Context(), 2)
		require.NoError(t, err)
		t.Cleanup(func() { _ = leases.Close() })
		e.replicas[i] = replica{db: d, leases: leases}
	}
	return e
}

func (e *env) q(sql string) string {
	return strings.ReplaceAll(sql, "openrails.", pgx.Identifier{e.schema}.Sanitize()+".")
}

func (e *env) exec(sql string, args ...any) int64 {
	tag, err := e.admin.Exec(e.t.Context(), e.q(sql), args...)
	require.NoError(e.t, err)
	return tag.RowsAffected()
}

func (e *env) newMerchant() uuid.UUID {
	var id uuid.UUID
	require.NoError(e.t, e.admin.QueryRow(e.t.Context(), e.q(`INSERT INTO openrails.merchants (slug) VALUES ($1) RETURNING id`), "idem-"+uuid.NewString()[:8]).Scan(&id))
	return id
}

func (e *env) storeWith(i int, ttl, lease time.Duration) *idempotency.Store {
	s, err := idempotency.NewStore(e.replicas[i].db, e.replicas[i].leases, ttl, lease)
	require.NoError(e.t, err)
	return s
}

func (e *env) store(i int) *idempotency.Store { return e.storeWith(i, ttl, lease) }

func (e *env) ctx() context.Context { return e.ctxFor(e.merchant) }

func (e *env) ctxFor(id uuid.UUID) context.Context {
	return merchant.WithID(e.t.Context(), merchant.ID(id))
}

// lapse ends key's lease on the database clock, as elapsed time or starved
// renewals would.
func (e *env) lapse(op, key string) {
	require.EqualValues(e.t, 1, e.exec(`UPDATE openrails.idempotency_keys SET lease_expires_at = now() - interval '1 millisecond'
		WHERE operation = $1 AND idempotency_key = $2 AND status = 'processing'`, op, key))
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
			require.True(t, o.rec.Leased)
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
	for i := range 2 {
		again, rec, err := e.store(i).Begin(e.ctx(), "checkout", "replay")
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

// A held claim outlives many leases; a dead owner's claim is reclaimed by
// exactly one caller once its lease lapses on the database clock, and the
// dead owner is fenced out.
func TestStaleProcessing(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	owner, _, err := e.store(0).Begin(e.ctx(), "checkout", "stale")
	require.NoError(t, err)
	work, stop := owner.Hold(e.ctx())
	deadline := time.Now().Add(3 * lease)
	for time.Now().Before(deadline) {
		require.Empty(t, owners(t, e.race(4, "checkout", "stale")), "a live owner keeps its claim")
		time.Sleep(lease / 5)
	}
	require.NoError(t, work.Err(), "renewals kept the owner working")
	stop() // the owner dies

	require.Empty(t, owners(t, e.race(16, "checkout", "stale")), "not before the lease lapses")
	require.Eventually(t, func() bool {
		rec, err := e.store(1).Get(e.ctx(), "checkout", "stale")
		return err == nil && !rec.Leased
	}, 2*lease, 20*time.Millisecond)
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

// When no renewal can be confirmed, the owner's context is cancelled before
// its lease can lapse: an owner stops before anyone else may start.
func TestHoldCancelsBeforeTheLeaseLapses(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	owner, _, err := e.store(0).Begin(e.ctx(), "checkout", "starved")
	require.NoError(t, err)
	// Starve renewals: a transaction holds the claim row.
	tx, err := e.admin.Begin(t.Context())
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(context.Background()) }()
	_, err = tx.Exec(t.Context(), e.q(`SELECT 1 FROM openrails.idempotency_keys WHERE idempotency_key = 'starved' FOR UPDATE`))
	require.NoError(t, err)

	work, stop := owner.Hold(e.ctx())
	defer stop()
	select {
	case <-work.Done():
	case <-time.After(2 * lease):
		t.Fatal("the owner kept working without a confirmed lease")
	}
	require.ErrorIs(t, context.Cause(work), idempotency.ErrClaimLost)
	rec, err := e.store(1).Watch(e.ctx(), "checkout", "starved")
	require.NoError(t, err)
	require.True(t, rec.Leased, "the owner stopped while its lease still held")

	// A reclaim after the lapse passes the key on; the stopped owner's
	// transactions refuse to commit.
	require.NoError(t, tx.Rollback(t.Context()))
	e.lapse("checkout", "starved")
	won := owners(t, e.race(8, "checkout", "starved"))
	require.Len(t, won, 1)
	guarded := db.WithCommitGuard(e.ctx(), owner.InTx)
	err = e.replicas[0].db.MerchantTx(guarded, func(ctx context.Context, tx pgx.Tx) error { return nil })
	require.ErrorIs(t, err, idempotency.ErrClaimLost)
	require.NoError(t, e.replicas[1].db.MerchantTx(db.WithCommitGuard(e.ctx(), won[0].InTx), func(context.Context, pgx.Tx) error { return nil }))
}

// A token is never reused: an owner whose row was collected and claimed
// afresh is still fenced out.
func TestTokenSurvivesCollection(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	old, _, err := e.store(0).Begin(e.ctx(), "checkout", "gc")
	require.NoError(t, err)
	e.exec(`UPDATE openrails.idempotency_keys SET lease_expires_at = now() - interval '2 seconds', expires_at = now() - interval '1 second' WHERE idempotency_key = 'gc'`)
	deleted, err := riverjobs.IdempotencyGCWorker{DB: e.replicas[1].db}.Sweep(t.Context())
	require.NoError(t, err)
	require.EqualValues(t, 1, deleted)
	fresh, _, err := e.store(1).Begin(e.ctx(), "checkout", "gc")
	require.NoError(t, err)
	require.NotNil(t, fresh)
	require.False(t, fresh.Reclaimed, "the key started over")
	require.ErrorIs(t, old.Complete(e.ctx(), json.RawMessage(`{"old":true}`)), idempotency.ErrClaimLost)
	require.NoError(t, fresh.Complete(e.ctx(), json.RawMessage(`{"fresh":true}`)))
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
	for _, m := range []uuid.UUID{e.merchant, other} {
		e.exec(`INSERT INTO openrails.idempotency_keys
			(merchant_id, operation, idempotency_key, status, token, result, lease_expires_at, expires_at)
			SELECT $1, 'webhook.stripe.test', 'evt_' || g, 'succeeded', gen_random_uuid(), '{}'::jsonb, now(), now() + interval '1 hour' FROM generate_series(1, 1300) g`, m)
	}
	require.Equal(t, 2602, e.rows())

	gc := riverjobs.IdempotencyGCWorker{DB: e.replicas[1].db}
	require.NoError(t, gc.Work(t.Context(), &river.Job[riverjobs.IdempotencyGCArgs]{}))
	require.Equal(t, 2602, e.rows(), "nothing has expired")

	e.exec(`UPDATE openrails.idempotency_keys SET lease_expires_at = LEAST(lease_expires_at, now() - interval '2 seconds'), expires_at = now() - interval '1 second'`)
	again, _, err := s.Begin(e.ctx(), "checkout", "done")
	require.NoError(t, err)
	require.NotNil(t, again, "an expired result is not replayed; the key is claimable afresh")
	deleted, err := gc.Sweep(t.Context())
	require.NoError(t, err)
	require.EqualValues(t, 2601, deleted, "every expired row, across batches and merchants")
	require.Equal(t, 1, e.rows(), "the fresh claim stays")
	require.NoError(t, again.Complete(e.ctx(), json.RawMessage(`{"n":2}`)))
}

// dedup is one replica's webhook dedup service.
func (e *env) dedup(i int, lease time.Duration) *webhooks.DeduplicationService {
	d, err := webhooks.NewDeduplicationService(e.storeWith(i, webhooks.WebhookClaimTTL, lease), e.replicas[i].db)
	require.NoError(e.t, err)
	return d
}

// effects is a money effect table: one row per application of an event. The
// handler marks the event in its effect transaction.
func (e *env) effects() func(ctx context.Context, d *db.DB, event string) error {
	e.exec(`CREATE TABLE openrails.test_effects (event text NOT NULL)`)
	return func(ctx context.Context, d *db.DB, event string) error {
		return effect(ctx, d, event, true)
	}
}

// effect applies event; mark=false is a handler that leaves the mark to
// ProcessWebhook.
func effect(ctx context.Context, d *db.DB, event string, mark bool) error {
	return d.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO openrails.test_effects (event) VALUES ($1)`, event); err != nil {
			return err
		}
		if !mark {
			return nil
		}
		return webhooks.MarkWebhookProcessedInTx(ctx, tx)
	})
}

func (e *env) applied(event string) int {
	var n int
	require.NoError(e.t, e.admin.QueryRow(e.t.Context(), e.q(`SELECT count(*) FROM openrails.test_effects WHERE event = $1`), event).Scan(&n))
	return n
}

// Two replicas' dedup services see one delivery storm for one event: its
// effects run once and every duplicate answers the owner's outcome. A
// retryable failure releases the event to the next redelivery on any replica;
// a crash after the effects committed is recognised from webhook_events once
// the dead claim lapses.
func TestWebhookDedupeAcrossReplicas(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	dedup := [2]*webhooks.DeduplicationService{e.dedup(0, webhooks.WebhookClaimLease), e.dedup(1, webhooks.WebhookClaimLease)}
	apply := e.effects()
	source := models.RailStripe.EventSource()
	deliver := func(i int, event string, effect func(ctx context.Context) error) error {
		return dedup[i].ProcessWebhook(e.ctx(), event, "payment_intent.succeeded", source, effect)
	}

	slow := func(i int) func(ctx context.Context) error {
		return func(ctx context.Context) error {
			time.Sleep(100 * time.Millisecond)
			return apply(ctx, e.replicas[i].db, "evt_storm")
		}
	}
	start := make(chan struct{})
	errs := make([]error, 16)
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs[i] = deliver(i%2, "evt_storm", slow(i%2))
		}()
	}
	close(start)
	wg.Wait()
	require.Equal(t, 1, e.applied("evt_storm"), "one delivery applies the event")
	for _, err := range errs {
		require.NoError(t, err, "a concurrent duplicate waits for the owner and answers its outcome")
	}
	for i := range 2 {
		require.NoError(t, deliver(i, "evt_storm", slow(i)))
	}
	require.Equal(t, 1, e.applied("evt_storm"), "late redeliveries change nothing")

	// A retryable failure on one replica; the redelivery lands on the other.
	var tries atomic.Int32
	flaky := func(i int) func(ctx context.Context) error {
		return func(ctx context.Context) error {
			if tries.Add(1) == 1 {
				return errors.New("database hiccup")
			}
			return apply(ctx, e.replicas[i].db, "evt_flaky")
		}
	}
	require.Error(t, deliver(0, "evt_flaky", flaky(0)))
	require.NoError(t, deliver(1, "evt_flaky", flaky(1)))
	require.NoError(t, deliver(0, "evt_flaky", flaky(0)))
	require.EqualValues(t, 2, tries.Load())
	require.Equal(t, 1, e.applied("evt_flaky"))

	// A replica dies after committing the effects and the webhook_events mark
	// in the handler's transaction, before completing its claim.
	op := fmt.Sprintf("webhook.%s.%s", source, "payment_intent.succeeded")
	dead, _, err := e.store(0).Begin(e.ctx(), op, "evt_crash")
	require.NoError(t, err)
	require.NotNil(t, dead)
	e.exec(`INSERT INTO openrails.webhook_events (merchant_id, op, event_id) VALUES ($1, $2, 'evt_crash')`, e.merchant, op)
	e.lapse(op, "evt_crash")
	var crashed atomic.Int32
	require.NoError(t, deliver(1, "evt_crash", func(context.Context) error { crashed.Add(1); return nil }))
	require.Zero(t, crashed.Load(), "the applied fact wins over the reclaimed delivery")
	rec, err := e.store(1).Get(e.ctx(), op, "evt_crash")
	require.NoError(t, err)
	require.Equal(t, idempotency.StatusSucceeded, rec.Status)
}

// An owner whose lease lapsed while it was alive and mid-handler never
// commits its effects: the replica that reclaimed the event applies it once,
// and the superseded owner's transaction is refused by its commit guard.
func TestWebhookOwnerLosingItsLeaseNeverApplies(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	// A lease long enough that the owner's own renewals do not notice first:
	// the commit guard alone must stop it.
	dedup := [2]*webhooks.DeduplicationService{e.dedup(0, time.Minute), e.dedup(1, time.Minute)}
	e.effects()
	// A handler without the in-transaction mark: only the guard protects it.
	apply := func(ctx context.Context, d *db.DB, event string) error { return effect(ctx, d, event, false) }
	source := models.RailStripe.EventSource()
	op := fmt.Sprintf("webhook.%s.%s", source, "charge.refunded")

	inside, resume := make(chan struct{}), make(chan struct{})
	ownerErr := make(chan error, 1)
	go func() {
		ownerErr <- dedup[0].ProcessWebhook(e.ctx(), "evt_lapse", "charge.refunded", source, func(ctx context.Context) error {
			close(inside)
			<-resume
			return apply(ctx, e.replicas[0].db, "evt_lapse")
		})
	}()
	<-inside
	e.lapse(op, "evt_lapse")
	require.NoError(t, dedup[1].ProcessWebhook(e.ctx(), "evt_lapse", "charge.refunded", source, func(ctx context.Context) error {
		return apply(ctx, e.replicas[1].db, "evt_lapse")
	}))
	close(resume)
	require.ErrorIs(t, <-ownerErr, idempotency.ErrClaimLost)
	require.Equal(t, 1, e.applied("evt_lapse"), "the event is applied once")
	for i := range 2 {
		require.NoError(t, dedup[i].ProcessWebhook(e.ctx(), "evt_lapse", "charge.refunded", source, func(ctx context.Context) error {
			return apply(ctx, e.replicas[i].db, "evt_lapse")
		}))
	}
	require.Equal(t, 1, e.applied("evt_lapse"))
}

// A handler transaction that finds the event already marked rolls back.
func TestWebhookMarkRollsBackADuplicateTransaction(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	apply := e.effects()
	d := e.dedup(0, webhooks.WebhookClaimLease)
	source := models.RailStripe.EventSource()
	op := fmt.Sprintf("webhook.%s.%s", source, "invoice.paid")
	require.NoError(t, d.ProcessWebhook(e.ctx(), "evt_twice", "invoice.paid", source, func(ctx context.Context) error {
		// The mark lands between the handler's two effect transactions.
		if err := apply(ctx, e.replicas[0].db, "evt_twice"); err != nil {
			return err
		}
		return apply(ctx, e.replicas[0].db, "evt_twice")
	}))
	require.Equal(t, 1, e.applied("evt_twice"), "the second transaction saw the mark and rolled back")
	var marks int
	require.NoError(t, e.admin.QueryRow(t.Context(), e.q(`SELECT count(*) FROM openrails.webhook_events WHERE op = $1 AND event_id = 'evt_twice'`), op).Scan(&marks))
	require.Equal(t, 1, marks)
}

// Duplicates waiting out an owner use neither the lease-renewal pool nor
// their request's pinned connection: two dozen waiters on 10-connection pools
// all get their answer, and the owner's renewals keep its claim alive.
func TestWebhookDuplicatesLeaveRenewalsAlone(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	dedup := [2]*webhooks.DeduplicationService{e.dedup(0, lease), e.dedup(1, lease)}
	e.effects()
	source := models.RailStripe.EventSource()
	op := fmt.Sprintf("webhook.%s.%s", source, "invoice.paid")
	apply := func(i int) func(ctx context.Context) error {
		return func(ctx context.Context) error { return effect(ctx, e.replicas[i].db, "evt_hot", true) }
	}

	inside, resume := make(chan struct{}), make(chan struct{})
	ownerErr := make(chan error, 1)
	go func() {
		ownerErr <- dedup[0].ProcessWebhook(e.ctx(), "evt_hot", "invoice.paid", source, func(ctx context.Context) error {
			close(inside)
			<-resume
			return apply(0)(ctx)
		})
	}()
	<-inside
	renewals := e.replicas[0].leases.Pool().Stat().AcquireCount()

	const waiters = 24
	errs := make([]error, waiters)
	var wg sync.WaitGroup
	for i := range waiters {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, release, err := e.replicas[i%2].db.WithMerchantConn(e.ctx())
			if err != nil {
				errs[i] = err
				return
			}
			defer release()
			errs[i] = dedup[i%2].ProcessWebhook(ctx, "evt_hot", "invoice.paid", source, apply(i%2))
		}()
	}
	time.Sleep(lease / 2)
	require.Eventually(t, func() bool {
		held := e.replicas[0].db.Pool().Stat().AcquiredConns() + e.replicas[1].db.Pool().Stat().AcquiredConns()
		return held <= 4
	}, 2*lease, 50*time.Millisecond, "waiters hold no request connection")
	time.Sleep(lease) // past the lease: only renewals keep the owner's claim
	rec, err := e.store(1).Watch(e.ctx(), op, "evt_hot")
	require.NoError(t, err)
	require.True(t, rec.Leased, "the owner's renewals stayed on time")
	require.LessOrEqual(t, e.replicas[0].leases.Pool().Stat().AcquireCount()-renewals, int64(10), "the lease pool served renewals only")

	close(resume)
	require.NoError(t, <-ownerErr)
	wg.Wait()
	for i, err := range errs {
		require.NoError(t, err, "waiter %d answers the owner's outcome", i)
	}
	require.Equal(t, 1, e.applied("evt_hot"))
}

// #1105: a request holding its pinned connection runs pool work on that same
// connection, even on a one-connection pool; and a pool with nothing free
// answers ErrPoolExhausted within a bound instead of waiting forever.
func TestPoolWorkReusesThePinAndNeverHangs(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	config, err := pgxpool.ParseConfig(strings.TrimSpace(os.Getenv("OPENRAILS_GREENFIELD_DSN")))
	require.NoError(t, err)
	config.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	d, err := db.NewWithPGXPool(pool, e.schema)
	require.NoError(t, err)

	ctx, release, err := d.WithMerchantConn(e.ctx())
	require.NoError(t, err)
	var n int
	require.NoError(t, d.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error { return nil }))
	require.NoError(t, d.DataPool().MerchantTx(ctx, merchant.ID(e.merchant), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM openrails.idempotency_keys`).Scan(&n)
	}), "pool work inside a pinned request reuses the pin")
	require.NoError(t, d.DataPool().QueryRow(ctx, `SELECT 1`).Scan(&n))

	// The pin holds the only connection: work outside the request is refused
	// in bounded time.
	started := time.Now()
	_, err = d.DataPool().Exec(e.t.Context(), `SELECT 1`)
	require.ErrorIs(t, err, db.ErrPoolExhausted)
	require.Less(t, time.Since(started), 15*time.Second)
	release()
	_, err = d.DataPool().Exec(e.t.Context(), `SELECT 1`)
	require.NoError(t, err)
}
