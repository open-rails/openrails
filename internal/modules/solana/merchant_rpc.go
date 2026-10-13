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
	solanatokens "github.com/open-rails/openrails/internal/modules/solana/tokens"
)

// MerchantRPCBuilder resolves one merchant's Solana RPC client at use time for
// the process-wide services (poller, crank, intent verify). Nothing is cached,
// so a rotated rpc_url takes effect on the next pass. MerchantsFn is
// late-bound; a nil fn or service arms nothing.
type MerchantRPCBuilder struct {
	Config      *config.Config
	MerchantsFn func() *merchants.Service
	// Endpoint replaces the network's public endpoint: a sandbox run's
	// loopback node (provider_sandbox.solana_rpc_url).
	Endpoint string
}

func (b *MerchantRPCBuilder) merchants() *merchants.Service {
	if b == nil || b.MerchantsFn == nil {
		return nil
	}
	return b.MerchantsFn()
}

// Resolve arms the merchant's RPC client from its Solana PSP (active, else
// newest archived for drain): its rpc_url first, its network's public
// endpoint as fallback. nil client with nil error = no Solana PSP.
func (b *MerchantRPCBuilder) Resolve(ctx context.Context, mid billing.MerchantID) (*solanarpc.RPCClient, error) {
	if b == nil {
		return nil, nil
	}
	svc := b.merchants()
	if svc == nil {
		return nil, nil
	}
	scope, ok, err := svc.PullPSPScope(ctx, mid, "solana")
	if err != nil {
		return nil, fmt.Errorf("solana: resolve merchant %s rail account: %w", mid.String(), err)
	}
	if !ok {
		return nil, nil
	}
	rpcURL, err := svc.OptionalPSPSecret(ctx, mid, scope, config.SolanaSecretRPCURL)
	if err != nil {
		return nil, fmt.Errorf("solana: merchant %s rpc_url: %w", mid.String(), err)
	}
	return solanarpc.NewRPCClientWithConfig(solanarpc.RPCClientConfig{
		Network:   scope.Environment,
		URL:       rpcURL,
		PublicURL: b.Endpoint,
		ReadOnly:  b.Config != nil && config.IsProviderReadOnly(b.Config),
	}), nil
}

// Network is the network the ctx merchant's Solana PSP pays on, its
// environment; mainnet when it has none.
func (b *MerchantRPCBuilder) Network(ctx context.Context) string {
	svc := b.merchants()
	mid, err := merchant.Require(ctx)
	if svc == nil || err != nil {
		return config.EnvironmentMainnet
	}
	scope, ok, err := svc.PullPSPScope(ctx, mid, "solana")
	if err != nil || !ok {
		return config.EnvironmentMainnet
	}
	return scope.Environment
}

// RecurringTokens is the ctx merchant's network and the tokens accepted on
// it: the recurring services' mint lookup.
func (b *MerchantRPCBuilder) RecurringTokens(ctx context.Context) (string, map[string]config.TokenConfig) {
	network := b.Network(ctx)
	return network, solanatokens.Accepted(network)
}

// Treasury is the merchant's Solana treasury: its active Solana PSP's account.
func (b *MerchantRPCBuilder) Treasury(ctx context.Context, mid billing.MerchantID) (solanago.PublicKey, error) {
	svc := b.merchants()
	if svc == nil {
		return solanago.PublicKey{}, fmt.Errorf("solana: no merchant configuration")
	}
	scope, ok, err := svc.ActivePSPScope(ctx, mid, "solana")
	if err != nil {
		return solanago.PublicKey{}, err
	}
	if !ok {
		return solanago.PublicKey{}, fmt.Errorf("solana: merchant %s has no active Solana PSP, so no treasury", mid)
	}
	return solanago.PublicKeyFromBase58(scope.AccountID)
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
