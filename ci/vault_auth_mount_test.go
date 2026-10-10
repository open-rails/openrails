//go:build e2e && integration

package ci_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	vaultapi "github.com/hashicorp/vault/api"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/vaulttest"
)

// An auth method mounted at the operator's own path (<owner>-<env>/approle):
// OpenRails logs in there with vault.auth_mount, and the default path does
// not serve it.
func TestVaultLogsInAtItsAuthMount(t *testing.T) {
	v := vaulttest.New(t)
	root, ctx := v.Client(), t.Context()
	mount := v.Mount + "-prod/approle"
	require.NoError(t, root.Sys().EnableAuthWithOptionsWithContext(ctx, mount, &vaultapi.EnableAuthOptions{Type: "approle"}))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		require.NoError(t, root.Sys().DisableAuthWithContext(ctx, mount))
	})
	_, err := root.Logical().WriteWithContext(ctx, "auth/"+mount+"/role/openrails", map[string]any{"token_ttl": "1h"})
	require.NoError(t, err)
	role, err := root.Logical().ReadWithContext(ctx, "auth/"+mount+"/role/openrails/role-id")
	require.NoError(t, err)
	secret, err := root.Logical().WriteWithContext(ctx, "auth/"+mount+"/role/openrails/secret-id", nil)
	require.NoError(t, err)
	roleID, secretID := role.Data["role_id"].(string), secret.Data["secret_id"].(string)

	f := newFixture(t)
	connect := func(authMount string) (*openrails.Client, error) {
		cfg := f.config()
		cfg.Merchant = openrails.MerchantDeclaration{Slug: "vault-" + uuid.NewString()[:8], DisplayName: "Vault"}
		cfg.Vault = &openrails.VaultConfig{Address: v.Addr, AuthMethod: "approle", RoleID: roleID, SecretID: secretID, AuthMount: authMount}
		client, err := openrails.New(t.Context(), cfg, openrails.Deps{Postgres: f.pool})
		if err == nil {
			t.Cleanup(func() { require.NoError(t, client.Close(context.Background())) })
		}
		return client, err
	}

	at, err := connect(mount)
	require.NoError(t, err)
	require.Eventually(t, func() bool { return probe(t, at, "openrails_vault") == nil }, 20*time.Second, 50*time.Millisecond, "logged in at auth/"+mount)

	elsewhere, err := connect("")
	require.NoError(t, err)
	require.Never(t, func() bool { return probe(t, elsewhere, "openrails_vault") == nil }, 2*time.Second, 100*time.Millisecond, "auth/approle is not this Vault's approle")

	for authMount, want := range map[string]string{
		"auth/" + mount:     "the path after auth/",
		v.Mount + "/../sys": "plain Vault path",
	} {
		_, err := connect(authMount)
		require.ErrorContains(t, err, want)
	}
}
