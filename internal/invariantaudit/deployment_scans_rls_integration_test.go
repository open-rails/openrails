//go:build integration

package invariantaudit

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/dbtest"
)

// Destructive-rate ceilings deliberately combine per-merchant and fleet-wide
// actor counts. Their truth must not depend on connection GUCs or RLS bypass.
func TestOR860_DestructiveRateCeilingCountsAcrossMerchants(t *testing.T) {
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

	// The platform scan sees both merchants without a session scope.
	var basePool int64
	require.NoError(t, app.QueryRow(ctx, `SELECT count(*) FROM billing.rail_intents
        WHERE origin IN ('user','admin') AND intent_type=ANY($1::text[])
        AND actor=$2 AND created_at>=now()-interval '1 hour'`, types, actor).Scan(&basePool))
	require.EqualValues(t, 6, basePool)

	// Per-merchant budgets remain isolated while the actor budget is global.
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
