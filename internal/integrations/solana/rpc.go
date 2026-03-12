package solana

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	solanago "github.com/doujins-org/solana-go"
	"github.com/doujins-org/solana-go/rpc"
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

	// HeliusAPIKey enables Helius as the primary RPC provider.
	HeliusAPIKey string

	// Network determines which endpoints to use (mainnet, devnet, testnet).
	Network string
}

// NewRPCClientWithConfig creates a new Solana RPC client with fallback support.
func NewRPCClientWithConfig(cfg RPCClientConfig) *RPCClient {
	network := strings.ToLower(cfg.Network)
	if network == "" {
		network = "mainnet"
	}

	fallback := NewRPCFallbackClient(RPCFallbackConfig{
		CustomEndpoint: cfg.Endpoint,
		HeliusAPIKey:   cfg.HeliusAPIKey,
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

// GetTokenBalance returns the SPL token balance for an address and mint
func (c *RPCClient) GetTokenBalance(ctx context.Context, tokenAccount solanago.PublicKey) (*rpc.UiTokenAmount, error) {
	resp, err := c.fallback.GetTokenAccountBalance(ctx, tokenAccount)
	if err != nil {
		return nil, err
	}
	return resp.Value, nil
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

// ConfirmTransaction waits for a transaction to be confirmed
func (c *RPCClient) ConfirmTransaction(ctx context.Context, signature solanago.Signature, commitment rpc.CommitmentType) error {
	timeout := 60 * time.Second
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("transaction confirmation timeout for %s", signature.String())
		case <-ticker.C:
			status, err := c.fallback.GetSignatureStatuses(ctx, true, signature)
			if err != nil {
				log.WithError(err).Warn("Failed to get signature status")
				continue
			}

			if len(status.Value) > 0 && status.Value[0] != nil {
				sigStatus := status.Value[0]

				// Check for errors
				if sigStatus.Err != nil {
					return fmt.Errorf("transaction failed: %v", sigStatus.Err)
				}

				// Check if confirmed at desired level
				if sigStatus.ConfirmationStatus != "" {
					switch commitment {
					case rpc.CommitmentProcessed:
						if sigStatus.ConfirmationStatus == rpc.ConfirmationStatusProcessed ||
							sigStatus.ConfirmationStatus == rpc.ConfirmationStatusConfirmed ||
							sigStatus.ConfirmationStatus == rpc.ConfirmationStatusFinalized {
							return nil
						}
					case rpc.CommitmentConfirmed:
						if sigStatus.ConfirmationStatus == rpc.ConfirmationStatusConfirmed ||
							sigStatus.ConfirmationStatus == rpc.ConfirmationStatusFinalized {
							return nil
						}
					case rpc.CommitmentFinalized:
						if sigStatus.ConfirmationStatus == rpc.ConfirmationStatusFinalized {
							return nil
						}
					}
				}
			}
		}
	}
}

// GetLatestBlockhash gets the latest blockhash for transaction creation
func (c *RPCClient) GetLatestBlockhash(ctx context.Context) (solanago.Hash, error) {
	return c.fallback.GetLatestBlockhash(ctx)
}

// GetMinimumBalanceForRentExemption returns the minimum balance needed for rent exemption
func (c *RPCClient) GetMinimumBalanceForRentExemption(ctx context.Context, dataSize uint64) (uint64, error) {
	return c.fallback.GetMinimumBalanceForRentExemption(ctx, dataSize)
}

// GetEndpoint returns the current RPC endpoint
func (c *RPCClient) GetEndpoint() string {
	return c.fallback.GetEndpoint()
}

// SignatureInfo is a pared-down view of a signature lookup.
type SignatureInfo struct {
	Signature string
	HasError  bool
}

// GetSignaturesForAddress finds transactions that reference a specific address.
func (c *RPCClient) GetSignaturesForAddress(ctx context.Context, address string, limit int) ([]SignatureInfo, error) {
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

	resp, err := c.fallback.GetSignaturesForAddressWithOpts(ctx, pubkey, opts)
	if err != nil {
		return nil, fmt.Errorf("failed to get signatures for address %s: %w", pubkey.String(), err)
	}

	results := make([]SignatureInfo, 0, len(resp))
	for _, sig := range resp {
		results = append(results, SignatureInfo{
			Signature: sig.Signature.String(),
			HasError:  sig.Err != nil,
		})
	}

	return results, nil
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
