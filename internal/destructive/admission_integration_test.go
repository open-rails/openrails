//go:build e2e && integration

package destructive_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/billingauth/authtest"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/destructive"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/migrate"
	"github.com/stretchr/testify/require"
)

type admissionWorld struct {
	t      *testing.T
	pool   *pgxpool.Pool
	db     *db.DB
	schema string
	clock  *clockwork.FakeClock
	gate   *destructive.Admissions
}

func newAdmissionsWorld(t *testing.T) *admissionWorld {
	t.Helper()
	dsn := os.Getenv("OPENRAILS_E2E_DSN")
	if dsn == "" {
		t.Skip("OPENRAILS_E2E_DSN is required")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	cfg.MaxConns = 2
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	require.NoError(t, err)
	schema := "destructive_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	t.Cleanup(func() {
		_, err := pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		require.NoError(t, err)
		pool.Close()
	})
	require.NoError(t, migrate.Apply(t.Context(), pool, migrate.Options{Schema: schema}))
	database, err := db.NewWithPGXPool(pool, schema)
	require.NoError(t, err)
	w := &admissionWorld{t: t, pool: pool, db: database, schema: schema, clock: clockwork.NewFakeClockAt(time.Now().UTC().Truncate(time.Microsecond))}
	w.gate = destructive.NewAdmissions(database, w.clock, w.project)
	return w
}
func (w *admissionWorld) project(ctx context.Context, tx pgx.Tx, h destructive.Hold) error {
	data, err := json.Marshal(h)
	if err != nil {
		return err
	}
	kind := "destructive.hold_opened"
	at := h.OpenedAt
	if h.ClearedAt != nil {
		kind = "destructive.hold_cleared"
		at = *h.ClearedAt
	}
	return gen.New(tx).EnqueueDestructiveHoldEvent(ctx, gen.EnqueueDestructiveHoldEventParams{MerchantID: h.MerchantID, EventType: kind, HoldID: h.ID, OccurredAt: at, Data: data, DedupeKey: kind + ":" + h.ID.String()})
}
func (w *admissionWorld) exec(sql string, args ...any) {
	w.t.Helper()
	_, err := w.pool.Exec(w.t.Context(), strings.ReplaceAll(sql, "billing.", w.schema+"."), args...)
	require.NoError(w.t, err)
}
func (w *admissionWorld) merchant() uuid.UUID {
	w.t.Helper()
	id := uuid.New()
	w.exec(`INSERT INTO billing.merchants(id,slug,status) VALUES($1,$2,'active')`, id, "core-"+id.String())
	return id
}
func command(mid uuid.UUID, actor string) destructive.Command {
	key := uuid.NewString()
	return destructive.Command{MerchantID: mid, Kind: destructive.Refund, Target: "payment:" + key, Key: key, Digest: destructive.Digest(key), Units: 1, Actor: destructive.Actor{Issuer: "https://issuer.test", ID: actor, Credential: "key:one"}}
}
func (w *admissionWorld) done(c destructive.Command) destructive.Action {
	w.t.Helper()
	a, err := w.gate.Admit(w.t.Context(), c)
	require.NoError(w.t, err)
	_, err = w.gate.Start(w.t.Context(), c.MerchantID, a.ID)
	require.NoError(w.t, err)
	require.NoError(w.t, w.gate.Finish(w.t.Context(), c.MerchantID, a.ID, destructive.OutcomeSucceeded, "done"))
	return a
}
func held(t *testing.T, err error) destructive.Hold {
	t.Helper()
	var e *destructive.HeldError
	require.ErrorAs(t, err, &e)
	return e.Hold
}

var operator = destructive.Actor{Issuer: "https://operator.test", ID: "operator"}

func TestDestructiveAdmissionConcurrencyAndPersistentHold(t *testing.T) {
	w := newAdmissionsWorld(t)
	mid, other := w.merchant(), w.merchant()
	for range 4 {
		w.done(command(mid, "same"))
	}
	start := make(chan struct{})
	type answer struct {
		action destructive.Action
		err    error
	}
	answers := make(chan answer, 2)
	for _, m := range []uuid.UUID{mid, other} {
		go func() { <-start; a, e := w.gate.Admit(t.Context(), command(m, "same")); answers <- answer{a, e} }()
	}
	close(start)
	var accepted destructive.Action
	var incident destructive.Hold
	for range 2 {
		r := <-answers
		if r.err == nil {
			require.Equal(t, uuid.Nil, accepted.ID)
			accepted = r.action
		} else {
			incident = held(t, r.err)
		}
	}
	require.NotEqual(t, uuid.Nil, accepted.ID)
	require.Equal(t, destructive.ScopeActor, incident.Scope)
	_, err := w.gate.Start(t.Context(), accepted.MerchantID, accepted.ID)
	require.ErrorIs(t, err, destructive.ErrHeld)
	require.NoError(t, w.gate.Finish(t.Context(), accepted.MerchantID, accepted.ID, destructive.OutcomeNoEffect, "not started"))
	w.clock.Advance(48 * time.Hour)
	restarted := destructive.NewAdmissions(w.db, w.clock, w.project)
	_, err = restarted.Admit(t.Context(), command(other, "same"))
	require.Equal(t, incident.ID, held(t, err).ID, "time and process restart do not clear")
	_, err = restarted.Clear(t.Context(), incident.MerchantID, incident.ID, command(mid, "same").Actor, "reviewed")
	require.ErrorIs(t, err, destructive.ErrSelfClear)
	cleared, err := restarted.Clear(t.Context(), incident.MerchantID, incident.ID, operator, "reviewed")
	require.NoError(t, err)
	require.NotNil(t, cleared.ClearedAt)
	_, err = restarted.Clear(t.Context(), incident.MerchantID, incident.ID, operator, "different reason")
	require.ErrorIs(t, err, destructive.ErrConflict)
	_, err = restarted.Admit(t.Context(), command(other, "same"))
	require.NoError(t, err)
	var events int
	require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+w.schema+".host_outbox").Scan(&events))
	require.Equal(t, 2, events, "one durable opened event and one clear")
}

func TestDestructiveAdmissionReplayBatchAndNoEffect(t *testing.T) {
	w := newAdmissionsWorld(t)
	mid := w.merchant()
	c := command(mid, "actor")
	a, err := w.gate.Admit(t.Context(), c)
	require.NoError(t, err)
	c.Actor.Credential = "key:rotated"
	replay, err := w.gate.Admit(t.Context(), c)
	require.NoError(t, err)
	require.Equal(t, a.ID, replay.ID)
	require.True(t, replay.Replayed)
	stranger := c
	stranger.Actor.ID = "other"
	_, err = w.gate.Admit(t.Context(), stranger)
	require.ErrorIs(t, err, destructive.ErrConflict)
	require.NoError(t, w.gate.Finish(t.Context(), mid, a.ID, destructive.OutcomeNoEffect, "rolled back"))
	retry, err := w.gate.Admit(t.Context(), c)
	require.NoError(t, err)
	require.NotEqual(t, a.ID, retry.ID)
	_, err = w.gate.Start(t.Context(), mid, a.ID)
	require.ErrorIs(t, err, destructive.ErrActionFinal)
	changed := c
	changed.Digest = destructive.Digest("different")
	_, err = w.gate.Admit(t.Context(), changed)
	require.ErrorIs(t, err, destructive.ErrConflict)
	second := command(mid, "actor")
	second.Units = 4
	batch, err := w.gate.AdmitBatch(t.Context(), []destructive.Command{c, second})
	require.NoError(t, err)
	require.True(t, batch[0].Replayed)
	third, fourth := command(mid, "actor"), command(mid, "actor")
	_, err = w.gate.AdmitBatch(t.Context(), []destructive.Command{third, fourth})
	held(t, err)
	var count int
	require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+w.schema+".destructive_actions").Scan(&count))
	require.Equal(t, 3, count, "a denied batch creates no partial receipts")
	otherIssuer := command(mid, "actor")
	otherIssuer.Actor.Issuer = "https://different.test"
	_, err = w.gate.Admit(t.Context(), otherIssuer)
	require.NoError(t, err, "issuer-qualified actor is distinct")
}

func TestDestructiveAdmissionScopesAndClearEpoch(t *testing.T) {
	w := newAdmissionsWorld(t)
	mid := w.merchant()
	for i := range 15 {
		w.done(command(mid, fmt.Sprint(i)))
	}
	_, err := w.gate.Admit(t.Context(), command(mid, "next"))
	h := held(t, err)
	require.Equal(t, destructive.ScopeMerchant, h.Scope)
	_, err = w.gate.Clear(t.Context(), mid, h.ID, operator, "clear merchant")
	require.NoError(t, err)
	// The same fake instant still starts a new sequence epoch, not a timestamp loophole.
	for range 5 {
		w.done(command(mid, "new actor"))
	}
	_, err = w.gate.Admit(t.Context(), command(mid, "new actor"))
	h = held(t, err)
	require.Equal(t, destructive.ScopeActor, h.Scope)
	_, err = w.gate.Clear(t.Context(), mid, h.ID, operator, "clear actor")
	require.NoError(t, err)
	// Previous actor hold is cleared, but pending units must survive a new clear.
	for range 5 {
		_, err = w.gate.Admit(t.Context(), command(mid, "pending"))
		require.NoError(t, err)
	}
	_, err = w.gate.Admit(t.Context(), command(mid, "pending"))
	h = held(t, err)
	_, err = w.gate.Clear(t.Context(), mid, h.ID, operator, "keep reservations")
	require.NoError(t, err)
	w.clock.Advance(72 * time.Hour)
	_, err = w.gate.Admit(t.Context(), command(mid, "pending"))
	require.Equal(t, destructive.ScopeActor, held(t, err).Scope)
}

func TestDestructiveAdmissionPendingCompletionConsumesClearEpoch(t *testing.T) {
	w := newAdmissionsWorld(t)
	mid := w.merchant()
	var admitted []destructive.Action
	for range 5 {
		a, err := w.gate.Admit(t.Context(), command(mid, "late"))
		require.NoError(t, err)
		admitted = append(admitted, a)
	}
	_, err := w.gate.Admit(t.Context(), command(mid, "late"))
	h := held(t, err)
	_, err = w.gate.Clear(t.Context(), mid, h.ID, operator, "keep earlier reservations")
	require.NoError(t, err)
	// Every receipt preceded clear, but all its effects complete afterwards at
	// the same clock instant. Their units still consume the new epoch.
	for _, a := range admitted {
		_, err = w.gate.Start(t.Context(), mid, a.ID)
		require.NoError(t, err)
		require.NoError(t, w.gate.Finish(t.Context(), mid, a.ID, destructive.OutcomeSucceeded, "late receipt"))
	}
	_, err = w.gate.Admit(t.Context(), command(mid, "late"))
	require.Equal(t, destructive.ScopeActor, held(t, err).Scope)
}

func TestDestructiveAdmissionDeploymentBudget(t *testing.T) {
	w := newAdmissionsWorld(t)
	for i := range 50 {
		w.done(command(w.merchant(), fmt.Sprint(i)))
	}
	_, err := w.gate.Admit(t.Context(), command(w.merchant(), "last"))
	require.Equal(t, destructive.ScopeDeployment, held(t, err).Scope)
}

func TestDestructiveAdmissionCommitBoundary(t *testing.T) {
	w := newAdmissionsWorld(t)
	mid := w.merchant()
	ctx := merchant.WithID(t.Context(), billing.MerchantID(mid))
	err := w.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := w.gate.Admit(ctx, command(mid, "actor"))
		require.ErrorIs(t, err, db.ErrCallerTransaction)
		return errors.New("caller rollback")
	})
	require.Error(t, err)
	// A one-connection pool and existing lazy pin must reuse that connection.
	cfg := w.pool.Config()
	cfg.MaxConns = 1
	one, err := pgxpool.NewWithConfig(t.Context(), cfg)
	require.NoError(t, err)
	defer one.Close()
	database, err := db.NewWithPGXPool(one, w.schema)
	require.NoError(t, err)
	gate := destructive.NewAdmissions(database, w.clock, w.project)
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	require.NoError(t, database.RunInMerchantConn(ctx, func(ctx context.Context) error { _, err := gate.Admit(ctx, command(mid, "one")); return err }))
	// Projection failure rolls back both the hold and its delivery facts.
	broken := destructive.NewAdmissions(database, w.clock, func(context.Context, pgx.Tx, destructive.Hold) error { return errors.New("queue unavailable") })
	c := command(mid, "burst")
	c.Units = 6
	_, err = broken.Admit(t.Context(), c)
	require.ErrorContains(t, err, "queue unavailable")
	var holds int
	require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+w.schema+".destructive_holds").Scan(&holds))
	require.Zero(t, holds)
}

func TestDestructiveAdmissionStartedRecoveryAndImmutableFacts(t *testing.T) {
	w := newAdmissionsWorld(t)
	mid := w.merchant()
	c := command(mid, "actor")
	a, err := w.gate.Admit(t.Context(), c)
	require.NoError(t, err)
	_, err = w.gate.Start(t.Context(), mid, a.ID)
	require.NoError(t, err)
	require.NoError(t, w.gate.Finish(t.Context(), mid, a.ID, destructive.OutcomeUncertain, "provider response lost"))
	for range 4 {
		_, err = w.gate.Admit(t.Context(), command(mid, "actor"))
		require.NoError(t, err)
	}
	_, err = w.gate.Admit(t.Context(), command(mid, "actor"))
	held(t, err)
	resumed, err := w.gate.Start(t.Context(), mid, a.ID)
	require.NoError(t, err)
	require.Equal(t, destructive.StateUncertain, resumed.State)
	require.NoError(t, w.gate.Finish(t.Context(), mid, a.ID, destructive.OutcomeSucceeded, "receipt"))
	require.NoError(t, w.gate.Finish(t.Context(), mid, a.ID, destructive.OutcomeNoEffect, "racing no-op"))
	final, err := w.gate.GetAction(t.Context(), mid, a.ID)
	require.NoError(t, err)
	require.Equal(t, destructive.StateSucceeded, final.State)
	_, err = w.pool.Exec(t.Context(), "UPDATE "+w.schema+".destructive_actions SET actor_id='other' WHERE id=$1", a.ID)
	require.Error(t, err)
	_, err = w.pool.Exec(t.Context(), "DELETE FROM "+w.schema+".destructive_actions WHERE id=$1", a.ID)
	require.Error(t, err)
}

func TestDestructiveAdmissionAutomationAndUnsentFence(t *testing.T) {
	w := newAdmissionsWorld(t)
	mid := w.merchant()
	for range 25 {
		c := command(mid, "ignored")
		a, err := w.gate.AdmitAutonomous(t.Context(), c, "startup_catalog")
		require.NoError(t, err)
		_, err = w.gate.Start(t.Context(), mid, a.ID)
		require.NoError(t, err)
		require.NoError(t, w.gate.Finish(t.Context(), mid, a.ID, destructive.OutcomeSucceeded, "applied"))
	}
	_, err := w.gate.AdmitAutonomous(t.Context(), command(mid, "ignored"), "startup_catalog")
	h := held(t, err)
	require.Equal(t, "automation:"+mid.String(), h.ScopeKey)
	staff, err := w.gate.Admit(t.Context(), command(mid, "operator"))
	require.NoError(t, err, "automation spends no staff allowance")
	ctx := billingauth.BindStaff(t.Context(), billingauth.Staff{Merchant: billing.MerchantID(mid), Identity: authtest.User("staff")})
	_, err = w.gate.AdmitAutonomous(ctx, command(mid, "operator"), "startup_catalog")
	require.ErrorContains(t, err, "staff command")
	_, err = w.gate.Start(t.Context(), mid, staff.ID)
	require.NoError(t, err)
	for range 4 {
		_, err = w.gate.Admit(t.Context(), command(mid, "operator"))
		require.NoError(t, err)
	}
	_, err = w.gate.Admit(t.Context(), command(mid, "operator"))
	held(t, err)
	_, err = w.gate.StartPending(t.Context(), mid, staff.ID)
	require.ErrorIs(t, err, destructive.ErrHeld, "a proven unsent attempt cannot bypass a later hold")
	_, err = w.gate.Start(t.Context(), mid, staff.ID)
	require.NoError(t, err, "possible effects still retain recovery authority")
}

func TestDestructiveAdmissionPendingLookup(t *testing.T) {
	w := newAdmissionsWorld(t)
	mid := w.merchant()
	c := command(mid, "operator")
	c.Kind = destructive.ConfigurationChange
	a, err := w.gate.Admit(t.Context(), c)
	require.NoError(t, err)
	_, err = w.gate.Start(t.Context(), mid, a.ID)
	require.NoError(t, err)
	require.NoError(t, w.gate.Finish(t.Context(), mid, a.ID, destructive.OutcomeUncertain, "vault response lost"))
	previous, found, err := w.gate.FindPending(t.Context(), mid, c.Target, c.Actor, c.Digest)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, a.ID, previous.ID)
	other := c.Actor
	other.Issuer = "https://other.test"
	_, found, err = w.gate.FindPending(t.Context(), mid, c.Target, other, c.Digest)
	require.NoError(t, err)
	require.False(t, found)
}

// queryReceipt records the generated query for an EXPLAIN of that same SQL,
// while exercising its normal typed reader and schema rewriting first.
type queryReceipt struct {
	gen.DBTX
	sql  string
	args []any
}

func (q *queryReceipt) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	q.sql = sql
	q.args = args
	return q.DBTX.QueryRow(ctx, sql, args...)
}

func TestDestructiveAdmissionActivityThresholdPlan(t *testing.T) {
	w := newAdmissionsWorld(t)
	mid, other := w.merchant(), w.merchant()
	product, price := uuid.New(), uuid.New()
	w.exec(`INSERT INTO billing.products(merchant_id,id,key,display_name,tier_rank,archived) VALUES($1,$2,'scale','Scale',0,false)`, mid, product)
	w.exec(`INSERT INTO billing.prices(merchant_id,id,product_id,key,amount,currency,archived) VALUES($1,$2,$3,'scale',1000000,'USD',false)`, mid, price, product)
	w.exec(`INSERT INTO billing.customers(merchant_id,id) SELECT $1,md5(n::text)::uuid FROM generate_series(1,2000)n`, mid)
	w.exec(`INSERT INTO billing.payments(merchant_id,customer_id,price_id,channel,transaction_id,amount,list_amount,currency,status,purchased_at)
 SELECT $1,c.id,$2,'manual',c.id::text||':'||n::text,1000000,1000000,'USD','succeeded',now()-interval '1 day'
 FROM billing.customers c CROSS JOIN generate_series(1,2)n WHERE c.merchant_id=$1`, mid, price)
	w.exec(`ANALYZE billing.payments`)
	w.exec(`ANALYZE billing.merchants`)
	capture := &queryReceipt{DBTX: w.db.Qx(t.Context())}
	q := gen.New(capture)
	n, err := q.DestructiveMerchantActivityAtLeast(t.Context(), gen.DestructiveMerchantActivityAtLeastParams{MerchantID: mid, Since: w.clock.Now().Add(-30 * 24 * time.Hour), Required: 17})
	require.NoError(t, err)
	require.EqualValues(t, 17, n, "duplicates spend one membership and query stops at threshold")
	var plan string
	sql := "EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) " + strings.ReplaceAll(capture.sql, "billing.", w.schema+".")
	require.NoError(t, w.pool.QueryRow(t.Context(), sql, capture.args...).Scan(&plan))
	t.Log("merchant activity plan:", plan)
	require.Contains(t, plan, "Limit")
	require.Contains(t, plan, "payments_destructive_scale_idx", "qualify activity through its ordered partial index")
	n, err = q.DestructiveMerchantActivityAtLeast(t.Context(), gen.DestructiveMerchantActivityAtLeastParams{MerchantID: other, Since: w.clock.Now().Add(-30 * 24 * time.Hour), Required: 17})
	require.NoError(t, err)
	require.Zero(t, n, "another merchant cannot borrow active memberships")
	n, err = q.DestructiveDeploymentActivityAtLeast(t.Context(), gen.DestructiveDeploymentActivityAtLeastParams{Since: w.clock.Now().Add(-30 * 24 * time.Hour), Required: 17})
	require.NoError(t, err)
	require.EqualValues(t, 17, n)
	sql = "EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) " + strings.ReplaceAll(capture.sql, "billing.", w.schema+".")
	require.NoError(t, w.pool.QueryRow(t.Context(), sql, capture.args...).Scan(&plan))
	t.Log("deployment activity plan:", plan)
	require.Contains(t, plan, "payments_destructive_scale_idx")
	n, err = q.DestructiveMerchantActivityAtLeast(t.Context(), gen.DestructiveMerchantActivityAtLeastParams{MerchantID: mid, Since: w.clock.Now(), Required: 17})
	require.NoError(t, err)
	require.Zero(t, n, "old activity cannot scale a current allowance")
}
