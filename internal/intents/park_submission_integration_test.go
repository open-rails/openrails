//go:build integration

package intents

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

func TestParkRetainsSubmittedPaymentsAndSealedCompletion(t *testing.T) {
	// Superuser deliberately removes RLS as a backstop: predicates must enforce
	// ownership themselves, including the zero-row Park fallback.
	d := dbtest.OpenAppDB(t, dbtest.SharedSuperuserDSN(t))
	mid, psp := uuid.New(), uuid.New()
	ctx := merchant.WithID(t.Context(), merchant.ID(mid))
	_, err := d.Pool().Exec(ctx, `INSERT INTO billing.merchants(id,slug) VALUES($1,$2)`, mid, mid.String())
	require.NoError(t, err)
	_, err = d.Pool().Exec(ctx, `INSERT INTO billing.psps(id,merchant_id,rail,account_id) VALUES($1,$2,'nmi',$3)`, psp, mid, psp.String())
	require.NoError(t, err)
	store := NewStore(d)
	for _, tc := range []struct{ kind, evidence string }{
		{"invoice_collection", `{"submitted_at":"2026-09-21T00:00:00Z"}`},
		{"subscription_collection", `{"submitted_at":"2026-09-21T00:00:00Z"}`},
		{"nmi_sale", `{"sale_submitted":true}`},
		{"initial_membership", `{"initial_submitted":true}`},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			id := uuid.New()
			_, err := d.Pool().Exec(ctx, `INSERT INTO billing.rail_intents(id,merchant_id,psp_id,rail,intent_type,idempotency_key,status,origin,payload,result_evidence) VALUES($1,$2,$3,'nmi',$4,$1::uuid::text,'in_flight','system','{}',$5)`, id, mid, psp, tc.kind, tc.evidence)
			require.NoError(t, err)
			before, err := store.Get(ctx, id)
			require.NoError(t, err)
			other := merchant.WithID(t.Context(), merchant.ID(uuid.New()))
			require.NoError(t, store.Park(other, id, time.Now(), "blocked"))
			untouched, err := store.Get(ctx, id)
			require.NoError(t, err)
			require.Equal(t, StatusInFlight, untouched.Status)
			require.NoError(t, store.Park(ctx, uuid.New(), time.Now(), "missing"))
			require.NoError(t, store.Park(ctx, id, time.Now(), "mode blocked"))
			owned, err := store.Get(ctx, id)
			require.NoError(t, err)
			require.Equal(t, StatusUnknownNeedsVerify, owned.Status)
			require.Equal(t, before.ResultEvidence, owned.ResultEvidence)
			require.Equal(t, before.Payload, owned.Payload)
			require.Error(t, store.MarkFailedRetryable(ctx, id, time.Now(), "not a Stripe cancellation"))
			native, err := store.Get(ctx, id)
			require.NoError(t, err)
			require.Equal(t, StatusUnknownNeedsVerify, native.Status, "native submitted payments cannot become retryable")
			// A stale Park races a completion holding this row's lock. Observe the
			// database wait, then commit the seal before allowing Park to proceed.
			_, err = d.Pool().Exec(ctx, `UPDATE billing.rail_intents SET status='in_flight' WHERE id=$1`, id)
			require.NoError(t, err)
			tx, err := d.Pool().Begin(ctx)
			require.NoError(t, err)
			defer tx.Rollback(context.Background())
			var pid int
			require.NoError(t, tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid))
			_, err = tx.Exec(ctx, `UPDATE billing.rail_intents SET status='failed_terminal',result_evidence=result_evidence || '{"sealed":true}'::jsonb WHERE id=$1`, id)
			require.NoError(t, err)
			done := make(chan error, 1)
			go func() { done <- store.Park(ctx, id, time.Now(), "stale blocked result") }()
			require.Eventually(t, func() bool {
				var waiting bool
				err := d.Pool().QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, pid).Scan(&waiting)
				return err == nil && waiting
			}, 5*time.Second, 10*time.Millisecond)
			require.NoError(t, tx.Commit(ctx))
			require.NoError(t, <-done)
			sealed, err := store.Get(ctx, id)
			require.NoError(t, err)
			require.Equal(t, StatusFailedTerminal, sealed.Status)
			require.Contains(t, string(sealed.ResultEvidence), `"sealed": true`)
			require.Equal(t, before.Payload, sealed.Payload)
		})
	}
}
