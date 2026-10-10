//go:build e2e && integration

package ci_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/vaulttest"
)

// The dev Vault gives each test its own KV v2 mount: documents read back at
// their version, a stale check-and-set is refused, another test's mount never
// sees them, and a mount is gone when its test ends.
func TestVaultHarness(t *testing.T) {
	t.Parallel()
	v, other := vaulttest.New(t), vaulttest.New(t)
	type merchant struct {
		DisplayName string            `json:"display_name"`
		Settings    map[string]string `json:"settings"`
	}
	const path = "merchants/acme/merchant"

	first := v.Put(t, path, merchant{DisplayName: "Acme", Settings: map[string]string{"locale": "en"}})
	var got merchant
	require.Equal(t, first, v.Get(t, path, &got))
	require.Equal(t, merchant{DisplayName: "Acme", Settings: map[string]string{"locale": "en"}}, got)
	require.Zero(t, other.Get(t, path, &got), "another test's mount holds nothing")

	second, err := v.PutCAS(t, path, merchant{DisplayName: "Acme Inc"}, first)
	require.NoError(t, err)
	require.Equal(t, first+1, second)
	_, err = v.PutCAS(t, path, merchant{DisplayName: "stale"}, first)
	require.ErrorIs(t, err, vaulttest.ErrCASMismatch)
	got = merchant{}
	require.Equal(t, second, v.Get(t, path, &got))
	require.Equal(t, "Acme Inc", got.DisplayName, "the stale edit changed nothing")

	psp := "merchants/acme/psps/mobius"
	created, err := v.PutCAS(t, psp, map[string]any{"rail": "nmi", "account_id": "1234"}, 0)
	require.NoError(t, err)
	require.Equal(t, 1, created)
	_, err = v.PutCAS(t, psp, map[string]any{"rail": "nmi", "account_id": "5678"}, 0)
	require.ErrorIs(t, err, vaulttest.ErrCASMismatch, "version 0 creates only")

	var mount string
	t.Run("cleanup", func(t *testing.T) { mount = vaulttest.New(t).Mount })
	mounts, err := v.Client().Sys().ListMountsWithContext(t.Context())
	require.NoError(t, err)
	require.Contains(t, mounts, v.Mount+"/")
	require.NotContains(t, mounts, mount+"/", "a test's mount is removed when it ends")
}
