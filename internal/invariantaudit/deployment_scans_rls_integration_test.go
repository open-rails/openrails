//go:build integration

package invariantaudit

import (
	stdcontext "context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// or#860: the #732 destructive-rate ceiling is a FAIL-OPEN control until this
// passes. It counted destructive intents on the base pool, which is not a
// privileged pool — it is the same openrails_app role with no app.merchant_id,
// so rail_intents' FORCEd RLS matched `merchant_id = NULL`, the count was
// always 0, and the ceiling could never be exceeded.
//
// This test asserts BOTH halves on the enforcing role: the retired base-pool
// shape still lies (that is the regression pin — it is what a superuser-backed
// harness can never show), and migration 0021's SECURITY DEFINER reader tells
// the truth across merchants.
func TestOR860_DestructiveRateCeilingCountsAcrossMerchantsUnderRLS(t *testing.T) {
	ctx, super, app := pools(t)

	suffix := uuid.NewString()[:8]
	actor := "or860-actor-" + suffix + "@example.com"
	intentType := "nmi_delete_subscription"

	// Two merchants, three destructive intents each: the credential-theft shape
	// the ceiling exists to catch, where no single merchant looks alarming.
	var merchants []uuid.UUID
	for i := 0; i < 2; i++ {
		id := uuid.New()
		merchants = append(merchants, id)
		_, err := super.Exec(ctx,
			`INSERT INTO openrails.merchants (id, slug, status) VALUES ($1, $2, 'active')`,
			id, "or860-"+suffix+"-"+uuid.NewString()[:6])
		require.NoError(t, err)
		for j := 0; j < 3; j++ {
			_, err = super.Exec(ctx, `
				INSERT INTO openrails.rail_intents
				  (merchant_id, rail, intent_type, idempotency_key, origin, actor, status, next_attempt_at)
				VALUES ($1,'nmi',$2,$3,'user',$4,'pending', now())`,
				id, intentType, "or860-"+suffix+"-"+uuid.NewString(), actor)
			require.NoError(t, err)
		}
	}
	_ = merchants

	types := []string{intentType}

	// The retired shape: a GUC-less count on the app role. Zero, no error.
	var basePool int64
	require.NoError(t, app.QueryRow(ctx, `
		SELECT count(*) FROM openrails.rail_intents
		 WHERE origin IN ('user','admin') AND intent_type = ANY($1::text[])
		   AND actor = $2 AND created_at >= now() - interval '1 hour'`,
		types, actor).Scan(&basePool))
	require.EqualValues(t, 0, basePool,
		"if this ever becomes non-zero the base pool has gained RLS bypass — re-read or#824 before 'fixing' this test")

	// The fix: the definer reader sees all six.
	var global, byActor int64
	require.NoError(t, app.QueryRow(ctx,
		`SELECT openrails.count_destructive_intents_since($1::text[], now() - interval '1 hour')`,
		types).Scan(&global))
	require.GreaterOrEqual(t, global, int64(6),
		"or#860: the deployment-wide destructive count must span merchants, or the ceiling never trips")

	require.NoError(t, app.QueryRow(ctx,
		`SELECT openrails.count_destructive_intents_by_actor_since($1, $2::text[], now() - interval '1 hour')`,
		actor, types).Scan(&byActor))
	require.EqualValues(t, 6, byActor,
		"or#860: one actor operating across two merchants must be visible as one actor")
}

// or#861: the alert evaluator and the #787 findings digest selected their work
// set on the base pool, so both had never run in production. Same proof shape.
func TestOR861_ArmedMerchantScansSeeMerchantsUnderRLS(t *testing.T) {
	ctx, super, app := pools(t)

	suffix := uuid.NewString()[:8]
	var seeded []uuid.UUID
	for i := 0; i < 2; i++ {
		id := uuid.New()
		seeded = append(seeded, id)
		_, err := super.Exec(ctx,
			`INSERT INTO openrails.merchants (id, slug, status) VALUES ($1, $2, 'active')`,
			id, "or861-"+suffix+"-"+uuid.NewString()[:6])
		require.NoError(t, err)
		_, err = super.Exec(ctx,
			`INSERT INTO openrails.alert_rules (merchant_id, name, template, enabled)
			 VALUES ($1, $2, 'payment_failure_rate', true)`, id, "or861-rule-"+suffix)
		require.NoError(t, err)
	}

	var basePool int64
	require.NoError(t, app.QueryRow(ctx,
		`SELECT count(*) FROM (SELECT DISTINCT merchant_id FROM openrails.alert_rules WHERE enabled) x`).
		Scan(&basePool))
	require.EqualValues(t, 0, basePool,
		"the retired base-pool armed scan must still see nothing — that silence is the whole defect")

	armed := map[uuid.UUID]bool{}
	rows, err := app.Query(ctx, `SELECT merchant_id FROM openrails.armed_alert_merchant_ids()`)
	require.NoError(t, err)
	for rows.Next() {
		var id uuid.UUID
		require.NoError(t, rows.Scan(&id))
		armed[id] = true
	}
	rows.Close()
	require.NoError(t, rows.Err())
	for _, id := range seeded {
		require.Truef(t, armed[id], "or#861: armed merchant %s missing — the evaluator would skip it", id)
	}
}

// The cross-merchant readers must RAISE, not return an empty set, when their
// definer cannot bypass RLS. That is the property that stops this whole class
// of defect from ever being silent again (0016's contract, extended by 0021).
func TestCrossMerchantReadersRefuseAWeakDefiner(t *testing.T) {
	ctx, super, app := pools(t)

	role := "or860_weak_" + uuid.NewString()[:8]
	_, err := super.Exec(ctx, `CREATE ROLE `+role+` NOLOGIN`)
	require.NoError(t, err)
	t.Cleanup(func() {
		bg := stdcontext.Background()
		_, _ = super.Exec(bg, `ALTER FUNCTION openrails.count_destructive_intents_since(text[], timestamptz) OWNER TO CURRENT_USER`)
		_, _ = super.Exec(bg, `DROP ROLE IF EXISTS `+role)
	})
	_, err = super.Exec(ctx, `GRANT USAGE ON SCHEMA openrails TO `+role)
	require.NoError(t, err)
	_, err = super.Exec(ctx,
		`ALTER FUNCTION openrails.count_destructive_intents_since(text[], timestamptz) OWNER TO `+role)
	require.NoError(t, err)

	var n int64
	err = app.QueryRow(ctx,
		`SELECT openrails.count_destructive_intents_since(ARRAY['nmi_delete_subscription']::text[], now() - interval '1 hour')`).
		Scan(&n)
	require.Error(t, err,
		"a definer that cannot bypass RLS must RAISE — returning 0 is how the ceiling silently stopped protecting anything")
}
