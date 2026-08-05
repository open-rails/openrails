//go:build integration

package intents

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
)

// destructiveType is a stable destructive intent type for seeding (the ceiling
// consumes the WHOLE DestructiveIntentTypes() set, not any one member).
const ceilingTestType = TypeNMIPaymentMethodDelete

// seedCeilingMerchant inserts a bare merchant and registers cleanup of its
// rail_intents + findings + row.
func seedCeilingMerchant(t *testing.T, dbi *db.DB) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	pool := dbi.Pool()
	id := uuid.New()
	_, err := pool.Exec(ctx, `INSERT INTO openrails.merchants (id, slug, status) VALUES ($1, $2, 'active')`,
		id, "ceiling-"+uuid.NewString()[:8])
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.rail_intents WHERE merchant_id = $1`, id)
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.reconciliation_findings WHERE merchant_id = $1`, id)
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.merchants WHERE id = $1`, id)
	})
	return id
}

// insertCeilingIntent seeds one rail_intents row with an EXPLICIT created_at and
// actor/origin — the ceiling counts by created_at, so a future-dated window
// isolates the count from any other rows in the shared package database.
func insertCeilingIntent(t *testing.T, dbi *db.DB, merchantID uuid.UUID, actor string, origin Origin, createdAt time.Time) {
	t.Helper()
	var actorArg any
	if actor != "" {
		actorArg = actor
	}
	pool := dbtest.SharedMerchantPool(t, merchantID)
	pspID := dbtest.EnsureTestPSP(context.Background(), t, pool, merchantID, "mobius")
	_, err := pool.Exec(context.Background(),
		`INSERT INTO openrails.rail_intents
		   (merchant_id, rail, psp_id, intent_type, idempotency_key, status, origin, actor, next_attempt_at, created_at)
		 VALUES ($1, 'mobius', $2, $3, $4, 'pending', $5, $6, $7, $7)`,
		merchantID, pspID, ceilingTestType, ceilingTestType+":"+uuid.NewString(), string(origin), actorArg, createdAt.UTC())
	require.NoError(t, err)
}

func trippedFindingCount(t *testing.T, dbi *db.DB, merchantID uuid.UUID, findingType, subjectKey string) (int, string, string) {
	t.Helper()
	var n int
	var severity, status string
	err := dbi.Pool().QueryRow(context.Background(),
		`SELECT count(*), coalesce(max(severity),''), coalesce(max(status),'')
		   FROM openrails.reconciliation_findings
		  WHERE merchant_id = $1 AND finding_type = $2 AND subject_key = $3`,
		merchantID, findingType, subjectKey).Scan(&n, &severity, &status)
	require.NoError(t, err)
	return n, severity, status
}

// Per-actor ceiling trips at the 6th op in a rolling hour: 5 prior destructive
// user ops are the wall; the 6th is refused. Root is INCLUDED — the gate keys
// on the actor id with no role bypass, so a "root" principal trips identically.
func TestRateCeiling_PerActorTripsAtSixth(t *testing.T) {
	ctx := context.Background()
	merchant := seedCeilingMerchant(t, dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID()))
	dbi := dbtest.OpenMerchantDB(t, merchant)
	gate := NewRateCeiling(dbi)

	// Future window so the count sees ONLY the rows we seed.
	base := time.Now().UTC().Add(2 * time.Hour)
	actor := "root-" + uuid.NewString() // a "root/owner" principal — no exemption

	// 4 prior ops: the 5th op is still under the wall (allowed).
	for i := 0; i < 4; i++ {
		insertCeilingIntent(t, dbi, merchant, actor, OriginUser, base)
	}
	require.NoError(t, gate.Check(ctx, CheckParams{
		Actor: actor, MerchantID: merchant, IntentType: ceilingTestType, Origin: OriginUser,
	}, base), "5th op (4 prior) must be allowed")

	// 5 prior ops: the 6th trips the per-actor ceiling.
	insertCeilingIntent(t, dbi, merchant, actor, OriginUser, base)
	err := gate.Check(ctx, CheckParams{
		Actor: actor, MerchantID: merchant, IntentType: ceilingTestType, Origin: OriginUser,
	}, base)
	require.Error(t, err, "6th op must be refused")
	require.True(t, errors.Is(err, ErrRateCeilingTripped))
	var rce *RateCeilingError
	require.ErrorAs(t, err, &rce)
	assert.Equal(t, ceilingPerActor, rce.Ceiling)
	assert.EqualValues(t, 5, rce.ActorCount)

	// The trip raised the operator alert: a critical, requires_review finding.
	n, severity, status := trippedFindingCount(t, dbi, merchant, RateCeilingTrippedFindingType, "actor:"+actor)
	assert.Equal(t, 1, n, "trip must raise exactly one standing finding")
	assert.Equal(t, "critical", severity)
	assert.Equal(t, "requires_review", status)
}

// The merchant-wide anti-theft wall trips at the 16th op in ONE merchant's
// rolling hour — distinct actors so the per-actor ceiling never fires; only the
// merchant wall does. Every test in this file gets a FRESH merchant, so the
// count is independent by construction: no shared budget, no window juggling.
func TestRateCeiling_PerMerchantTripsAtSixteenth(t *testing.T) {
	ctx := context.Background()
	merchant := seedCeilingMerchant(t, dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID()))
	dbi := dbtest.OpenMerchantDB(t, merchant)
	gate := NewRateCeiling(dbi)
	base := time.Now().UTC().Add(5 * time.Hour)

	// 14 prior ops (distinct actors): the 15th is still under the wall.
	for i := 0; i < 14; i++ {
		insertCeilingIntent(t, dbi, merchant, fmt.Sprintf("g-%d-%s", i, uuid.NewString()), OriginUser, base)
	}
	require.NoError(t, gate.Check(ctx, CheckParams{
		Actor: "probe-" + uuid.NewString(), MerchantID: merchant, IntentType: ceilingTestType, Origin: OriginUser,
	}, base), "15th op (14 prior) must be allowed")

	// 15 prior ops: the 16th trips this merchant's ceiling.
	insertCeilingIntent(t, dbi, merchant, "g-14-"+uuid.NewString(), OriginUser, base)
	err := gate.Check(ctx, CheckParams{
		Actor: "probe-" + uuid.NewString(), MerchantID: merchant, IntentType: ceilingTestType, Origin: OriginUser,
	}, base)
	require.Error(t, err, "16th op must be refused")
	require.True(t, errors.Is(err, ErrRateCeilingTripped))
	var rce *RateCeilingError
	require.ErrorAs(t, err, &rce)
	assert.Equal(t, ceilingPerMerchant, rce.Ceiling)
	assert.EqualValues(t, 15, rce.MerchantCount)

	n, severity, _ := trippedFindingCount(t, dbi, merchant, RateCeilingTrippedFindingType, "merchant:"+merchant.String())
	assert.Equal(t, 1, n, "a merchant-wide trip must raise a merchant-keyed finding")
	assert.Equal(t, "critical", severity)
}

// or#887: the anti-theft ceiling is PER MERCHANT. One merchant exhausting its
// hourly destructive budget must not refuse another merchant's FIRST op — a
// deployment-wide budget makes merchant A's ordinary customer cancellations
// deny service to merchant B, which is cross-tenant DoS on a platform built for
// thousands of merchants.
func TestRateCeiling_OneMerchantsBurstDoesNotRefuseAnother(t *testing.T) {
	ctx := context.Background()
	root := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	loud := seedCeilingMerchant(t, root)
	quiet := seedCeilingMerchant(t, root)
	base := time.Now().UTC().Add(20 * time.Hour)

	// Merchant A burns its whole budget: distinct actors, so only the
	// merchant-wide wall is in play, never the per-actor one.
	loudDB := dbtest.OpenMerchantDB(t, loud)
	for i := 0; i < PerMerchantHourlyCeiling; i++ {
		insertCeilingIntent(t, loudDB, loud, fmt.Sprintf("loud-%d-%s", i, uuid.NewString()), OriginUser, base)
	}
	err := NewRateCeiling(loudDB).Check(ctx, CheckParams{
		Actor: "loud-probe-" + uuid.NewString(), MerchantID: loud, IntentType: ceilingTestType, Origin: OriginUser,
	}, base)
	require.Error(t, err, "the merchant that burned its budget is walled")
	require.True(t, errors.Is(err, ErrRateCeilingTripped))

	// Merchant B has posted NOTHING. Its first destructive op must be admitted.
	quietDB := dbtest.OpenMerchantDB(t, quiet)
	require.NoError(t, NewRateCeiling(quietDB).Check(ctx, CheckParams{
		Actor: "quiet-" + uuid.NewString(), MerchantID: quiet, IntentType: ceilingTestType, Origin: OriginUser,
	}, base), "a neighbour merchant's burst must never refuse this merchant's first destructive op")
}

// System-origin destructive ops do NOT burn the anti-theft budget — that budget
// is about a stolen credential, and no principal produced these.
func TestRateCeiling_SystemOriginDoesNotBurnTheAntiTheftBudget(t *testing.T) {
	ctx := context.Background()
	merchant := seedCeilingMerchant(t, dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID()))
	dbi := dbtest.OpenMerchantDB(t, merchant)
	gate := NewRateCeiling(dbi)
	base := time.Now().UTC().Add(8 * time.Hour)

	for i := 0; i < 20; i++ {
		insertCeilingIntent(t, dbi, merchant, "", OriginSystem, base)
	}
	// A user op sees a clean budget (system ops excluded).
	require.NoError(t, gate.Check(ctx, CheckParams{
		Actor: "u-" + uuid.NewString(), MerchantID: merchant, IntentType: ceilingTestType, Origin: OriginUser,
	}, base), "system-origin ops must not count against the anti-theft ceilings")
}

// or#842: but they are NOT ungated. Check returned nil for origin='system', so
// the ceiling was absent for exactly the paths that queue the most irreversible
// work with no human in the loop. The system wall is PER MERCHANT: one
// merchant's runaway convergence hits it; a large fleet of merchants each
// converging their own book does not.
func TestRateCeiling_SystemOriginWalledPerMerchant(t *testing.T) {
	ctx := context.Background()
	root := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	merchant := seedCeilingMerchant(t, root)
	neighbour := seedCeilingMerchant(t, root)
	dbi := dbtest.OpenMerchantDB(t, merchant)
	gate := NewRateCeiling(dbi)
	base := time.Now().UTC().Add(14 * time.Hour)

	// 49 automated destructive ops this hour: the 50th is still admitted.
	for i := 0; i < 49; i++ {
		insertCeilingIntent(t, dbi, merchant, "", OriginSystem, base)
	}
	require.NoError(t, gate.Check(ctx, CheckParams{
		MerchantID: merchant, IntentType: ceilingTestType, Origin: OriginSystem,
	}, base), "50th automated op (49 prior) must be admitted")

	// 50 prior: the 51st is refused, and nothing is queued.
	insertCeilingIntent(t, dbi, merchant, "", OriginSystem, base)
	err := gate.Check(ctx, CheckParams{
		MerchantID: merchant, IntentType: ceilingTestType, Origin: OriginSystem,
	}, base)
	require.Error(t, err, "the 51st automated destructive op must be refused")
	require.True(t, errors.Is(err, ErrRateCeilingTripped))
	var rce *RateCeilingError
	require.ErrorAs(t, err, &rce)
	assert.Equal(t, ceilingSystemMerchant, rce.Ceiling)
	assert.EqualValues(t, 50, rce.SystemMerchantCount)

	// The refusal reaches the operator queue, keyed on the merchant.
	n, severity, status := trippedFindingCount(t, dbi, merchant, RateCeilingTrippedFindingType, "system:"+merchant.String())
	assert.Equal(t, 1, n, "an automated trip must raise exactly one standing finding")
	assert.Equal(t, "critical", severity)
	assert.Equal(t, "requires_review", status)

	// Fleet scale: the neighbour merchant's automation is untouched by it. A
	// deployment-wide wall on system origin would have stopped this one too.
	require.NoError(t, gate.Check(ctx, CheckParams{
		MerchantID: neighbour, IntentType: ceilingTestType, Origin: OriginSystem,
	}, base), "one merchant hitting its wall must not stop every other merchant's dunning")
}

// Early warning on the automated leg: crossing 50% of the per-merchant ceiling
// raises the finding before the wall, so a runaway is visible while it builds.
func TestRateCeiling_SystemEarlyWarningFires(t *testing.T) {
	ctx := context.Background()
	merchant := seedCeilingMerchant(t, dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID()))
	dbi := dbtest.OpenMerchantDB(t, merchant)
	gate := NewRateCeiling(dbi)
	base := time.Now().UTC().Add(17 * time.Hour)
	subject := "system:" + merchant.String()

	for i := 0; i < 23; i++ {
		insertCeilingIntent(t, dbi, merchant, "", OriginSystem, base)
	}
	require.NoError(t, gate.Check(ctx, CheckParams{
		MerchantID: merchant, IntentType: ceilingTestType, Origin: OriginSystem,
	}, base))
	if n, _, _ := trippedFindingCount(t, dbi, merchant, RateCeilingWarningFindingType, subject); n != 0 {
		t.Fatalf("no warning expected below 50%%, got %d", n)
	}

	insertCeilingIntent(t, dbi, merchant, "", OriginSystem, base)
	require.NoError(t, gate.Check(ctx, CheckParams{
		MerchantID: merchant, IntentType: ceilingTestType, Origin: OriginSystem,
	}, base), "an op crossing 50%% is warned, not refused")

	n, severity, status := trippedFindingCount(t, dbi, merchant, RateCeilingWarningFindingType, subject)
	assert.Equal(t, 1, n, "the 50% crossing raises an early-warning finding")
	assert.Equal(t, "high", severity)
	assert.Equal(t, "requires_review", status)
}

// Early warning: an op that crosses 50% of a ceiling (but stays below the wall)
// raises a high-severity warning finding — before the wall, so operators can
// notice-and-rotate.
func TestRateCeiling_EarlyWarningFires(t *testing.T) {
	ctx := context.Background()
	merchant := seedCeilingMerchant(t, dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID()))
	dbi := dbtest.OpenMerchantDB(t, merchant)
	gate := NewRateCeiling(dbi)
	base := time.Now().UTC().Add(11 * time.Hour)
	actor := "w-" + uuid.NewString()

	// 1 prior op: the 2nd op (running total 2) is below 50% of 5 — no warning.
	insertCeilingIntent(t, dbi, merchant, actor, OriginUser, base)
	require.NoError(t, gate.Check(ctx, CheckParams{
		Actor: actor, MerchantID: merchant, IntentType: ceilingTestType, Origin: OriginUser,
	}, base))
	if n, _, _ := trippedFindingCount(t, dbi, merchant, RateCeilingWarningFindingType, "actor:"+actor); n != 0 {
		t.Fatalf("no warning expected below 50%%, got %d", n)
	}

	// 2 prior ops: the 3rd op (running total 3) crosses 50% of 5 — warning fires,
	// op still allowed.
	insertCeilingIntent(t, dbi, merchant, actor, OriginUser, base)
	require.NoError(t, gate.Check(ctx, CheckParams{
		Actor: actor, MerchantID: merchant, IntentType: ceilingTestType, Origin: OriginUser,
	}, base), "op crossing 50%% is warned, not refused")

	n, severity, status := trippedFindingCount(t, dbi, merchant, RateCeilingWarningFindingType, "actor:"+actor)
	assert.Equal(t, 1, n, "the 50% crossing raises an early-warning finding")
	assert.Equal(t, "high", severity)
	assert.Equal(t, "requires_review", status)
}

// The real chokepoint: Store.Enqueue refuses the 6th destructive user intent
// and CREATES NO ROW for it (fail-closed by construction — the write-ahead
// intent never exists, so the destructive op cannot happen).
func TestRateCeiling_EnqueueChokepointRefusesSixth(t *testing.T) {
	ctx := context.Background()
	// rail_intents is RLS-forced with a WITH CHECK on app.merchant_id, so the
	// handle must be pinned to the merchant whose rows this test writes — seeding
	// through a handle pinned to a DIFFERENT merchant makes every insert fail.
	// (merchants itself is RLS-exempt, hence the two-step.)
	bootstrap := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	merchant := seedCeilingMerchant(t, bootstrap)
	dbi := dbtest.OpenMerchantDB(t, merchant)
	pool := dbi.Pool()

	// No window scrubbing: the ceiling counts THIS merchant's rows and the
	// merchant is freshly seeded, so no sibling test can inflate it. It needed
	// scrubbing while the wall was deployment-wide — and under enforced RLS that
	// DELETE could not even reach another merchant's rows, which is precisely
	// the fleet-scale failure or#887 removed rather than papered over.
	gated := NewStoreGated(dbi, NewRateCeiling(dbi))
	actor := "cust-" + uuid.NewString()
	pspID := dbtest.EnsureTestPSP(ctx, t, pool, merchant, "mobius")

	enqueue := func(i int) error {
		subID := uuid.New()
		_, err := gated.Enqueue(ctx, EnqueueParams{
			MerchantID:     merchant,
			Provider:       "mobius",
			PspID:          pspID,
			IntentType:     ceilingTestType,
			SubscriptionID: &subID,
			IdempotencyKey: fmt.Sprintf("%s:%s:%d", ceilingTestType, actor, i),
			NextAttemptAt:  time.Now().UTC(),
			Origin:         OriginUser,
			Actor:          actor,
		})
		return err
	}

	for i := 0; i < 5; i++ {
		require.NoError(t, enqueue(i), "op %d must be admitted", i+1)
	}
	err := enqueue(5)
	require.Error(t, err, "the 6th destructive op must be refused")
	require.True(t, errors.Is(err, ErrRateCeilingTripped))

	var rows int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM openrails.rail_intents WHERE merchant_id = $1 AND actor = $2`,
		merchant, actor).Scan(&rows))
	assert.Equal(t, 5, rows, "exactly 5 rows created; the refused 6th wrote nothing")
}
