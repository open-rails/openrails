//go:build integration

package riverjobs

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/stretchr/testify/require"
)

// Exercise the scheduled worker's ordinary full-mode
// producer after a method is attributed to another provider account.
func TestScheduledRebillRejectsMismatchedPSP(t *testing.T) {
	f := newDunningCertaintyFixture(t, 720, 2*time.Hour, true)
	other := uuid.New()
	require.NoError(t, f.dbi.RunInMerchantConn(f.ctx, func(ctx context.Context) error {
		qx := f.dbi.Qx(ctx)
		_, err := qx.Exec(ctx, `INSERT INTO openrails.psps (id,merchant_id,rail,environment,account_id,key,archived) VALUES ($1,$2,'nmi','test',$3,'review-809',true)`, other, dbtest.TestMerchantID.UUID(), "review-809-"+other.String())
		if err != nil {
			return err
		}
		_, err = qx.Exec(ctx, `UPDATE openrails.payment_methods SET psp_id=$2 WHERE id=(SELECT payment_method_id FROM openrails.subscriptions WHERE id=$1)`, f.subID, other)
		return err
	}))
	before := f.state(t)
	outcome := f.run(t)
	after := f.state(t)
	require.Equal(t, dunningOutcomeFailed, outcome)
	require.Zero(t, f.nmiWrites.Load(), "cross-PSP scheduled rebill must not contact the provider")
	require.Equal(t, before, after, "no claim or failure lifecycle should be recorded")
	// Restore the fixture's original attribution so its cleanup retains its
	// normal order and the extra provider account can be removed.
	require.NoError(t, f.dbi.RunInMerchantConn(f.ctx, func(ctx context.Context) error {
		qx := f.dbi.Qx(ctx)
		_, err := qx.Exec(ctx, `UPDATE openrails.payment_methods SET psp_id=(SELECT psp_id FROM openrails.subscriptions WHERE id=$1) WHERE id=(SELECT payment_method_id FROM openrails.subscriptions WHERE id=$1)`, f.subID)
		if err != nil {
			return err
		}
		_, err = qx.Exec(ctx, `DELETE FROM openrails.psps WHERE id=$1`, other)
		return err
	}))
}
