//go:build integration

package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/authkit"
	authcore "github.com/open-rails/authkit/embedded"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func TestCLICatalogNamespacesStaySeparate(t *testing.T) {
	_, appDSN := dbtest.SharedRLSPostgres(t)
	admin := dbtest.SharedSuperuserPGXPool(t)
	ctx := cliCmdContext(appDSN, "staging")
	core, err := authcore.New(authcore.Config{
		Keys:      authcore.KeysConfig{VerifyOnly: true},
		Token:     authcore.TokenConfig{Issuer: "https://catalog-cli.test/" + uuid.NewString(), IssuedAudiences: []string{"test"}},
		RBAC:      []authcore.PersonaDef{{Name: "merchant", Parent: authkit.RootPersona}},
		Ephemeral: authcore.EphemeralConfig{AllowMemory: true},
	}, authcore.Deps{Postgres: admin})
	require.NoError(t, err)
	require.NoError(t, core.SeedPermissionGroupContainment(ctx))
	_, err = core.EnsureRootGroup(ctx)
	require.NoError(t, err)

	name := "catalog-cli-" + uuid.NewString()[:8]
	unboundID, boundID := uuid.New(), uuid.New()
	_, err = admin.Exec(ctx, `INSERT INTO openrails.merchants (id, slug, status) VALUES ($1, $2, 'active')`, unboundID, name)
	require.NoError(t, err)
	manifestPath := filepath.Join(t.TempDir(), "catalog.yaml")
	writeManifest := func(title string) {
		t.Helper()
		raw := fmt.Sprintf(`version: 1
catalogs:
  - merchant: %s
    products:
      - key: base
        display_name: %s
        entitlements: [base]
        prices:
          - currency: usd
            unit_amount: 1000000
            duration: 30d
            auto_renew: true
`, name, title)
		require.NoError(t, os.WriteFile(manifestPath, []byte(raw), 0o600))
	}
	execute := func(cmd *cobra.Command, args ...string) (string, error) {
		t.Helper()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(args)
		cmd.SilenceErrors = true
		err := cmd.ExecuteContext(ctx)
		return out.String(), err
	}
	push := func(unbound bool) error {
		args := []string{"--file", manifestPath, "--insert"}
		if unbound {
			args = append(args, "--unbound-merchants")
		}
		_, err := execute(newPushCatalogCmd(), args...)
		return err
	}
	dump := func(unbound bool) (string, error) {
		args := []string{"--slug", name}
		if unbound {
			args = append(args, "--unbound-merchants")
		}
		return execute(newDumpCatalogCmd(), args...)
	}

	writeManifest("Host catalog")
	require.ErrorIs(t, push(false), merchants.ErrMerchantNotFound, "default AuthKit mode cannot fall back to a host-local name")
	_, err = dump(false)
	require.ErrorIs(t, err, merchants.ErrMerchantNotFound)
	require.NoError(t, push(true))
	hostDump, err := dump(true)
	require.NoError(t, err)
	require.Contains(t, hostDump, "Host catalog")

	// The same spelling in the AuthKit namespace owns a different billing row.
	group, err := core.CreatePermissionGroup(ctx, authkit.CreatePermissionGroupRequest{Persona: "merchant", InstanceSlug: name})
	require.NoError(t, err)
	_, err = admin.Exec(ctx, `INSERT INTO openrails.merchants (id, slug, status, permission_group_id) VALUES ($1, $2, 'active', $3)`, boundID, name, group)
	require.NoError(t, err)
	writeManifest("AuthKit catalog")
	require.NoError(t, push(false))
	boundDump, err := dump(false)
	require.NoError(t, err)
	require.Contains(t, boundDump, "AuthKit catalog")
	require.NotContains(t, boundDump, "Host catalog")
	hostDump, err = dump(true)
	require.NoError(t, err)
	require.Contains(t, hostDump, "Host catalog")
	require.NotContains(t, hostDump, "AuthKit catalog")
	for id, title := range map[uuid.UUID]string{unboundID: "Host catalog", boundID: "AuthKit catalog"} {
		var storedTitle string
		require.NoError(t, admin.QueryRow(context.Background(), `SELECT display_name FROM openrails.products WHERE merchant_id=$1 AND key='base'`, id).Scan(&storedTitle))
		require.Equal(t, title, storedTitle)
	}

	_, err = admin.Exec(ctx, `UPDATE openrails.merchants SET deleted_at=now() WHERE id=$1`, unboundID)
	require.NoError(t, err)
	require.ErrorIs(t, push(true), merchants.ErrMerchantNotFound, "unbound mode cannot fall back to a bound projection")
	_, err = dump(true)
	require.ErrorIs(t, err, merchants.ErrMerchantNotFound)
	_, err = dump(false)
	require.NoError(t, err, "retiring the unbound identity leaves the bound identity available")
}
