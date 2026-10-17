package solana

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestBalanceCacheExpiresAndRecoversWithoutStaleSuccess(t *testing.T) {
	var failed atomic.Bool
	stub, url := newRPCStub(t, func(method string, _ int, _ []json.RawMessage) any {
		if method == "getGenesisHash" {
			return devnetGenesisHash
		}
		if failed.Load() {
			return rpcFault{Code: -32000, Message: "temporarily unavailable"}
		}
		return map[string]any{"context": map[string]any{"slot": 1}, "value": uint64(80_000_000)}
	})
	client := NewRPCClientWithConfig(RPCClientConfig{Network: "devnet", PublicURL: url})
	mid, psp := uuid.New(), uuid.New()
	wallet := solanago.NewWallet().PublicKey()
	now := time.Now()
	var cache BalanceCache
	balance, err := cache.Read(t.Context(), client, mid, psp, wallet, now)
	require.NoError(t, err)
	require.Equal(t, uint64(80_000_000), balance)
	failed.Store(true)
	_, err = cache.Read(t.Context(), client, mid, psp, wallet, now.Add(time.Minute))
	require.NoError(t, err)
	require.Equal(t, 1, stub.count("getBalance"))
	_, err = cache.Read(t.Context(), client, mid, psp, wallet, now.Add(BalanceCacheTTL))
	require.Error(t, err)
	failed.Store(false)
	_, err = cache.Read(t.Context(), client, mid, psp, wallet, now.Add(BalanceCacheTTL))
	require.NoError(t, err, "failure is not cached and old success cannot conceal it")
	require.Equal(t, 3, stub.count("getBalance"))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = cache.Read(ctx, client, mid, psp, wallet, now.Add(BalanceCacheTTL))
	require.ErrorIs(t, err, context.Canceled, "cancellation also applies to cache hits")
}

func TestBalanceCacheBindsMerchantPSPWalletAndRPCConfiguration(t *testing.T) {
	stub, url := newRPCStub(t, func(method string, _ int, _ []json.RawMessage) any {
		if method == "getGenesisHash" {
			return devnetGenesisHash
		}
		return map[string]any{"context": map[string]any{"slot": 1}, "value": uint64(1)}
	})
	client := NewRPCClientWithConfig(RPCClientConfig{Network: "devnet", PublicURL: url})
	mid, psp := uuid.New(), uuid.New()
	wallet := solanago.NewWallet().PublicKey()
	now := time.Now()
	var cache BalanceCache
	for _, row := range []struct {
		name     string
		mid, psp uuid.UUID
		wallet   solanago.PublicKey
	}{
		{"original", mid, psp, wallet},
		{"other merchant", uuid.New(), psp, wallet},
		{"other PSP", mid, uuid.New(), wallet},
		{"other wallet", mid, psp, solanago.NewWallet().PublicKey()},
	} {
		t.Run(row.name, func(t *testing.T) {
			_, err := cache.Read(t.Context(), client, row.mid, row.psp, row.wallet, now)
			require.NoError(t, err)
		})
	}
	require.Equal(t, 4, stub.count("getBalance"))
	replacement, replacementURL := newRPCStub(t, func(method string, _ int, _ []json.RawMessage) any {
		if method == "getGenesisHash" {
			return testnetGenesisHash
		}
		return map[string]any{"context": map[string]any{"slot": 1}, "value": uint64(2)}
	})
	wrong := NewRPCClientWithConfig(RPCClientConfig{Network: "devnet", PublicURL: replacementURL})
	_, err := cache.Read(t.Context(), wrong, mid, psp, wallet, now)
	require.ErrorContains(t, err, "testnet")
	require.Zero(t, replacement.count("getBalance"), "wrong network is rejected before reading its balance")
	right := NewRPCClientWithConfig(RPCClientConfig{Network: "testnet", PublicURL: replacementURL})
	balance, err := cache.Read(t.Context(), right, mid, psp, wallet, now)
	require.NoError(t, err)
	require.Equal(t, uint64(2), balance)
}

func TestBalanceCacheSlowTargetDoesNotBlockAnother(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	_, slowURL := newRPCStub(t, func(string, int, []json.RawMessage) any {
		close(started)
		<-release
		return devnetGenesisHash
	})
	_, goodURL := newRPCStub(t, func(method string, _ int, _ []json.RawMessage) any {
		if method == "getGenesisHash" {
			return devnetGenesisHash
		}
		return map[string]any{"context": map[string]any{"slot": 1}, "value": uint64(100)}
	})
	mid, psp := uuid.New(), uuid.New()
	wallet := solanago.NewWallet().PublicKey()
	var cache BalanceCache
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := cache.Read(ctx, NewRPCClientWithConfig(RPCClientConfig{Network: "devnet", PublicURL: slowURL}), mid, psp, wallet, time.Now())
		done <- err
	}()
	<-started
	goodCtx, stop := context.WithTimeout(t.Context(), time.Second)
	defer stop()
	balance, err := cache.Read(goodCtx, NewRPCClientWithConfig(RPCClientConfig{Network: "devnet", PublicURL: goodURL}), mid, psp, wallet, time.Now())
	cancel()
	close(release)
	require.NoError(t, err)
	require.Equal(t, uint64(100), balance)
	require.ErrorIs(t, <-done, context.Canceled)
}
