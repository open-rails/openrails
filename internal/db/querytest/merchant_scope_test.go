//go:build integration

package querytest

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/dbtest"
)

// SEC-18: the merchant-admin by-id surface must not rely on RLS as its ONLY
// control.
//
// GET /v1/merchant/findings/:id and POST /v1/merchant/findings/:id/resolve were
// `WHERE id = $1` with no merchant predicate anywhere in the application layer —
// and resolve EXECUTES cancel/refund/revoke/grant against whatever the finding
// names. RLS was the single control, and RLS is a property of the CONNECTED
// ROLE: the shipped compose ran DB_USERNAME=admin (superuser), and ENV unset
// used to read as development, which switched the boot-time RLS-role check off.
// In that posture merchant A's owner, holding a legitimate
// merchant:findings:resolve grant, could resolve merchant B's finding.
//
// This test runs on the SUPERUSER pool ON PURPOSE — RLS is disabled for it, so
// only the query's own predicate can hold the line. It asserts the boundary,
// not the policy.
func TestMerchantAdminByIDQueriesScopeToTheRequestMerchant(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.SharedSuperuserPGXPool(t, "querytest asserts the QUERY PREDICATE with RLS deliberately disabled (SEC-18) and TRUNCATE/ANALYZEs the perf corpus")

	var bypasses bool
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT COALESCE(bool_or(rolsuper OR rolbypassrls), FALSE)
		  FROM pg_roles WHERE rolname = current_user`).Scan(&bypasses))
	require.True(t, bypasses, "this test is only meaningful with RLS DISABLED — it proves the query predicate, not the policy")

	suffix := uuid.NewString()[:8]
	merchantA, merchantB := uuid.New(), uuid.New()
	for id, slug := range map[uuid.UUID]string{merchantA: "sec18-a-" + suffix, merchantB: "sec18-b-" + suffix} {
		_, err := pool.Exec(ctx,
			`INSERT INTO openrails.merchants (id, slug, status) VALUES ($1, $2, 'active')`, id, slug)
		require.NoError(t, err)
	}

	seed := gen.New(pool)
	newFinding := func(merchantID uuid.UUID) uuid.UUID {
		t.Helper()
		row, err := seed.UpsertReconciliationFinding(ctx, gen.UpsertReconciliationFindingParams{
			MerchantID:  merchantID,
			FindingType: "life.subscription.missing",
			SubjectKey:  "sub_" + suffix,
			Severity:    "high",
			Status:      "requires_review",
			Evidence:    []byte(`{"provider":"nmi"}`),
		})
		require.NoError(t, err)
		return row.ID
	}
	findingA, findingB := newFinding(merchantA), newFinding(merchantB)

	// A request pins its merchant on the connection (MerchantDBConnMW) whatever
	// the role is, so the GUC is the honest stand-in for "merchant A is calling".
	conn, err := pool.Acquire(ctx)
	require.NoError(t, err)
	defer conn.Release()
	_, err = conn.Exec(ctx, `SELECT set_config('app.merchant_id', $1, false)`, merchantA.String())
	require.NoError(t, err)
	q := gen.New(conn)

	t.Run("read is scoped", func(t *testing.T) {
		own, err := q.GetReconciliationFinding(ctx, findingA)
		require.NoError(t, err)
		require.Equal(t, merchantA, own.MerchantID)

		_, err = q.GetReconciliationFinding(ctx, findingB)
		require.ErrorIs(t, err, pgx.ErrNoRows, "merchant A must not read merchant B's finding even with RLS disabled")
	})

	t.Run("resolve, ignore and note-append are scoped", func(t *testing.T) {
		actor := "operator@example.test"
		notes := "cross-merchant attempt"

		n, err := q.AdminResolveReconciliationFinding(ctx, gen.AdminResolveReconciliationFindingParams{
			ID:            findingB,
			OperatorNotes: &notes,
			ResolvedBy:    &actor,
		})
		require.NoError(t, err)
		require.Zero(t, n, "resolving another merchant's finding must affect no rows — resolve executes cancel/refund/revoke/grant")

		n, err = q.AdminIgnoreReconciliationFinding(ctx, gen.AdminIgnoreReconciliationFindingParams{
			ID:            findingB,
			OperatorNotes: &notes,
			ResolvedBy:    &actor,
		})
		require.NoError(t, err)
		require.Zero(t, n, "ignoring another merchant's finding must affect no rows")

		n, err = q.AppendReconciliationFindingNotes(ctx, gen.AppendReconciliationFindingNotesParams{
			ID:   findingB,
			Note: notes,
		})
		require.NoError(t, err)
		require.Zero(t, n, "annotating another merchant's finding must affect no rows")

		// B is untouched, read back on the unscoped superuser handle.
		var status string
		var operatorNotes *string
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT status, operator_notes FROM openrails.reconciliation_findings WHERE id = $1`,
			findingB).Scan(&status, &operatorNotes))
		require.Equal(t, "requires_review", status)
		require.Nil(t, operatorNotes)

		// And the same statement still works for the caller's OWN finding.
		n, err = q.AdminResolveReconciliationFinding(ctx, gen.AdminResolveReconciliationFindingParams{
			ID:            findingA,
			OperatorNotes: &notes,
			ResolvedBy:    &actor,
		})
		require.NoError(t, err)
		require.EqualValues(t, 1, n, "the merchant predicate must not break the legitimate path")
	})

	t.Run("a request with no merchant pinned resolves nothing", func(t *testing.T) {
		bare, err := pool.Acquire(ctx)
		require.NoError(t, err)
		defer bare.Release()
		_, err = gen.New(bare).GetReconciliationFinding(ctx, findingB)
		require.ErrorIs(t, err, pgx.ErrNoRows, "no app.merchant_id ⇒ no by-id access; fail closed")
	})
}
