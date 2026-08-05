//go:build integration

package reconcile

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/dbtest"
)

// seedTestPSPBinding declares one PSP for the test merchant and returns the
// binding a pull must now carry (or#893): the engine refuses a provider section
// with no PSP, because an unbound pass read the whole rail's mirror — every
// PSP's book at once — and wrote unattributed rows.
func seedTestPSPBinding(t *testing.T, appDB *db.DB, baseCtx context.Context, rail string) RailMerchantAccountBinding {
	t.Helper()
	return seedTestPSPBindingFor(t, appDB, baseCtx, dbtest.TestMerchantID.UUID(), rail)
}

// seedTestPSPBindingFor is seedTestPSPBinding for a test-owned merchant.
func seedTestPSPBindingFor(t *testing.T, appDB *db.DB, baseCtx context.Context, merchantID uuid.UUID, rail string) RailMerchantAccountBinding {
	t.Helper()
	accountID := rail + "-acct-" + uuid.NewString()[:8]
	var row gen.OpenrailsPsp
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		now := time.Now().UTC()
		var err error
		row, err = appDB.Gen(ctx).UpsertPSP(ctx, gen.UpsertPSPParams{
			MerchantID:     merchantID,
			Rail:           rail,
			AccountID:      accountID,
			LastVerifiedAt: &now,
		})
		return err
	}))
	t.Cleanup(func() {
		_ = appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.psps WHERE id=$1`, row.ID)
			return nil
		})
	})
	return RailMerchantAccountBinding{ID: row.ID, Rail: rail, AccountID: accountID}
}
