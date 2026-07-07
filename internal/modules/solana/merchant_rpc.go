package solana

import (
	"context"
	"fmt"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"

	"github.com/open-rails/openrails/config"
	solanarpc "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/merchant"
)

// #728: per-merchant Solana RPC arming for the PROCESS-WIDE services (payment
// poller, recurring crank, intent verify legs). Precedence per #699/#725:
// merchant-store rail-account settings (rpc_provider / rpc_api_key) first, the
// boot-built client as fallback for merchants with no declared solana account.
// Malformed settings on a DECLARED account fail LOUD — never a silent
// cross-plane fallback. Nothing is cached across passes: clients are cheap
// per-pass structs, so a rotated setting takes effect on the next pass.

// MerchantRPCBuilder resolves one merchant's Solana RPC client at use time.
// MerchantsFn is late-bound (Runtime.Merchants wires after build); a nil
// fn/service = boot plane only.
type MerchantRPCBuilder struct {
	Config      *config.Config
	MerchantsFn func() *merchants.Service
	// Endpoint overrides store-armed clients' RPC endpoint — a test seam for
	// fake RPC servers. Zero value = real endpoints.
	Endpoint string
}

func (b *MerchantRPCBuilder) merchants() *merchants.Service {
	if b == nil || b.MerchantsFn == nil {
		return nil
	}
	return b.MerchantsFn()
}

func (b *MerchantRPCBuilder) testMode() bool {
	return b != nil && b.Config != nil && b.Config.IsTestMode()
}

// Resolve arms the merchant's RPC client: a declared solana account's store
// settings win; a merchant with no declared account falls back to the boot
// client. nil client with nil error = neither plane armed (caller skips/warns).
// The scope pick is the pull scope (active for new work, else newest archived
// for drain — #655), matching the #725 collection resolver.
func (b *MerchantRPCBuilder) Resolve(ctx context.Context, mid merchant.ID) (*solanarpc.RPCClient, error) {
	if b == nil {
		return nil, nil
	}
	svc := b.merchants()
	if svc == nil {
		return nil, nil // nothing arms without the merchants service (#788)
	}
	scope, ok, err := svc.PullRailMerchantAccountScope(ctx, mid, "solana", config.ExpectedProviderEnvironment(b.testMode()))
	if err != nil {
		return nil, fmt.Errorf("solana: resolve merchant %s rail account: %w", mid.String(), err)
	}
	if !ok {
		return nil, nil // no declared solana account → not armed (#788)
	}
	settings, err := config.ParseSolanaAccountSettings(scope.Settings)
	if err != nil {
		// Fail loud: a declared account never falls back across planes (#699).
		return nil, fmt.Errorf("solana: merchant %s account %s settings: %w", mid.String(), scope.AccountID, err)
	}
	network := "mainnet"
	if b.testMode() {
		network = "devnet"
	}
	return solanarpc.NewRPCClientWithConfig(solanarpc.RPCClientConfig{
		Endpoint:    b.Endpoint,
		RPCProvider: settings.RPCProvider,
		RPCAPIKey:   settings.RPCAPIKey,
		Network:     network,
		ReadOnly:    b.Config != nil && b.Config.IsProviderReadOnly(),
	}), nil
}

// ChainReader adapts the builder to per-intent chain reads (the #674 verify
// leg): the merchant comes off the intent runner's merchant-scoped ctx.
// Satisfies riverjobs.SolanaTxReader.
func (b *MerchantRPCBuilder) ChainReader() *MerchantChainReader {
	return &MerchantChainReader{builder: b}
}

// MerchantChainReader is a merchant-resolving GetTransaction surface.
type MerchantChainReader struct {
	builder *MerchantRPCBuilder
}

// client resolves the ctx merchant's armed RPC client (fail closed on none).
func (r *MerchantChainReader) client(ctx context.Context) (*solanarpc.RPCClient, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, fmt.Errorf("solana chain read: %w", err)
	}
	client, err := r.builder.Resolve(ctx, mid)
	if err != nil {
		return nil, err
	}
	if client == nil {
		return nil, fmt.Errorf("solana chain read: no RPC client armed for merchant %s (#728)", mid.String())
	}
	return client, nil
}

func (r *MerchantChainReader) GetTransaction(ctx context.Context, signature solanago.Signature) (*rpc.GetTransactionResult, error) {
	client, err := r.client(ctx)
	if err != nil {
		return nil, err
	}
	return client.GetTransaction(ctx, signature)
}

func (r *MerchantChainReader) GetAccountData(ctx context.Context, address solanago.PublicKey) ([]byte, error) {
	client, err := r.client(ctx)
	if err != nil {
		return nil, err
	}
	return client.GetAccountData(ctx, address)
}

func (r *MerchantChainReader) GetLatestBlockhash(ctx context.Context) (solanago.Hash, error) {
	client, err := r.client(ctx)
	if err != nil {
		return solanago.Hash{}, err
	}
	return client.GetLatestBlockhash(ctx)
}

func (r *MerchantChainReader) GetBalance(ctx context.Context, address solanago.PublicKey) (uint64, error) {
	client, err := r.client(ctx)
	if err != nil {
		return 0, err
	}
	return client.GetBalance(ctx, address)
}

func (r *MerchantChainReader) GetTokenBalanceForMint(ctx context.Context, owner solanago.PublicKey, mint solanago.PublicKey) (uint64, error) {
	client, err := r.client(ctx)
	if err != nil {
		return 0, err
	}
	return client.GetTokenBalanceForMint(ctx, owner, mint)
}

func (r *MerchantChainReader) WatchTransaction(ctx context.Context, sig solanago.Signature, commitment rpc.CommitmentType, timeout time.Duration) (*solanarpc.TransactionOutcome, error) {
	client, err := r.client(ctx)
	if err != nil {
		return nil, err
	}
	return client.WatchTransaction(ctx, sig, commitment, timeout)
}
