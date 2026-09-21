//go:build integration

package merchants

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrations/vault/vaulttest"
)

func TestMain(m *testing.M) {
	code := m.Run()
	dbtest.TerminateShared()
	vaulttest.TerminateShared()
	os.Exit(code)
}

// SEC-19: the live-PSP probe must give the SAME answer under the production
// posture (connected as openrails_app, which enforces RLS) as it does under a
// privileged role. openrails.psps ENABLEs + FORCEs RLS with a
// merchant-GUC policy, so the retired one-shot
// `SELECT EXISTS (... FROM openrails.psps WHERE environment='live')` on a
// no-GUC pool returned ZERO ROWS AND NO ERROR — reporting "no live accounts"
// precisely where it mattered and silently disarming the CCBill IP gate built
// on it. This test asserts both halves: the naive read still lies, and
// ProbeLiveRailPSPs tells the truth.
func TestProbeLiveRailPSPsUnderEnforcingRLS(t *testing.T) {
	ctx := context.Background()
	superDSN, appDSN := dbtest.SharedRLSPostgres(t)

	suffix := uuid.NewString()[:8]
	merchantID := uuid.NewString()

	superRaw, err := pgxpool.New(ctx, superDSN)
	require.NoError(t, err)
	defer superRaw.Close()
	super := db.WrapPool(superRaw, config.DefaultSchema)

	_, err = super.Exec(ctx,
		`INSERT INTO billing.merchants (id, slug) VALUES ($1::uuid, $2) ON CONFLICT (id) DO NOTHING`,
		merchantID, "sec19-"+suffix)
	require.NoError(t, err)

	appRaw, err := pgxpool.New(ctx, appDSN)
	require.NoError(t, err)
	defer appRaw.Close()
	appPool := db.WrapPool(appRaw, config.DefaultSchema)

	svc, err := NewService(appPool, nil, "live")
	require.NoError(t, err)

	// No live CCBill PSP anywhere yet — but "absent" is only reportable because
	// the directory read succeeded and every merchant was asked.
	got, err := svc.ProbeLiveRailPSPs(ctx, "ccbill")
	require.NoError(t, err)
	require.Equal(t, LiveRailAbsent, got)

	_, err = super.Exec(ctx,
		`INSERT INTO billing.psps (merchant_id, rail, environment, account_id, archived)
		 VALUES ($1::uuid, 'ccbill', 'live', $2, false)
		 ON CONFLICT (rail, environment, account_id) DO NOTHING`,
		merchantID, "945280-"+suffix)
	require.NoError(t, err)

	// An intentional platform scan no longer depends on RLS bypass or a GUC.
	var naive bool
	require.NoError(t, appPool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM billing.psps
			 WHERE rail = lower($1) AND environment = 'live'
		)`, "ccbill").Scan(&naive))
	require.True(t, naive, "the normal pool sees the declared live account without session scope")

	got, err = svc.ProbeLiveRailPSPs(ctx, "ccbill")
	require.NoError(t, err)
	require.Equal(t, LiveRailPresent, got, "the explicit platform probe must see the live PSP")
}
