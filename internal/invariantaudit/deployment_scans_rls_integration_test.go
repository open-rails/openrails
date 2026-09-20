//go:build integration

package invariantaudit

import (
	stdcontext "context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/dbtest"
)

// or#860: the #732 destructive-rate ceiling is a FAIL-OPEN control until this
// passes. It counted destructive intents on the base pool, which is not a
// privileged pool — it is the same openrails_app role with no app.merchant_id,
// so rail_intents' FORCEd RLS matched `merchant_id = NULL`, the count was
// always 0, and the ceiling could never be exceeded.
//
// This test asserts BOTH halves on the enforcing role: the retired base-pool
// shape still lies (that is the regression pin — it is what a superuser-backed
// harness can never show), and the SECURITY DEFINER readers the ceiling
// ACTUALLY calls tell the truth.
//
// or#887 narrowed which truth that is. The ceiling's budget is PER-MERCHANT —
// a shared deployment-wide wall let one busy tenant deny service to every
// other — so the readers under test are the per-merchant leg and the per-actor
// leg. The anti-theft property survives the narrowing: a forged-identity burst
// is still walled inside the merchant it targets, and one credential operating
// across merchants is still visible as ONE actor, which is what the second
// assertion below pins.
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
			`INSERT INTO billing.merchants (id, slug, status) VALUES ($1, $2, 'active')`,
			id, "or860-"+suffix+"-"+uuid.NewString()[:6])
		require.NoError(t, err)
		pspID := dbtest.EnsureTestPSP(ctx, t, super, id, "nmi")
		for j := 0; j < 3; j++ {
			_, err = super.Exec(ctx, `
				INSERT INTO billing.rail_intents
				  (merchant_id, rail, psp_id, intent_type, idempotency_key, origin, actor, status, next_attempt_at)
				VALUES ($1,'nmi',$2,$3,$4,'user',$5,'pending', now())`,
				id, pspID, intentType, "or860-"+suffix+"-"+uuid.NewString(), actor)
			require.NoError(t, err)
		}
	}
	types := []string{intentType}
	origins := []string{"user", "admin"}

	// The retired shape: a GUC-less count on the app role. Zero, no error.
	var basePool int64
	require.NoError(t, app.QueryRow(ctx, `
		SELECT count(*) FROM billing.rail_intents
		 WHERE origin IN ('user','admin') AND intent_type = ANY($1::text[])
		   AND actor = $2 AND created_at >= now() - interval '1 hour'`,
		types, actor).Scan(&basePool))
	require.EqualValues(t, 0, basePool,
		"if this ever becomes non-zero the base pool has gained RLS bypass — re-read or#824 before 'fixing' this test")

	// The fix, leg 1 (or#887): the per-merchant definer reader — the one the
	// ceiling actually calls — sees each merchant's own three, from a pool that
	// carries no app.merchant_id and would otherwise have counted zero.
	for _, mid := range merchants {
		var perMerchant int64
		require.NoError(t, app.QueryRow(ctx,
			`SELECT billing.count_destructive_intents_for_merchant_since($1, $2::text[], $3::text[], now() - interval '1 hour')`,
			mid, origins, types).Scan(&perMerchant))
		require.EqualValues(t, 3, perMerchant,
			"or#887: the ceiling's per-merchant count must see the merchant's own destructive intents, or the wall never trips")
	}

	// Leg 2: the per-actor reader still spans merchants. This is the anti-theft
	// half — the budget is per-merchant, but ONE stolen credential must not be
	// six invisible ones.
	var byActor int64
	require.NoError(t, app.QueryRow(ctx,
		`SELECT billing.count_destructive_intents_by_actor_since($1, $2::text[], now() - interval '1 hour')`,
		actor, types).Scan(&byActor))
	require.EqualValues(t, 6, byActor,
		"or#860: one actor operating across two merchants must be visible as one actor")
}

// The cross-merchant readers must RAISE, not return an empty set, when their
// definer cannot bypass RLS. That is the property that stops this whole class
// of defect from ever being silent again (0016's contract, extended by 0021).
func TestCrossMerchantReadersRefuseAWeakDefiner(t *testing.T) {
	ctx, super, app := pools(t)

	const fn = `billing.count_destructive_intents_for_merchant_since(uuid, text[], text[], timestamptz)`

	role := "or860_weak_" + uuid.NewString()[:8]
	_, err := super.Exec(ctx, `CREATE ROLE `+role+` NOLOGIN`)
	require.NoError(t, err)
	t.Cleanup(func() {
		bg := stdcontext.Background()
		_, _ = super.Exec(bg, `ALTER FUNCTION `+fn+` OWNER TO CURRENT_USER`)
		_, _ = super.Exec(bg, `DROP ROLE IF EXISTS `+role)
	})
	_, err = super.Exec(ctx, `GRANT USAGE ON SCHEMA billing TO `+role)
	require.NoError(t, err)
	_, err = super.Exec(ctx, `ALTER FUNCTION `+fn+` OWNER TO `+role)
	require.NoError(t, err)

	var n int64
	err = app.QueryRow(ctx,
		`SELECT billing.count_destructive_intents_for_merchant_since($1, ARRAY['user','admin']::text[], ARRAY['nmi_delete_subscription']::text[], now() - interval '1 hour')`,
		uuid.New()).
		Scan(&n)
	require.Error(t, err,
		"a definer that cannot bypass RLS must RAISE — returning 0 is how the ceiling silently stopped protecting anything")
}
