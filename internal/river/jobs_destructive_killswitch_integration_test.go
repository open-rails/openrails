//go:build integration

package riverjobs

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
)

// #836: an operator watching a mass cancellation at 3am could not stop it
// without a deploy — provider_write_mode is config/env only, River's QueuePause
// is unwired, and the #679 volume breaker holds provider deletes but never the
// LOCAL cancel + revoke that actually takes access away. The switch is a single
// row: flipping it halts every destructive plane on every node at its next gate
// check, and flipping it back resumes them.
func TestKillSwitchHaltsAndResumesTheConvergeSweep(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)
	dbi := dbtest.OpenAppDB(t, dsn)
	merchantID := dbtest.TestMerchantID.UUID()
	baseCtx := dbtest.WithTestMerchant(context.Background())
	dbtest.EnsureTestMerchant(baseCtx, t, dbi.Pool())

	worker := ConvergeSweepWorker{DB: dbi}

	// seedStaleSession creates one piece of convergeable drift and returns its id.
	seedStaleSession := func(suffix string) (uuid.UUID, uuid.UUID, uuid.UUID) {
		productID, priceID, sessionID := uuid.New(), uuid.New(), uuid.New()
		require.NoError(t, dbi.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
			customer := dbtest.EnsureCustomerIDPgx(ctx, t, dbi.Qx(ctx), uuid.NewString())
			exec := func(sql string, args ...any) {
				_, err := dbi.Qx(ctx).Exec(ctx, sql, args...)
				require.NoError(t, err)
			}
			exec(`INSERT INTO openrails.products (id,key,display_name,tier_group,entitlements_spec,merchant_id) VALUES ($1,$2,$2,$3,'{}'::jsonb,$4)`,
				productID, "ks-prod-"+suffix, "ks-tier-"+suffix, merchantID)
			exec(`INSERT INTO openrails.prices (id,product_id,amount,currency,access_duration_hours,auto_renew,merchant_id) VALUES ($1,$2,999,'usd',720,true,$3)`,
				priceID, productID, merchantID)
			exec(`INSERT INTO openrails.checkout_sessions (id,price_id,mode,rail,status,amount,currency,expires_at,merchant_id,customer_id)
			      VALUES ($1,$2,'one_off','nmi','created',999,'usd',$3,$4,$5)`,
				sessionID, priceID, time.Now().Add(-time.Hour), merchantID, customer)
			return nil
		}))
		t.Cleanup(func() {
			_ = dbi.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
				_, _ = dbi.Qx(ctx).Exec(ctx, `DELETE FROM openrails.reconciliation_findings WHERE merchant_id=$1 AND subject_key=$2`, merchantID, "checkout_session:"+sessionID.String())
				_, _ = dbi.Qx(ctx).Exec(ctx, `DELETE FROM openrails.checkout_sessions WHERE id=$1`, sessionID)
				_, _ = dbi.Qx(ctx).Exec(ctx, `DELETE FROM openrails.prices WHERE id=$1`, priceID)
				_, _ = dbi.Qx(ctx).Exec(ctx, `DELETE FROM openrails.products WHERE id=$1`, productID)
				return nil
			})
		})
		return sessionID, priceID, productID
	}
	sessionStatus := func(id uuid.UUID) string {
		var status string
		require.NoError(t, dbi.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
			return dbi.Qx(ctx).QueryRow(ctx, `SELECT status FROM openrails.checkout_sessions WHERE id=$1`, id).Scan(&status)
		}))
		return status
	}

	// --- switch OFF (the shipped default) -> the sweep is an observer --------
	haltedSession, _, _ := seedStaleSession(uuid.NewString()[:8])
	dbtest.DisarmDestructiveActions(baseCtx, t, dbi.Pool())
	require.NoError(t, worker.Work(context.Background(), &river.Job[ConvergeSweepArgs]{}))
	require.Equal(t, "created", sessionStatus(haltedSession),
		"the kill switch is OFF: the sweep must converge nothing")

	// --- operator flips it on: ONE UPDATE, no deploy, no restart -------------
	dbtest.ArmDestructiveActions(baseCtx, t, dbi.Pool(), merchantID)
	require.NoError(t, worker.Work(context.Background(), &river.Job[ConvergeSweepArgs]{}))
	require.Equal(t, "expired", sessionStatus(haltedSession),
		"armed: the same sweep converges the same drift")

	// --- and back off mid-flight: the NEXT pass stops dead -------------------
	resumedSession, _, _ := seedStaleSession(uuid.NewString()[:8])
	dbtest.DisarmDestructiveActions(baseCtx, t, dbi.Pool())
	require.NoError(t, worker.Work(context.Background(), &river.Job[ConvergeSweepArgs]{}))
	require.Equal(t, "created", sessionStatus(resumedSession),
		"an operator can halt an in-progress convergence at its next gate check")

	dbtest.ArmDestructiveActions(baseCtx, t, dbi.Pool(), merchantID)
}

// #836: readonly mode never made the converge sweep an observer — it had no
// IsProviderReadOnly check at all, unlike the refresh and dunning workers.
func TestConvergeSweepHonorsReadonlyMode(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)
	dbi := dbtest.OpenAppDB(t, dsn)
	merchantID := dbtest.TestMerchantID.UUID()
	baseCtx := dbtest.WithTestMerchant(context.Background())
	dbtest.EnsureTestMerchant(baseCtx, t, dbi.Pool())
	dbtest.ArmDestructiveActions(baseCtx, t, dbi.Pool(), merchantID)

	suffix := uuid.NewString()[:8]
	productID, priceID, sessionID := uuid.New(), uuid.New(), uuid.New()
	require.NoError(t, dbi.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		customer := dbtest.EnsureCustomerIDPgx(ctx, t, dbi.Qx(ctx), uuid.NewString())
		exec := func(sql string, args ...any) {
			_, err := dbi.Qx(ctx).Exec(ctx, sql, args...)
			require.NoError(t, err)
		}
		exec(`INSERT INTO openrails.products (id,key,display_name,tier_group,entitlements_spec,merchant_id) VALUES ($1,$2,$2,$3,'{}'::jsonb,$4)`,
			productID, "ro-prod-"+suffix, "ro-tier-"+suffix, merchantID)
		exec(`INSERT INTO openrails.prices (id,product_id,amount,currency,access_duration_hours,auto_renew,merchant_id) VALUES ($1,$2,999,'usd',720,true,$3)`,
			priceID, productID, merchantID)
		exec(`INSERT INTO openrails.checkout_sessions (id,price_id,mode,rail,status,amount,currency,expires_at,merchant_id,customer_id)
		      VALUES ($1,$2,'one_off','nmi','created',999,'usd',$3,$4,$5)`,
			sessionID, priceID, time.Now().Add(-time.Hour), merchantID, customer)
		return nil
	}))
	t.Cleanup(func() {
		_ = dbi.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
			_, _ = dbi.Qx(ctx).Exec(ctx, `DELETE FROM openrails.reconciliation_findings WHERE merchant_id=$1 AND subject_key=$2`, merchantID, "checkout_session:"+sessionID.String())
			_, _ = dbi.Qx(ctx).Exec(ctx, `DELETE FROM openrails.checkout_sessions WHERE id=$1`, sessionID)
			_, _ = dbi.Qx(ctx).Exec(ctx, `DELETE FROM openrails.prices WHERE id=$1`, priceID)
			_, _ = dbi.Qx(ctx).Exec(ctx, `DELETE FROM openrails.products WHERE id=$1`, productID)
			return nil
		})
	})

	require.NoError(t, readonlySweepWorker(dbi).Work(context.Background(), &river.Job[ConvergeSweepArgs]{}))

	var status string
	require.NoError(t, dbi.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		return dbi.Qx(ctx).QueryRow(ctx, `SELECT status FROM openrails.checkout_sessions WHERE id=$1`, sessionID).Scan(&status)
	}))
	require.Equal(t, "created", status, "readonly mode must make the converge sweep a pure observer")
}

func readonlySweepWorker(dbi *db.DB) ConvergeSweepWorker {
	return ConvergeSweepWorker{DB: dbi, Config: &config.Config{ProviderWriteMode: config.ProviderWriteModeReadOnly}}
}
