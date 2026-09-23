//go:build integration

package riverjobs

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/stretchr/testify/require"
)

func TestDunningWorker_MaterializeRecordsParkedIntent(t *testing.T) {
	f := newDunningCertaintyFixture(t, 720, 24*time.Hour, true)
	require.NoError(t, f.dbi.RunInMerchantConn(f.ctx, func(ctx context.Context) error {
		sub, err := f.subSvc.GetByID(ctx, f.subID)
		require.NoError(t, err)
		// Repeated limited-mode scans persist one bounded decision and never charge.
		for range 2 {
			outcome, err := f.worker.processSubscription(ctx, sub, f.lifecycle, f.priceSvc, true)
			require.NoError(t, err)
			require.Equal(t, dunningOutcomeMaterialized, outcome)
		}
		var count int
		require.NoError(t, f.dbi.Qx(ctx).QueryRow(ctx,
			`SELECT count(*) FROM billing.rail_intents WHERE subscription_id = $1`, f.subID).Scan(&count))
		require.Equal(t, 1, count)
		var status, origin, kind string
		var pspID uuid.UUID
		var expiresAt *time.Time
		require.NoError(t, f.dbi.Qx(ctx).QueryRow(ctx,
			`SELECT status, origin, intent_type, psp_id, expires_at FROM billing.rail_intents WHERE subscription_id = $1`, f.subID).
			Scan(&status, &origin, &kind, &pspID, &expiresAt))
		require.Equal(t, intents.StatusPending, status)
		require.Equal(t, string(intents.OriginSystem), origin)
		require.Equal(t, subscriptions.TypeManualRebill, kind)
		require.Equal(t, sub.PspID, pspID)
		require.NotNil(t, expiresAt)
		return nil
	}))
	require.Zero(t, f.nmiWrites.Load())
	state := f.state(t)
	require.Equal(t, "past_due", state.status)
	require.Nil(t, state.lastRetryAt)
}
