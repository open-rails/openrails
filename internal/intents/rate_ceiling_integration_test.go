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
const ceilingTestType = TypeNMIVaultDelete

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
	_, err := dbi.Pool().Exec(context.Background(),
		`INSERT INTO openrails.rail_intents
		   (merchant_id, rail, intent_type, idempotency_key, status, origin, actor, next_attempt_at, created_at)
		 VALUES ($1, 'mobius', $2, $3, 'pending', $4, $5, $6, $6)`,
		merchantID, ceilingTestType, ceilingTestType+":"+uuid.NewString(), string(origin), actorArg, createdAt.UTC())
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
	dbi := dbtest.OpenAppDB(t, dbtest.MerchantPinnedDSN(t, dbtest.TestMerchantID.UUID()))
	merchant := seedCeilingMerchant(t, dbi)
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

// Global ceiling trips at the 16th op across ALL actors/merchants in the hour —
// distinct actors so the per-actor ceiling never fires; the global wall does.
func TestRateCeiling_GlobalTripsAtSixteenth(t *testing.T) {
	ctx := context.Background()
	dbi := dbtest.OpenAppDB(t, dbtest.MerchantPinnedDSN(t, dbtest.TestMerchantID.UUID()))
	merchant := seedCeilingMerchant(t, dbi)
	gate := NewRateCeiling(dbi)
	base := time.Now().UTC().Add(2 * time.Hour)

	// 14 prior ops (distinct actors): the 15th is still under the global wall.
	for i := 0; i < 14; i++ {
		insertCeilingIntent(t, dbi, merchant, fmt.Sprintf("g-%d-%s", i, uuid.NewString()), OriginUser, base)
	}
	require.NoError(t, gate.Check(ctx, CheckParams{
		Actor: "probe-" + uuid.NewString(), MerchantID: merchant, IntentType: ceilingTestType, Origin: OriginUser,
	}, base), "15th op (14 prior) must be allowed")

	// 15 prior ops: the 16th trips the global ceiling.
	insertCeilingIntent(t, dbi, merchant, "g-14-"+uuid.NewString(), OriginUser, base)
	err := gate.Check(ctx, CheckParams{
		Actor: "probe-" + uuid.NewString(), MerchantID: merchant, IntentType: ceilingTestType, Origin: OriginUser,
	}, base)
	require.Error(t, err, "16th op must be refused")
	require.True(t, errors.Is(err, ErrRateCeilingTripped))
	var rce *RateCeilingError
	require.ErrorAs(t, err, &rce)
	assert.Equal(t, ceilingGlobal, rce.Ceiling)
	assert.EqualValues(t, 15, rce.GlobalCount)

	n, severity, _ := trippedFindingCount(t, dbi, merchant, RateCeilingTrippedFindingType, "global")
	assert.Equal(t, 1, n, "global trip must raise a global finding")
	assert.Equal(t, "critical", severity)
}

// System-origin destructive ops (automated dunning / decline cleanup) are
// #679's job and must NOT burn the anti-theft budget: 20 system ops in the
// window leave both ceilings untouched.
func TestRateCeiling_SystemOriginDoesNotCount(t *testing.T) {
	ctx := context.Background()
	dbi := dbtest.OpenAppDB(t, dbtest.MerchantPinnedDSN(t, dbtest.TestMerchantID.UUID()))
	merchant := seedCeilingMerchant(t, dbi)
	gate := NewRateCeiling(dbi)
	base := time.Now().UTC().Add(2 * time.Hour)

	for i := 0; i < 20; i++ {
		insertCeilingIntent(t, dbi, merchant, "", OriginSystem, base)
	}
	// A user op sees a clean budget (system ops excluded).
	require.NoError(t, gate.Check(ctx, CheckParams{
		Actor: "u-" + uuid.NewString(), MerchantID: merchant, IntentType: ceilingTestType, Origin: OriginUser,
	}, base), "system-origin ops must not count against the ceilings")

	// And a system op itself is never gated (inert for system origin).
	require.NoError(t, gate.Check(ctx, CheckParams{
		Actor: "", MerchantID: merchant, IntentType: ceilingTestType, Origin: OriginSystem,
	}, base))
}

// Early warning: an op that crosses 50% of a ceiling (but stays below the wall)
// raises a high-severity warning finding — before the wall, so operators can
// notice-and-rotate.
func TestRateCeiling_EarlyWarningFires(t *testing.T) {
	ctx := context.Background()
	dbi := dbtest.OpenAppDB(t, dbtest.MerchantPinnedDSN(t, dbtest.TestMerchantID.UUID()))
	merchant := seedCeilingMerchant(t, dbi)
	gate := NewRateCeiling(dbi)
	base := time.Now().UTC().Add(2 * time.Hour)
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
	dbi := dbtest.OpenAppDB(t, dbtest.MerchantPinnedDSN(t, dbtest.TestMerchantID.UUID()))
	pool := dbi.Pool()
	merchant := seedCeilingMerchant(t, dbi)

	// Clean the rolling window of any leftover user/admin destructive rows so the
	// global count cannot be inflated by earlier tests in this package.
	_, err := pool.Exec(ctx,
		`DELETE FROM openrails.rail_intents
		  WHERE origin IN ('user','admin') AND intent_type = ANY($1::text[])
		    AND created_at >= now() - interval '1 hour'`, DestructiveIntentTypes())
	require.NoError(t, err)

	gated := NewStoreGated(dbi, NewRateCeiling(dbi))
	actor := "cust-" + uuid.NewString()

	enqueue := func(i int) error {
		subID := uuid.New()
		_, err := gated.Enqueue(ctx, EnqueueParams{
			MerchantID:     merchant,
			Provider:       "mobius",
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
	err = enqueue(5)
	require.Error(t, err, "the 6th destructive op must be refused")
	require.True(t, errors.Is(err, ErrRateCeilingTripped))

	var rows int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM openrails.rail_intents WHERE merchant_id = $1 AND actor = $2`,
		merchant, actor).Scan(&rows))
	assert.Equal(t, 5, rows, "exactly 5 rows created; the refused 6th wrote nothing")
}
