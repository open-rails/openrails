package solana

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/gagliardetto/solana-go/rpc/jsonrpc"
	log "github.com/sirupsen/logrus"
)

// RPCEndpoint represents a single RPC endpoint with metadata.
type RPCEndpoint struct {
	Name     string // Human-readable name (e.g., "Helius", "Solana Public")
	URL      string // Full RPC URL
	Priority int    // Lower = higher priority
}

// RPCFallbackClient wraps multiple RPC clients and provides automatic failover.
type RPCFallbackClient struct {
	endpoints []RPCEndpoint
	clients   []*rpc.Client
	network   string
	mu        sync.RWMutex

	// Track endpoint health for smart failover
	failures map[int]time.Time // endpoint index -> next retry time
	// readOnly blocks SendTransaction/SendTransactionSkipPreflight (the only
	// chain mutations) with ErrProviderReadOnly when mode=readonly (#346).
	// Reads (account data, balances, signatures) always pass.
	readOnly bool
}

// RPCFallbackConfig holds configuration for building the fallback chain.
type RPCFallbackConfig struct {
	// CustomEndpoint bypasses the fallback chain entirely if set.
	CustomEndpoint string

	// RPCProvider selects the preferred Solana RPC provider. Empty defaults to "helius".
	RPCProvider string

	// RPCAPIKey is the key for RPCProvider.
	RPCAPIKey string

	// Network determines which endpoints to use (mainnet, devnet).
	Network string
	// ReadOnly blocks transaction submission at the wire (mode=readonly).
	ReadOnly bool
}

// DefaultMainnetEndpoints returns the default RPC endpoints for mainnet.
func DefaultMainnetEndpoints(heliusAPIKey string) []RPCEndpoint {
	endpoints := []RPCEndpoint{}
	priority := 0

	// Helius (primary if API key provided)
	if heliusAPIKey != "" {
		endpoints = append(endpoints, RPCEndpoint{
			Name:     "Helius",
			URL:      fmt.Sprintf("https://mainnet.helius-rpc.com/?api-key=%s", heliusAPIKey),
			Priority: priority,
		})
		priority++
	}

	// Solana public (fallback, rate-limited)
	endpoints = append(endpoints, RPCEndpoint{
		Name:     "Solana Public",
		URL:      "https://api.mainnet-beta.solana.com",
		Priority: priority,
	})

	return endpoints
}

// DefaultDevnetEndpoints returns the default RPC endpoints for devnet.
func DefaultDevnetEndpoints(heliusAPIKey string) []RPCEndpoint {
	endpoints := []RPCEndpoint{}
	priority := 0

	// Helius devnet (primary if API key provided)
	if heliusAPIKey != "" {
		endpoints = append(endpoints, RPCEndpoint{
			Name:     "Helius Devnet",
			URL:      fmt.Sprintf("https://devnet.helius-rpc.com/?api-key=%s", heliusAPIKey),
			Priority: priority,
		})
		priority++
	}

	// Solana public devnet
	endpoints = append(endpoints, RPCEndpoint{
		Name:     "Solana Devnet",
		URL:      "https://api.devnet.solana.com",
		Priority: priority,
	})

	return endpoints
}

// NewRPCFallbackClient creates a new RPC client with fallback support.
func NewRPCFallbackClient(cfg RPCFallbackConfig) *RPCFallbackClient {
	network := strings.ToLower(cfg.Network)
	if network == "" {
		network = "mainnet"
	}
	rpcProvider := strings.ToLower(strings.TrimSpace(cfg.RPCProvider))
	if rpcProvider == "" {
		rpcProvider = "helius"
	}
	rpcAPIKey := strings.TrimSpace(cfg.RPCAPIKey)

	var endpoints []RPCEndpoint

	// If custom endpoint is provided, use it exclusively (no fallback)
	if cfg.CustomEndpoint != "" {
		endpoints = []RPCEndpoint{{
			Name:     "Custom",
			URL:      cfg.CustomEndpoint,
			Priority: 0,
		}}
		log.WithFields(log.Fields{
			"endpoint": cfg.CustomEndpoint,
			"network":  network,
		}).Info("Using custom RPC endpoint (fallback disabled)")
	} else {
		// Build fallback chain based on network
		switch network {
		case "devnet":
			endpoints = DefaultDevnetEndpoints(rpcProviderAPIKey(rpcProvider, rpcAPIKey, "helius"))
		case "mainnet", "mainnet-beta":
			endpoints = DefaultMainnetEndpoints(rpcProviderAPIKey(rpcProvider, rpcAPIKey, "helius"))
		default:
			// Testnet uses Solana public only
			endpoints = []RPCEndpoint{{
				Name:     "Solana Testnet",
				URL:      "https://api.testnet.solana.com",
				Priority: 0,
			}}
		}

		// Log the fallback chain
		names := make([]string, len(endpoints))
		for i, ep := range endpoints {
			names[i] = ep.Name
		}
		log.WithFields(log.Fields{
			"chain":   strings.Join(names, " → "),
			"network": network,
		}).Info("Initialized Solana RPC fallback chain")
	}

	// Create RPC clients for each endpoint
	clients := make([]*rpc.Client, len(endpoints))
	for i, ep := range endpoints {
		clients[i] = rpc.New(ep.URL)
	}

	return &RPCFallbackClient{
		readOnly:  cfg.ReadOnly,
		endpoints: endpoints,
		clients:   clients,
		network:   network,
		failures:  make(map[int]time.Time),
	}
}

func rpcProviderAPIKey(rpcProvider, rpcAPIKey, want string) string {
	if rpcProvider == want {
		return rpcAPIKey
	}
	return ""
}

// failureCooldown is how long we wait before retrying a failed endpoint.
const failureCooldown = 30 * time.Second

// rateLimitCooldown is how long we wait before retrying an endpoint that returned 429.
const rateLimitCooldown = 2 * time.Minute

// getActiveEndpoints returns endpoints that aren't in cooldown, preserving priority order.
func (c *RPCFallbackClient) getActiveEndpoints() []int {
	c.mu.RLock()
	defer c.mu.RUnlock()

	now := time.Now()
	active := make([]int, 0, len(c.endpoints))

	for i := range c.endpoints {
		if retryAt, failed := c.failures[i]; failed {
			if now.Before(retryAt) {
				continue // Still in cooldown
			}
		}
		active = append(active, i)
	}

	// If all endpoints are in cooldown, return all of them (we have to try something)
	if len(active) == 0 {
		for i := range c.endpoints {
			active = append(active, i)
		}
	}

	return active
}

// markFailed records a failure for an endpoint.
func (c *RPCFallbackClient) markFailed(idx int, cooldown time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cooldown <= 0 {
		cooldown = failureCooldown
	}
	c.failures[idx] = time.Now().Add(cooldown)

	log.WithFields(log.Fields{
		"endpoint": c.endpoints[idx].Name,
		"url":      c.endpoints[idx].URL,
	}).Warn("RPC endpoint failed, entering cooldown")
}

// markSuccess clears the failure status for an endpoint.
func (c *RPCFallbackClient) markSuccess(idx int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.failures, idx)
}

func isRateLimitError(err error) bool {
	if err == nil {
		return false
	}

	var rpcErr *jsonrpc.RPCError
	if errors.As(err, &rpcErr) && rpcErr.Code == 429 {
		return true
	}

	var httpErr *jsonrpc.HTTPError
	if errors.As(err, &httpErr) && httpErr.Code == 429 {
		return true
	}

	return strings.Contains(strings.ToLower(err.Error()), "too many requests")
}

// withFallback executes a function against RPC endpoints with automatic failover.
// It tries endpoints in priority order, skipping those in cooldown.
func (c *RPCFallbackClient) withFallback(ctx context.Context, operation string, fn func(*rpc.Client) error) error {
	activeEndpoints := c.getActiveEndpoints()

	var lastErr error
	for _, idx := range activeEndpoints {
		client := c.clients[idx]
		endpoint := c.endpoints[idx]

		err := fn(client)
		if err == nil {
			c.markSuccess(idx)
			return nil
		}

		lastErr = err
		cooldown := failureCooldown
		if isRateLimitError(err) {
			cooldown = rateLimitCooldown
		}

		c.markFailed(idx, cooldown)

		log.WithFields(log.Fields{
			"endpoint":  endpoint.Name,
			"operation": operation,
			"error":     err.Error(),
		}).Info("RPC operation failed, trying next endpoint")
	}

	return fmt.Errorf("all RPC endpoints failed for %s: %w", operation, lastErr)
}

// GetBalance returns the SOL balance for an address with automatic failover.
func (c *RPCFallbackClient) GetBalance(ctx context.Context, address solanago.PublicKey) (uint64, error) {
	var balance uint64
	err := c.withFallback(ctx, "GetBalance", func(client *rpc.Client) error {
		resp, err := client.GetBalance(ctx, address, rpc.CommitmentFinalized)
		if err != nil {
			return err
		}
		balance = resp.Value
		return nil
	})
	return balance, err
}

// SendTransactionSkipPreflight submits WITHOUT the RPC preflight simulation. For
// a transaction that depends on very recently created/modified accounts (e.g. a
// subscription pull right after subscribe), the node's preflight simulates
// against a lagging bank and spuriously fails (InvalidAccountOwner / not-found)
// even though the transaction executes fine. Callers MUST confirm the signature
// afterward (WatchTransaction) since a skipped-preflight failure still lands.
func (c *RPCFallbackClient) SendTransactionSkipPreflight(ctx context.Context, tx *solanago.Transaction) (solanago.Signature, error) {
	if c.readOnly {
		return solanago.Signature{}, ErrProviderReadOnly
	}
	var sig solanago.Signature
	err := c.withFallback(ctx, "SendTransactionSkipPreflight", func(client *rpc.Client) error {
		resp, err := client.SendTransactionWithOpts(ctx, tx, rpc.TransactionOpts{
			SkipPreflight:       true,
			PreflightCommitment: rpc.CommitmentConfirmed,
		})
		if err != nil {
			return err
		}
		sig = resp
		return nil
	})
	return sig, err
}

// GetAccountData returns an account's raw data bytes, or (nil, nil) when the
// account does not exist. Uses Confirmed commitment so freshly-written state
// (e.g. a just-initialized PDA) is visible without waiting for finalization.
func (c *RPCFallbackClient) GetAccountData(ctx context.Context, address solanago.PublicKey) ([]byte, error) {
	var data []byte
	err := c.withFallback(ctx, "GetAccountData", func(client *rpc.Client) error {
		ai, err := client.GetAccountInfoWithOpts(ctx, address, &rpc.GetAccountInfoOpts{Commitment: rpc.CommitmentConfirmed})
		if err != nil {
			if errors.Is(err, rpc.ErrNotFound) {
				data = nil
				return nil
			}
			return err
		}
		if ai == nil || ai.Value == nil {
			data = nil
			return nil
		}
		data = ai.Value.Data.GetBinary()
		return nil
	})
	return data, err
}

// minContextSlotReadAttempts / Backoff bound the slot-gated read retry. A node
// that lags the requested minContextSlot returns an error; we retry the whole
// fallback chain (a different node may already be caught up) until one answers at
// or beyond the slot, or the bound is reached.
const minContextSlotReadAttempts = 8

// minContextSlotReadBackoff is a var (not const) only so tests can shrink it;
// production keeps the ~500ms pacing while lagging nodes catch up.
var minContextSlotReadBackoff = 500 * time.Millisecond

// GetAccountDataAtSlot is GetAccountData gated on minContextSlot: the read is
// evaluated only against a node that has reached `minSlot` (the slot our write
// confirmed at), so a lagging node returns a retryable error instead of stale
// data. minSlot == 0 behaves exactly like GetAccountData (no gating).
func (c *RPCFallbackClient) GetAccountDataAtSlot(ctx context.Context, address solanago.PublicKey, minSlot uint64) ([]byte, error) {
	if minSlot == 0 {
		return c.GetAccountData(ctx, address)
	}
	opts := &rpc.GetAccountInfoOpts{
		Commitment:     rpc.CommitmentConfirmed,
		MinContextSlot: &minSlot,
	}
	var data []byte
	read := func(ctx context.Context) error {
		return c.withFallback(ctx, "GetAccountDataAtSlot", func(client *rpc.Client) error {
			ai, err := client.GetAccountInfoWithOpts(ctx, address, opts)
			if err != nil {
				if errors.Is(err, rpc.ErrNotFound) {
					data = nil
					return nil
				}
				return err
			}
			if ai == nil || ai.Value == nil {
				data = nil
				return nil
			}
			data = ai.Value.Data.GetBinary()
			return nil
		})
	}
	if err := retryMinContextSlot(ctx, read); err != nil {
		return nil, err
	}
	return data, nil
}

// GetBalanceAtSlot is GetBalance gated on minContextSlot. The vendored
// getBalance lacks a MinContextSlot opts variant, so we read the slot the node
// served the response at (RPCContext.Context.Slot) and retry the fallback chain
// until a node answers at or beyond minSlot. minSlot == 0 == GetBalance.
func (c *RPCFallbackClient) GetBalanceAtSlot(ctx context.Context, address solanago.PublicKey, minSlot uint64) (uint64, error) {
	if minSlot == 0 {
		return c.GetBalance(ctx, address)
	}
	var balance uint64
	read := func(ctx context.Context) error {
		return c.withFallback(ctx, "GetBalanceAtSlot", func(client *rpc.Client) error {
			resp, err := client.GetBalance(ctx, address, rpc.CommitmentConfirmed)
			if err != nil {
				return err
			}
			if resp.Context.Slot < minSlot {
				return fmt.Errorf("minimum context slot has not been reached: served=%d want>=%d", resp.Context.Slot, minSlot)
			}
			balance = resp.Value
			return nil
		})
	}
	if err := retryMinContextSlot(ctx, read); err != nil {
		return 0, err
	}
	return balance, nil
}

// GetTokenAccountBalanceAtSlot is GetTokenAccountBalance gated on minContextSlot.
// As with GetBalanceAtSlot, the vendored method lacks a MinContextSlot opts
// variant, so we gate on the served RPCContext slot. minSlot == 0 ==
// GetTokenAccountBalance.
func (c *RPCFallbackClient) GetTokenAccountBalanceAtSlot(ctx context.Context, account solanago.PublicKey, minSlot uint64) (*rpc.GetTokenAccountBalanceResult, error) {
	if minSlot == 0 {
		return c.GetTokenAccountBalance(ctx, account)
	}
	var result *rpc.GetTokenAccountBalanceResult
	read := func(ctx context.Context) error {
		return c.withFallback(ctx, "GetTokenAccountBalanceAtSlot", func(client *rpc.Client) error {
			resp, err := client.GetTokenAccountBalance(ctx, account, rpc.CommitmentConfirmed)
			if err != nil {
				return err
			}
			if resp.Context.Slot < minSlot {
				return fmt.Errorf("minimum context slot has not been reached: served=%d want>=%d", resp.Context.Slot, minSlot)
			}
			result = resp
			return nil
		})
	}
	if err := retryMinContextSlot(ctx, read); err != nil {
		return nil, err
	}
	return result, nil
}

// retryMinContextSlot runs read() until it succeeds without a min-context-slot
// error, ctx is cancelled, or the attempt bound is reached. A min-context-slot
// error means every active node still lags the requested slot; we back off and
// retry (nodes catch up within a few hundred ms). Non-slot errors and success
// return immediately.
func retryMinContextSlot(ctx context.Context, read func(context.Context) error) error {
	var lastErr error
	for attempt := 0; attempt < minContextSlotReadAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(minContextSlotReadBackoff):
			}
		}
		err := read(ctx)
		if err == nil {
			return nil
		}
		lastErr = err
		if !isMinContextSlotError(err) {
			return err // a real error (not lag) — surface it immediately
		}
	}
	return fmt.Errorf("solana: node never reached minimum context slot after %d attempts: %w", minContextSlotReadAttempts, lastErr)
}

// GetLatestBlockhash gets the latest blockhash with automatic failover.
func (c *RPCFallbackClient) GetLatestBlockhash(ctx context.Context) (solanago.Hash, error) {
	var blockhash solanago.Hash
	err := c.withFallback(ctx, "GetLatestBlockhash", func(client *rpc.Client) error {
		resp, err := client.GetLatestBlockhash(ctx, rpc.CommitmentFinalized)
		if err != nil {
			return err
		}
		blockhash = resp.Value.Blockhash
		return nil
	})
	return blockhash, err
}

// GetMinimumBalanceForRentExemption returns the minimum balance with automatic failover.
func (c *RPCFallbackClient) GetMinimumBalanceForRentExemption(ctx context.Context, dataSize uint64) (uint64, error) {
	var balance uint64
	err := c.withFallback(ctx, "GetMinimumBalanceForRentExemption", func(client *rpc.Client) error {
		resp, err := client.GetMinimumBalanceForRentExemption(ctx, dataSize, rpc.CommitmentFinalized)
		if err != nil {
			return err
		}
		balance = resp
		return nil
	})
	return balance, err
}

// ErrProviderReadOnly is returned by every transaction-submission method when
// the provider is read-only (mode=readonly, #346). It is a hard failure of the
// requested operation, never a skip signal — mirrors nmi.ErrProviderReadOnly
// and stripeapi.ErrProviderReadOnly.
var ErrProviderReadOnly = errors.New("solana: transaction submission is blocked (mode=readonly)")

// SendTransaction submits a transaction with automatic failover.
func (c *RPCFallbackClient) SendTransaction(ctx context.Context, tx *solanago.Transaction) (solanago.Signature, error) {
	if c.readOnly {
		return solanago.Signature{}, ErrProviderReadOnly
	}
	var sig solanago.Signature
	err := c.withFallback(ctx, "SendTransaction", func(client *rpc.Client) error {
		resp, err := client.SendTransaction(ctx, tx)
		if err != nil {
			return err
		}
		sig = resp
		return nil
	})
	if err == nil {
		log.WithFields(log.Fields{
			"network": c.network,
		}).Info("Transaction sent to Solana")
	}
	return sig, err
}

// GetTransaction retrieves transaction details with automatic failover.
func (c *RPCFallbackClient) GetTransaction(ctx context.Context, signature solanago.Signature) (*rpc.GetTransactionResult, error) {
	var result *rpc.GetTransactionResult
	err := c.withFallback(ctx, "GetTransaction", func(client *rpc.Client) error {
		resp, err := client.GetTransaction(ctx, signature, &rpc.GetTransactionOpts{
			Commitment: rpc.CommitmentConfirmed,
			Encoding:   solanago.EncodingBase64,
		})
		if err != nil {
			return err
		}
		result = resp
		return nil
	})
	return result, err
}

// SimulateTransaction simulates a transaction with automatic failover.
func (c *RPCFallbackClient) SimulateTransaction(ctx context.Context, tx *solanago.Transaction) (*rpc.SimulateTransactionResponse, error) {
	var result *rpc.SimulateTransactionResponse
	err := c.withFallback(ctx, "SimulateTransaction", func(client *rpc.Client) error {
		resp, err := client.SimulateTransaction(ctx, tx)
		if err != nil {
			return err
		}
		result = resp
		return nil
	})
	return result, err
}

// GetSignatureStatuses gets signature statuses with automatic failover.
func (c *RPCFallbackClient) GetSignatureStatuses(ctx context.Context, searchHistory bool, sigs ...solanago.Signature) (*rpc.GetSignatureStatusesResult, error) {
	var result *rpc.GetSignatureStatusesResult
	err := c.withFallback(ctx, "GetSignatureStatuses", func(client *rpc.Client) error {
		resp, err := client.GetSignatureStatuses(ctx, searchHistory, sigs...)
		if err != nil {
			return err
		}
		result = resp
		return nil
	})
	return result, err
}

// GetTokenAccountBalance gets token account balance with automatic failover.
func (c *RPCFallbackClient) GetTokenAccountBalance(ctx context.Context, account solanago.PublicKey) (*rpc.GetTokenAccountBalanceResult, error) {
	var result *rpc.GetTokenAccountBalanceResult
	err := c.withFallback(ctx, "GetTokenAccountBalance", func(client *rpc.Client) error {
		resp, err := client.GetTokenAccountBalance(ctx, account, rpc.CommitmentFinalized)
		if err != nil {
			return err
		}
		result = resp
		return nil
	})
	return result, err
}

// GetSignaturesForAddressWithOpts gets signatures for an address with automatic failover.
func (c *RPCFallbackClient) GetSignaturesForAddressWithOpts(ctx context.Context, address solanago.PublicKey, opts *rpc.GetSignaturesForAddressOpts) ([]*rpc.TransactionSignature, error) {
	var result []*rpc.TransactionSignature
	err := c.withFallback(ctx, "GetSignaturesForAddress", func(client *rpc.Client) error {
		resp, err := client.GetSignaturesForAddressWithOpts(ctx, address, opts)
		if err != nil {
			return err
		}
		result = resp
		return nil
	})
	return result, err
}

// GetProgramAccountsWithOpts lists program-owned accounts with automatic failover.
func (c *RPCFallbackClient) GetProgramAccountsWithOpts(ctx context.Context, program solanago.PublicKey, opts *rpc.GetProgramAccountsOpts) (rpc.GetProgramAccountsResult, error) {
	var result rpc.GetProgramAccountsResult
	err := c.withFallback(ctx, "GetProgramAccounts", func(client *rpc.Client) error {
		resp, err := client.GetProgramAccountsWithOpts(ctx, program, opts)
		if err != nil {
			return err
		}
		result = resp
		return nil
	})
	return result, err
}

// GetEndpoint returns the primary endpoint URL (first in chain).
func (c *RPCFallbackClient) GetEndpoint() string {
	if len(c.endpoints) == 0 {
		return ""
	}
	return c.endpoints[0].URL
}
