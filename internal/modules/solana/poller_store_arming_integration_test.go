//go:build integration

package solana

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
	solanarpc "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/merchant"
)

// #728: the process-wide Solana poller arms its RPC PER MERCHANT per pass —
// merchant-store rail-account settings win, boot client fallback, malformed
// declared settings fail LOUD (pass skipped, never a wrong-plane client).

func newSolanaTestMerchant(t *testing.T, dbi *db.DB, slug string) merchant.ID {
	t.Helper()
	id := merchant.ID(uuid.New())
	_, err := dbi.Pool().Exec(context.Background(),
		`INSERT INTO openrails.merchants (id, slug, status) VALUES ($1, $2, 'active')`,
		id.UUID(), slug)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = dbi.Pool().Exec(context.Background(), `DELETE FROM openrails.rail_merchant_accounts WHERE merchant_id = $1`, id.UUID())
		_, _ = dbi.Pool().Exec(context.Background(), `DELETE FROM openrails.merchants WHERE id = $1`, id.UUID())
	})
	return id
}

func seedSolanaRailAccount(t *testing.T, dbi *db.DB, mid merchant.ID, accountID string, settings map[string]any) {
	t.Helper()
	evidence := map[string]any{"source": "test"}
	if len(settings) > 0 {
		evidence["settings"] = settings
	}
	raw, err := json.Marshal(evidence)
	require.NoError(t, err)
	_, err = dbi.Pool().Exec(context.Background(), `
		INSERT INTO openrails.rail_merchant_accounts (merchant_id, rail, environment, account_id, archived, evidence)
		VALUES ($1::uuid, 'solana', 'live', $2, false, $3::jsonb)
	`, mid.String(), accountID, raw)
	require.NoError(t, err)
}

func newSolanaMerchantsService(t *testing.T, dbi *db.DB) *merchants.Service {
	t.Helper()
	svc, err := merchants.NewService(dbi.DataPool(), merchants.NewMemorySecretStore(), "live")
	require.NoError(t, err)
	return svc
}

func TestMerchantRPCBuilder_StoreSettingsWin(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)
	dbi := dbtest.OpenAppDB(t, dsn)
	svc := newSolanaMerchantsService(t, dbi)
	sfx := uuid.NewString()[:8]
	mid := newSolanaTestMerchant(t, dbi, "sol-store-"+sfx)

	storeKey := "store-key-" + sfx
	seedSolanaRailAccount(t, dbi, mid, "WaLLet"+sfx, map[string]any{
		"rpc_provider": "helius",
		"rpc_api_key":  storeKey,
	})

	boot := solanarpc.NewRPCClientWithConfig(solanarpc.RPCClientConfig{RPCProvider: "public", Network: "mainnet", ReadOnly: true})
	b := &MerchantRPCBuilder{
		Config:      &config.Config{Env: "dev"},
		MerchantsFn: func() *merchants.Service { return svc },
		Boot:        boot,
	}

	client, err := b.Resolve(context.Background(), mid)
	require.NoError(t, err)
	require.NotNil(t, client)
	assert.NotSame(t, boot, client, "declared RPC settings build a store-armed client")
	assert.Contains(t, client.GetEndpoint(), "helius", "store rpc_provider wins")
	assert.Contains(t, client.GetEndpoint(), storeKey, "store rpc_api_key wins")
}

func TestMerchantRPCBuilder_BootFallbackWhenNoStoreRow(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)
	dbi := dbtest.OpenAppDB(t, dsn)
	svc := newSolanaMerchantsService(t, dbi)
	sfx := uuid.NewString()[:8]
	// Merchant declares NO solana account at all.
	mid := newSolanaTestMerchant(t, dbi, "sol-boot-"+sfx)

	boot := solanarpc.NewRPCClientWithConfig(solanarpc.RPCClientConfig{RPCProvider: "public", Network: "mainnet", ReadOnly: true})
	b := &MerchantRPCBuilder{
		Config:      &config.Config{Env: "dev"},
		MerchantsFn: func() *merchants.Service { return svc },
		Boot:        boot,
	}

	client, err := b.Resolve(context.Background(), mid)
	require.NoError(t, err)
	assert.Same(t, boot, client, "no declared account → boot-plane client")

	// A declared account WITHOUT rpc knobs also keeps the boot client.
	mid2 := newSolanaTestMerchant(t, dbi, "sol-plain-"+sfx)
	seedSolanaRailAccount(t, dbi, mid2, "WaLLet2"+sfx, map[string]any{"recipient_wallet": "W" + sfx})
	client2, err := b.Resolve(context.Background(), mid2)
	require.NoError(t, err)
	assert.Same(t, boot, client2, "declared account with no RPC knobs → boot client")
}

func TestMerchantRPCBuilder_MalformedSettingsFailLoud(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)
	dbi := dbtest.OpenAppDB(t, dsn)
	svc := newSolanaMerchantsService(t, dbi)
	sfx := uuid.NewString()[:8]
	mid := newSolanaTestMerchant(t, dbi, "sol-bad-"+sfx)
	seedSolanaRailAccount(t, dbi, mid, "WaLLet"+sfx, map[string]any{"rpc_provider": "bogus"})

	boot := solanarpc.NewRPCClientWithConfig(solanarpc.RPCClientConfig{RPCProvider: "public", Network: "mainnet", ReadOnly: true})
	b := &MerchantRPCBuilder{
		Config:      &config.Config{Env: "dev"},
		MerchantsFn: func() *merchants.Service { return svc },
		Boot:        boot,
	}

	client, err := b.Resolve(context.Background(), mid)
	require.Error(t, err, "malformed declared settings fail loud — never a silent fallback")
	assert.Nil(t, client)
}

// fakeSolanaRPC is a minimal JSON-RPC endpoint recording per-method hit counts.
type fakeSolanaRPC struct {
	mu    sync.Mutex
	calls map[string]int
}

func (f *fakeSolanaRPC) count(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[method]
}

func (f *fakeSolanaRPC) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ID     any    `json:"id"`
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &req)
		f.mu.Lock()
		if f.calls == nil {
			f.calls = map[string]int{}
		}
		f.calls[req.Method]++
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","result":[],"id":%v}`, jsonID(req.ID))
	}
}

func jsonID(id any) string {
	raw, err := json.Marshal(id)
	if err != nil || string(raw) == "null" {
		return "1"
	}
	return string(raw)
}

// TestSolanaPollerPass_PerMerchantStoreArming drives ONE poller pass over two
// merchants' pending references: the STORE-ONLY merchant's reference is polled
// against its store-armed RPC endpoint, the undeclared merchant's against the
// boot client — per merchant, per pass, RLS-scoped.
func TestSolanaPollerPass_PerMerchantStoreArming(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)
	dbi := dbtest.OpenAppDB(t, dsn)
	rdb := dbtest.NewSharedRedisClient(t)
	svc := newSolanaMerchantsService(t, dbi)
	sfx := uuid.NewString()[:8]

	storeFake := &fakeSolanaRPC{}
	storeSrv := httptest.NewServer(storeFake.handler())
	t.Cleanup(storeSrv.Close)
	bootFake := &fakeSolanaRPC{}
	bootSrv := httptest.NewServer(bootFake.handler())
	t.Cleanup(bootSrv.Close)

	// Merchant A: STORE-ONLY solana settings (rpc knobs declared on the account).
	midStore := newSolanaTestMerchant(t, dbi, "sol-pass-store-"+sfx)
	seedSolanaRailAccount(t, dbi, midStore, "WaLLetA"+sfx, map[string]any{
		"rpc_provider": "helius",
		"rpc_api_key":  "store-key-" + sfx,
	})
	// Merchant B: declares nothing → boot fallback.
	midBoot := newSolanaTestMerchant(t, dbi, "sol-pass-boot-"+sfx)

	cfg := &config.Config{Env: "dev"}
	bootClient := solanarpc.NewRPCClientWithConfig(solanarpc.RPCClientConfig{Endpoint: bootSrv.URL, Network: "mainnet"})
	builder := &MerchantRPCBuilder{
		Config:      cfg,
		MerchantsFn: func() *merchants.Service { return svc },
		Boot:        bootClient,
		// Test seam: store-armed clients hit the fake endpoint.
		Endpoint: storeSrv.URL,
	}

	paySvc := NewSolanaPayService(dbi, rdb, cfg, nil, nil, nil, nil, nil, nil)
	poller := &SolanaPayPoller{
		db:               dbi,
		redis:            rdb,
		cfg:              cfg,
		rpcBuilder:       builder,
		solanaPayService: paySvc,
		retryAfter:       map[string]time.Time{},
	}

	// One pending reference per merchant (session-backed, unexpired so the pass
	// needs no checkout-session read).
	seedPending := func(mid merchant.ID) string {
		ref, err := solanarpc.GenerateReference()
		require.NoError(t, err)
		mctx := merchant.WithID(context.Background(), mid)
		require.NoError(t, paySvc.storePendingPayment(mctx, ref, &PendingSolanaPayment{
			UserID:      uuid.NewString(),
			PriceID:     uuid.NewString(),
			SessionID:   uuid.NewString(),
			Amount:      1000000,
			Currency:    "usd",
			Token:       "USDC",
			TokenMint:   "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v",
			TokenAmount: 1000000,
			Recipient:   "11111111111111111111111111111112",
			CreatedAt:   time.Now(),
			ExpiresAt:   time.Now().Add(10 * time.Minute),
		}))
		t.Cleanup(func() { _ = paySvc.RemovePendingPayment(mctx, ref) })
		return ref
	}
	seedPending(midStore)
	seedPending(midBoot)

	// Grouping is merchant-attributed (#728).
	grouped, err := paySvc.PendingReferencesByMerchant(context.Background())
	require.NoError(t, err)
	require.Len(t, grouped[midStore], 1)
	require.Len(t, grouped[midBoot], 1)

	poller.pollPendingPayments(context.Background())

	assert.GreaterOrEqual(t, storeFake.count("getSignaturesForAddress"), 1,
		"store-only merchant's pass polls with the STORE-armed client")
	assert.GreaterOrEqual(t, bootFake.count("getSignaturesForAddress"), 1,
		"undeclared merchant's pass polls with the BOOT client")
	assert.Zero(t, storeFake.count(""), "no unparsed RPC bodies")
}
