//go:build integration

package checkout

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/merchant"
)

// #848: with TWO armed PSPs on the same rail, the wire PSP key must land the
// charge on THAT account's credentials (real Postgres psps rows + real
// merchants.Service — no fake resolvers), and the bare rail kind must refuse
// with both keys named instead of silently picking one.
func TestCheckoutResolvesSpecificPSPKeyAmongMultipleArmed(t *testing.T) {
	// A dedicated merchant: two armed nmi rows on the shared test merchant
	// would make bare-"nmi" resolution ambiguous for concurrently running
	// packages. Every handle below is pinned to it, so RLS enforces throughout.
	nano := time.Now().UnixNano()
	mid := merchant.ID(uuid.New())
	dbi := dbtest.OpenMerchantDB(t, mid.UUID())
	pool := dbi.Pool()
	slug := fmt.Sprintf("psp848-%d", nano)
	_, err := pool.Exec(context.Background(),
		`INSERT INTO openrails.merchants (id, slug, status) VALUES ($1, $2, 'active')`,
		mid.UUID(), slug)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM openrails.psps WHERE merchant_id = $1`, mid.UUID())
		_, _ = pool.Exec(context.Background(), `DELETE FROM openrails.merchant_secrets WHERE merchant_id = $1`, mid.UUID())
		_, _ = pool.Exec(context.Background(), `DELETE FROM openrails.merchants WHERE id = $1`, mid.UUID())
	})
	ctx := merchant.WithID(context.Background(), mid)

	dbp := db.WrapPool(pool, "")
	store, err := merchants.NewDBSecretStore(dbp)
	require.NoError(t, err)
	msvc, err := merchants.NewService(dbp, store, config.ExpectedProviderEnvironment(true))
	require.NoError(t, err)

	keyA := fmt.Sprintf("mobius-a-%d", nano)
	keyB := fmt.Sprintf("mobius-b-%d", nano)
	accountA := fmt.Sprintf("gw-a-%d", nano)
	accountB := fmt.Sprintf("gw-b-%d", nano)

	seed := func(key, accountID string) {
		_, err := pool.Exec(ctx, `
			INSERT INTO openrails.psps (merchant_id, rail, environment, account_id, key, archived, evidence)
			VALUES ($1::uuid, 'nmi', 'test', $2, $3, false, '{"source":"test_848"}'::jsonb)
		`, mid.String(), accountID, key)
		require.NoError(t, err)
	}
	seed(keyA, accountA)
	seed(keyB, accountB)

	putSecret := func(accountID, value string) {
		name, err := merchants.PSPSecretName("nmi", "test", accountID, "security_key")
		require.NoError(t, err)
		_, err = store.Put(ctx, mid, name, value)
		require.NoError(t, err)
	}
	putSecret(accountA, "sk-a-"+keyA)
	putSecret(accountB, "sk-b-"+keyB)

	svc := &CheckoutService{Config: &config.Config{ProviderWriteMode: config.ProviderWriteModeFull, TestMode: config.CredentialPostureSandbox}}
	svc.SetMerchantSecretStore(store)
	svc.SetRailMerchantAccountSecretResolver(msvc)

	// Each PSP key resolves ITS OWN security key — the charge lands on the
	// requested account, never a kind-level re-resolution.
	clientA, err := svc.resolveNMIClient(ctx, keyA)
	require.NoError(t, err)
	require.Equal(t, "sk-a-"+keyA, clientA.SecurityKey)

	clientB, err := svc.resolveNMIClient(ctx, keyB)
	require.NoError(t, err)
	require.Equal(t, "sk-b-"+keyB, clientB.SecurityKey)

	// The bare rail kind is ambiguous: refuse loudly, naming both keys.
	_, err = svc.resolveNMIClient(ctx, "nmi")
	var ambiguous *AmbiguousRailError
	require.ErrorAs(t, err, &ambiguous)
	require.Contains(t, ambiguous.Keys, keyA)
	require.Contains(t, ambiguous.Keys, keyB)

	// PSP-id provenance pins the exact row for each key.
	idA := svc.ResolvePSPID(ctx, keyA)
	idB := svc.ResolvePSPID(ctx, keyB)
	require.NotEqual(t, uuid.Nil, idA)
	require.NotEqual(t, uuid.Nil, idB)
	require.NotEqual(t, idA, idB)
}
