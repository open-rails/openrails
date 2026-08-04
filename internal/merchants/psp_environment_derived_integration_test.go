//go:build integration

package merchants

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db"
)

func pspEnvironment(t *testing.T, svc *Service, accountID string) string {
	t.Helper()
	var env string
	require.NoError(t, svc.pool.QueryRow(context.Background(), `
		SELECT environment FROM openrails.psps WHERE account_id = $1
	`, accountID).Scan(&env))
	return env
}

// #882: the PSP environment is DERIVED from the deployment's test_mode. A
// caller that still sends `environment` is refused, not silently ignored —
// tolerating it would keep teaching a knob that no longer exists.
func TestUpsertPaymentProviderConfigRefusesDeclaredEnvironment(t *testing.T) {
	pool := newTestPool(t)
	svc, err := NewService(db.WrapPool(pool, ""), NewMemorySecretStore(), "live")
	require.NoError(t, err)

	ctx := context.Background()
	tn, _, err := svc.Provision(ctx, ProvisionRequest{Slug: "psp-env-882", PermissionGroupID: "group-psp-env-882"})
	require.NoError(t, err)

	// Even an AGREEING declaration is refused.
	for i, declared := range []string{"live", "test", "moon"} {
		_, err = svc.UpsertPaymentProviderConfig(ctx, tn.ID, "ccbill", UpsertPaymentProviderConfigRequest{
			LegacyEnvironment: declared,
			AccountID:         "99882" + string(rune('0'+i)) + "-0000",
			Credentials:       map[string]string{"salt": "salt-882"},
		})
		require.ErrorContains(t, err, "`environment` was removed (#882)", "declared environment=%s", declared)
	}

	var count int
	require.NoError(t, svc.pool.QueryRow(ctx, `
		SELECT count(*) FROM openrails.psps WHERE account_id LIKE '99882%'
	`).Scan(&count))
	require.Zero(t, count, "a refused arm must never persist the PSP row")
}

// The derived environment still lands on the psps row, in BOTH postures.
func TestUpsertPaymentProviderConfigDerivesEnvironmentFromTestMode(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	for _, tc := range []struct {
		posture string
		slug    string
		account string
	}{
		{posture: "live", slug: "psp-env-live-882", account: "888201-0000"},
		{posture: "test", slug: "psp-env-test-882", account: "888202-0000"},
	} {
		svc, err := NewService(db.WrapPool(pool, ""), NewMemorySecretStore(), tc.posture)
		require.NoError(t, err)
		tn, _, err := svc.Provision(ctx, ProvisionRequest{Slug: tc.slug, PermissionGroupID: "group-" + tc.slug})
		require.NoError(t, err)

		cfg, err := svc.UpsertPaymentProviderConfig(ctx, tn.ID, "ccbill", UpsertPaymentProviderConfigRequest{
			AccountID:   tc.account,
			Credentials: map[string]string{"salt": "salt-" + tc.posture},
		})
		require.NoError(t, err)
		require.Equal(t, tc.posture, cfg.Environment)
		require.Equal(t, tc.posture, pspEnvironment(t, svc, tc.account))

		// The scoped secret name carries the same derived environment segment,
		// so the credential is resolvable at request time.
		name, ok, err := svc.ActivePSPSecretName(ctx, tn.ID, "ccbill", tc.posture, "salt")
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, "psps/ccbill/"+tc.posture+"/"+tc.account+"/salt", name)
	}
}
