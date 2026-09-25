package vault

import (
	"context"
	"crypto/ed25519"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/vaultfake"
)

// Login never waits on Vault: operations answer ErrNotAuthenticated until the
// background login succeeds, then work; a later outage is ErrUnavailable.
func TestLoginAuthenticatesInTheBackground(t *testing.T) {
	t.Setenv("VAULT_MAX_RETRIES", "0")
	fake := vaultfake.New("root-token")
	t.Cleanup(fake.Close)
	fake.SetUp(false)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	start := time.Now()
	client, sup, err := Login(ctx, Config{Address: fake.URL(), Token: "root-token"})
	require.NoError(t, err)
	require.Less(t, time.Since(start), time.Second)
	require.ErrorIs(t, sup.AuthState(), ErrNotAuthenticated)
	transit := NewTransitAdapter(client, "transit").WithSupervisor(sup)
	_, err = transit.Sign(ctx, "k", []byte("msg"))
	require.ErrorIs(t, err, ErrUnavailable)
	require.ErrorIs(t, sup.Probe(ctx), ErrUnavailable)

	fake.SetUp(true)
	require.Eventually(t, func() bool { return sup.AuthState() == nil }, 20*time.Second, 20*time.Millisecond)
	sig, err := transit.Sign(ctx, "k", []byte("msg"))
	require.NoError(t, err)
	require.True(t, ed25519.Verify(fake.PublicKey("k"), []byte("msg"), sig))
	require.NoError(t, sup.Probe(ctx))

	fake.SetUp(false)
	_, err = transit.PublicKey(ctx, "k")
	require.ErrorIs(t, err, ErrUnavailable)
	require.ErrorIs(t, sup.Probe(ctx), ErrUnavailable)
}
