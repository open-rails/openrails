package solana

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
	log "github.com/sirupsen/logrus"
)

// RPCClient handles interactions with the Solana blockchain.
// It supports automatic failover between multiple RPC endpoints.
type RPCClient struct {
	fallback *RPCFallbackClient
}

// RPCClientConfig holds configuration for creating an RPC client.
type RPCClientConfig struct {
	// Endpoint is a custom RPC endpoint. If set, bypasses the fallback chain.
	Endpoint string

	// RPCProvider selects the preferred Solana RPC provider. Empty defaults to "helius".
	RPCProvider string

	// RPCAPIKey is the key for RPCProvider.
	RPCAPIKey string

	// Network determines which endpoints to use (mainnet, devnet, testnet).
	Network string
	// ReadOnly blocks transaction submission at the wire (mode=readonly, #346).
	ReadOnly bool
}

// NewRPCClientWithConfig creates a new Solana RPC client with fallback support.
func NewRPCClientWithConfig(cfg RPCClientConfig) *RPCClient {
	network := strings.ToLower(cfg.Network)
	if network == "" {
		network = "mainnet"
	}

	fallback := NewRPCFallbackClient(RPCFallbackConfig{
		ReadOnly:       cfg.ReadOnly,
		CustomEndpoint: cfg.Endpoint,
		RPCProvider:    cfg.RPCProvider,
		RPCAPIKey:      cfg.RPCAPIKey,
		Network:        network,
	})

	return &RPCClient{
		fallback: fallback,
	}
}

// GetBalance returns the SOL balance for an address.
func (c *RPCClient) GetBalance(ctx context.Context, address solanago.PublicKey) (uint64, error) {
	return c.fallback.GetBalance(ctx, address)
}

// GetBalanceAtSlot returns the SOL balance evaluated at >= minSlot (the slot a
// preceding write confirmed at), so a lagging RPC node returns a retryable error
// instead of stale data. minSlot == 0 behaves like GetBalance.
func (c *RPCClient) GetBalanceAtSlot(ctx context.Context, address solanago.PublicKey, minSlot uint64) (uint64, error) {
	return c.fallback.GetBalanceAtSlot(ctx, address, minSlot)
}

// GetTokenBalance returns the SPL token balance for an address and mint
func (c *RPCClient) GetTokenBalance(ctx context.Context, tokenAccount solanago.PublicKey) (*rpc.UiTokenAmount, error) {
	resp, err := c.fallback.GetTokenAccountBalance(ctx, tokenAccount)
	if err != nil {
		return nil, err
	}
	return resp.Value, nil
}

// GetTokenBalanceForMintAtSlot is GetTokenBalanceForMint gated on minSlot: the
// ATA balance is read only against a node that has reached minSlot (the slot a
// preceding write — e.g. a pull/crank — confirmed at), so a lagging node returns
// a retryable error instead of a stale balance. minSlot == 0 behaves exactly like
// GetTokenBalanceForMint. Returns 0 when the ATA does not exist.
func (c *RPCClient) GetTokenBalanceForMintAtSlot(ctx context.Context, owner solanago.PublicKey, mint solanago.PublicKey, minSlot uint64) (uint64, error) {
	if minSlot == 0 {
		return c.GetTokenBalanceForMint(ctx, owner, mint)
	}
	ata, _, err := solanago.FindAssociatedTokenAddress(owner, mint)
	if err != nil {
		return 0, fmt.Errorf("failed to derive ATA for mint %s: %w", mint.String(), err)
	}
	resp, err := c.fallback.GetTokenAccountBalanceAtSlot(ctx, ata, minSlot)
	if err != nil {
		if strings.Contains(err.Error(), "could not find account") ||
			strings.Contains(err.Error(), "Invalid param") {
			return 0, nil
		}
		return 0, fmt.Errorf("failed to get token balance: %w", err)
	}
	if resp.Value == nil {
		return 0, nil
	}
	balance, err := strconv.ParseUint(resp.Value.Amount, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("failed to parse token balance %q: %w", resp.Value.Amount, err)
	}
	return balance, nil
}

// SimulateTransaction simulates a transaction to check if it would succeed
func (c *RPCClient) SimulateTransaction(ctx context.Context, tx *solanago.Transaction) (*rpc.SimulateTransactionResponse, error) {
	return c.fallback.SimulateTransaction(ctx, tx)
}

// SendTransaction submits a transaction to the blockchain
func (c *RPCClient) SendTransaction(ctx context.Context, tx *solanago.Transaction) (solanago.Signature, error) {
	return c.fallback.SendTransaction(ctx, tx)
}

// GetTransaction retrieves transaction details by signature
func (c *RPCClient) GetTransaction(ctx context.Context, signature solanago.Signature) (*rpc.GetTransactionResult, error) {
	return c.fallback.GetTransaction(ctx, signature)
}

func (c *RPCClient) GetTransactionWithRetry(ctx context.Context, signature solanago.Signature, attempts int, delay time.Duration) (*rpc.GetTransactionResult, error) {
	var lastErr error
	for i := 0; i < attempts; i++ {
		resp, err := c.GetTransaction(ctx, signature)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if !isNotFoundError(err) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
	}
	return nil, lastErr
}

func isNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "not found")
}

// ConfirmTransaction waits for an already-seen signature to reach the
// requested commitment. It is the verify path's wait (a signature discovered
// on-chain, e.g. by GetSignaturesForAddress), so the chain terminal is not
// known here; the wait ends when the commitment is reached, the chain reports
// an on-chain error, or the caller's context ends (xs-007 row 36 — it used to
// give up at 60 s and call a landing at 61 s a failure).
func (c *RPCClient) ConfirmTransaction(ctx context.Context, signature solanago.Signature, commitment rpc.CommitmentType) error {
	outcome, err := c.WatchTransaction(ctx, signature, commitment, ChainTerminal{})
	if err != nil {
		return fmt.Errorf("transaction confirmation for %s: %w", signature.String(), err)
	}
	if outcome.Err != nil {
		return fmt.Errorf("transaction failed: %v", outcome.Err)
	}
	return nil
}

// GetLatestBlockhash gets the latest blockhash for transaction creation
func (c *RPCClient) GetLatestBlockhash(ctx context.Context) (solanago.Hash, error) {
	return c.fallback.GetLatestBlockhash(ctx)
}

// LatestBlockhash gets the latest blockhash together with the chain terminal
// (lastValidBlockHeight) a transaction built on it must be watched against.
func (c *RPCClient) LatestBlockhash(ctx context.Context) (RecentBlockhash, error) {
	return c.fallback.LatestBlockhash(ctx)
}

// GetMinimumBalanceForRentExemption returns the minimum balance needed for rent exemption
func (c *RPCClient) GetMinimumBalanceForRentExemption(ctx context.Context, dataSize uint64) (uint64, error) {
	return c.fallback.GetMinimumBalanceForRentExemption(ctx, dataSize)
}

// GetAccountData returns an account's raw data bytes, or (nil, nil) when the
// account does not exist (used to read on-chain PDA state, e.g. the
// SubscriptionAuthority initId for building a subscribe instruction).
func (c *RPCClient) GetAccountData(ctx context.Context, address solanago.PublicKey) ([]byte, error) {
	return c.fallback.GetAccountData(ctx, address)
}

// GetAccountDataAtSlot is GetAccountData gated on minContextSlot: the account is
// read only against a node that has reached minSlot (the slot a preceding write
// confirmed at), so a lagging node returns a retryable error rather than a stale
// (or "missing") account. minSlot == 0 behaves like GetAccountData.
func (c *RPCClient) GetAccountDataAtSlot(ctx context.Context, address solanago.PublicKey, minSlot uint64) ([]byte, error) {
	return c.fallback.GetAccountDataAtSlot(ctx, address, minSlot)
}

// GetEndpoint returns the current RPC endpoint
func (c *RPCClient) GetEndpoint() string {
	return c.fallback.GetEndpoint()
}

// SignatureInfo is a pared-down view of a signature lookup.
type SignatureInfo struct {
	Signature string
	HasError  bool
	// BlockTime is the cluster-reported block time, nil when the node did not
	// return one (older ledger ranges).
	BlockTime *time.Time
}

// GetSignaturesForAddress finds transactions that reference a specific address.
func (c *RPCClient) GetSignaturesForAddress(ctx context.Context, address string, limit int) ([]SignatureInfo, error) {
	return c.GetSignaturesForAddressPage(ctx, address, "", limit)
}

// GetSignaturesForAddressPage is GetSignaturesForAddress with a pagination
// cursor: before != "" continues the newest-first walk strictly below that
// signature (#714 wallet-scan pagination). limit caps the page (RPC max 1000).
func (c *RPCClient) GetSignaturesForAddressPage(ctx context.Context, address string, before string, limit int) ([]SignatureInfo, error) {
	pubkey, err := solanago.PublicKeyFromBase58(strings.TrimSpace(address))
	if err != nil {
		return nil, fmt.Errorf("invalid address: %w", err)
	}

	opts := &rpc.GetSignaturesForAddressOpts{
		Commitment: rpc.CommitmentFinalized,
	}
	if limit > 0 {
		limitVal := limit
		opts.Limit = &limitVal
	}
	if before = strings.TrimSpace(before); before != "" {
		sig, err := solanago.SignatureFromBase58(before)
		if err != nil {
			return nil, fmt.Errorf("invalid before cursor: %w", err)
		}
		opts.Before = sig
	}

	resp, err := c.fallback.GetSignaturesForAddressWithOpts(ctx, pubkey, opts)
	if err != nil {
		return nil, fmt.Errorf("failed to get signatures for address %s: %w", pubkey.String(), err)
	}

	results := make([]SignatureInfo, 0, len(resp))
	for _, sig := range resp {
		info := SignatureInfo{
			Signature: sig.Signature.String(),
			HasError:  sig.Err != nil,
		}
		if sig.BlockTime != nil {
			t := sig.BlockTime.Time().UTC()
			info.BlockTime = &t
		}
		results = append(results, info)
	}

	return results, nil
}

// ProgramAccountFilter is one memcmp filter for GetProgramAccounts: account
// data must equal Bytes at Offset. Filters AND together.
type ProgramAccountFilter struct {
	Offset uint64
	Bytes  []byte
}

// ProgramAccount is one account returned by GetProgramAccounts.
type ProgramAccount struct {
	Address solanago.PublicKey
	Data    []byte
}

// GetProgramAccounts lists the accounts owned by program that match every
// memcmp filter — a bulk point-in-time listing (no slot gating), used by the
// #714 per-plan subscription enumeration.
func (c *RPCClient) GetProgramAccounts(ctx context.Context, program solanago.PublicKey, filters []ProgramAccountFilter) ([]ProgramAccount, error) {
	opts := &rpc.GetProgramAccountsOpts{
		Commitment: rpc.CommitmentFinalized,
		Encoding:   solanago.EncodingBase64,
	}
	for _, f := range filters {
		opts.Filters = append(opts.Filters, rpc.RPCFilter{
			Memcmp: &rpc.RPCFilterMemcmp{Offset: f.Offset, Bytes: solanago.Base58(f.Bytes)},
		})
	}
	resp, err := c.fallback.GetProgramAccountsWithOpts(ctx, program, opts)
	if err != nil {
		return nil, fmt.Errorf("failed to get program accounts for %s: %w", program.String(), err)
	}
	out := make([]ProgramAccount, 0, len(resp))
	for _, acc := range resp {
		pa := ProgramAccount{Address: acc.Pubkey}
		if acc.Account != nil && acc.Account.Data != nil {
			pa.Data = acc.Account.Data.GetBinary()
		}
		out = append(out, pa)
	}
	return out, nil
}

// TokenAccountInfo represents an SPL token account balance for a specific mint.
type TokenAccountInfo struct {
	Mint    string
	Balance uint64
}

// GetTokenBalanceForMint returns the SPL token balance for a specific mint owned by a wallet.
// It derives the Associated Token Account (ATA) address and queries its balance.
// Returns 0 if the account doesn't exist or has no balance.
func (c *RPCClient) GetTokenBalanceForMint(ctx context.Context, owner solanago.PublicKey, mint solanago.PublicKey) (uint64, error) {
	// Derive the Associated Token Account address
	ata, _, err := solanago.FindAssociatedTokenAddress(owner, mint)
	if err != nil {
		return 0, fmt.Errorf("failed to derive ATA for mint %s: %w", mint.String(), err)
	}

	// Get the token account balance
	resp, err := c.fallback.GetTokenAccountBalance(ctx, ata)
	if err != nil {
		// Account might not exist (user has never held this token)
		if strings.Contains(err.Error(), "could not find account") ||
			strings.Contains(err.Error(), "Invalid param") {
			return 0, nil
		}
		return 0, fmt.Errorf("failed to get token balance: %w", err)
	}

	if resp.Value == nil {
		return 0, nil
	}

	// Parse the amount string to uint64
	balance, err := strconv.ParseUint(resp.Value.Amount, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("failed to parse token balance %q: %w", resp.Value.Amount, err)
	}
	return balance, nil
}

// GetTokenBalances returns token balances for multiple mints owned by a wallet.
// Mints that don't exist or have zero balance are included with Balance=0.
func (c *RPCClient) GetTokenBalances(ctx context.Context, owner solanago.PublicKey, mints []string) ([]TokenAccountInfo, error) {
	if len(mints) == 0 {
		return nil, nil
	}
	accounts := make([]TokenAccountInfo, 0, len(mints))
	for _, mintStr := range mints {
		mint, err := solanago.PublicKeyFromBase58(mintStr)
		if err != nil {
			log.WithError(err).WithField("mint", mintStr).Debug("Failed to parse token mint")
			continue
		}

		balance, err := c.GetTokenBalanceForMint(ctx, owner, mint)
		if err != nil {
			log.WithError(err).WithField("mint", mintStr).Debug("Failed to get token balance")
			continue
		}

		accounts = append(accounts, TokenAccountInfo{Mint: mintStr, Balance: balance})
	}

	return accounts, nil
}

// PrimaryCredentialFingerprint exposes the armed-credential fingerprint of the
// primary endpoint (#SEC-17) — the credential itself is never in GetEndpoint().
func (c *RPCClient) PrimaryCredentialFingerprint() string {
	return c.fallback.PrimaryCredentialFingerprint()
}
