//go:build e2e && integration

package destructive_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/destructive"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/stretchr/testify/require"
)

// A callback rollback proves no effect. Retrying that operation after another
// command trips the actor must get a new admission and must not reuse Started.
func TestDestructiveLocalRollbackCannotBypassLaterHold(t *testing.T) {
	w := newAdmissionsWorld(t)
	mid := w.merchant()
	ctx := merchant.WithID(t.Context(), billing.MerchantID(mid))
	c := command(mid, "local")
	c.Kind = destructive.RevokeAccess
	failed := errors.New("forced local rollback")
	err := destructive.RunLocal(ctx, w.db, w.gate, c, func(ctx context.Context, _ pgx.Tx, d *db.DB) (bool, error) {
		_, err := d.Qx(ctx).Exec(ctx, `UPDATE billing.merchants SET slug='must-roll-back' WHERE id=$1`, mid)
		if err != nil {
			return false, err
		}
		return true, failed
	})
	require.ErrorIs(t, err, failed)
	for range 5 {
		w.done(command(mid, "local"))
	}
	_, err = w.gate.Admit(ctx, command(mid, "local"))
	held(t, err)
	invoked := false
	err = destructive.RunLocal(ctx, w.db, w.gate, c, func(context.Context, pgx.Tx, *db.DB) (bool, error) { invoked = true; return true, nil })
	held(t, err)
	require.False(t, invoked)
	var slug string
	require.NoError(t, w.db.Qx(ctx).QueryRow(ctx, `SELECT slug FROM billing.merchants WHERE id=$1`, mid).Scan(&slug))
	require.NotEqual(t, "must-roll-back", slug)
}
