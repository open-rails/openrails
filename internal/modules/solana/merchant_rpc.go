package solana

import (
	"context"
	"fmt"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	solanarpc "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchants"
)

// MerchantRPCBuilder resolves one merchant's Solana RPC client at use time for
// the process-wide services (poller, crank, intent verify). Nothing is cached,
// so a rotated setting takes effect on the next pass. MerchantsFn is
// late-bound; a nil fn or service arms nothing.
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
	return b != nil && b.Config != nil && config.IsTestMode(b.Config)
}

// Resolve arms the merchant's RPC client from its declared solana account's
// settings, picking the pull scope (active, else newest archived for drain).
// nil client with nil error = no declared account (caller skips/warns).
func (b *MerchantRPCBuilder) Resolve(ctx context.Context, mid billing.MerchantID) (*solanarpc.RPCClient, error) {
	if b == nil {
		return nil, nil
	}
	svc := b.merchants()
	if svc == nil {
		return nil, nil // nothing arms without the merchants service
	}
	scope, ok, err := svc.PullPSPScope(ctx, mid, "solana", config.ExpectedProviderEnvironment(b.testMode()))
	if err != nil {
		return nil, fmt.Errorf("solana: resolve merchant %s rail account: %w", mid.String(), err)
	}
	if !ok {
		return nil, nil // no declared solana account → not armed
	}
	settings, err := config.ParseSolanaAccountSettings(scope.Settings)
	if err != nil {
		// Malformed settings fail loud; there is no fallback client.
		return nil, fmt.Errorf("solana: merchant %s account %s settings: %w", mid.String(), scope.AccountID, err)
	}
	network := "mainnet"
	if b.testMode() {
		network = "devnet"
	}
	return solanarpc.NewRPCClientWithConfig(solanarpc.RPCClientConfig{
		Endpoint:        b.Endpoint,
		LoopbackFixture: b.Endpoint != "",
		RPCProvider:     settings.RPCProvider,
		RPCAPIKey:       settings.RPCAPIKey,
		Network:         network,
		ReadOnly:        b.Config != nil && config.IsProviderReadOnly(b.Config),
	}), nil
}

// ChainReader adapts the builder to the intent verify leg's chain reads: the
// merchant comes off the intent runner's merchant-scoped ctx. Satisfies
// riverjobs.SolanaTxReader.
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

func (r *MerchantChainReader) TransactionExpiredUnseen(ctx context.Context, signature solanago.Signature, terminal solanarpc.ChainTerminal) (bool, error) {
	client, err := r.client(ctx)
	if err != nil {
		return false, err
	}
	return client.TransactionExpiredUnseen(ctx, signature, terminal)
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

func (r *MerchantChainReader) WatchTransaction(ctx context.Context, sig solanago.Signature, commitment rpc.CommitmentType, terminal solanarpc.ChainTerminal) (*solanarpc.TransactionOutcome, error) {
	client, err := r.client(ctx)
	if err != nil {
		return nil, err
	}
	return client.WatchTransaction(ctx, sig, commitment, terminal)
}

// MintInfo reads a mint's token program, decimals and Token-2022 transfer fee
// and hook as they stand now (these can change; never cache them).
func (r *MerchantChainReader) MintInfo(ctx context.Context, mint solanago.PublicKey) (*solanarpc.MintInfo, error) {
	client, err := r.client(ctx)
	if err != nil {
		return nil, err
	}
	return client.GetMintInfo(ctx, mint)
}
