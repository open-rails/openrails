package solana

import (
	"context"
	"fmt"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
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

// Resolve uses the operation's pinned PSP when present. Unpinned work uses
// the merchant's active account, or newest archived account for draining.
// A pinned account never falls back to a replacement account.
func (b *MerchantRPCBuilder) Resolve(ctx context.Context, mid billing.MerchantID) (*solanarpc.RPCClient, error) {
	if id := db.PSPIDFromContext(ctx); id != uuid.Nil {
		client, _, err := b.ResolvePSP(ctx, mid, id)
		return client, err
	}
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

// ResolvePSP resolves the exact Solana account owning an operation.
// A missing account record never falls back to the merchant's newest PSP.
func (b *MerchantRPCBuilder) ResolvePSP(ctx context.Context, mid billing.MerchantID, pspID uuid.UUID) (*solanarpc.RPCClient, string, error) {
	svc := b.merchants()
	if svc == nil {
		return nil, "", fmt.Errorf("solana: merchant configuration unavailable")
	}
	scope, ok, err := svc.PSPScopeByID(ctx, mid, pspID)
	if err != nil {
		return nil, "", err
	}
	if !ok || scope.Rail != "solana" {
		return nil, "", fmt.Errorf("solana: PSP %s unavailable for merchant %s", pspID, mid)
	}
	rpcURL, err := svc.OptionalPSPSecret(ctx, mid, scope, config.SolanaSecretRPCURL)
	if err != nil {
		return nil, scope.Environment, err
	}
	return solanarpc.NewRPCClientWithConfig(solanarpc.RPCClientConfig{
		Network: scope.Environment, URL: rpcURL, PublicURL: b.Endpoint,
		ReadOnly: b.Config != nil && config.IsProviderReadOnly(b.Config),
	}), scope.Environment, nil
}

// Network follows the pinned PSP, or the merchant's current Solana account.
// An unavailable pinned account returns no network.
func (b *MerchantRPCBuilder) Network(ctx context.Context) string {
	svc := b.merchants()
	mid, err := merchant.Require(ctx)
	if svc == nil || err != nil {
		if db.PSPIDFromContext(ctx) != uuid.Nil {
			return ""
		}
		return config.EnvironmentMainnet
	}
	if id := db.PSPIDFromContext(ctx); id != uuid.Nil {
		scope, ok, err := svc.PSPScopeByID(ctx, mid, id)
		if err != nil || !ok || scope.Rail != "solana" {
			return ""
		}
		return scope.Environment
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
	if network == "" {
		return "", nil
	}
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
