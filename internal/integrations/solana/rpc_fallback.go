package solana

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	solanago "github.com/doujins-org/solana-go"
	"github.com/doujins-org/solana-go/rpc"
	"github.com/doujins-org/solana-go/rpc/jsonrpc"
	log "github.com/sirupsen/logrus"
)

// RPCEndpoint represents a single RPC endpoint with metadata.
type RPCEndpoint struct {
	Name     string // Human-readable name (e.g., "Helius", "Ankr", "Solana Public")
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
}

// RPCFallbackConfig holds configuration for building the fallback chain.
type RPCFallbackConfig struct {
	// CustomEndpoint bypasses the fallback chain entirely if set.
	CustomEndpoint string

	// HeliusAPIKey enables Helius as the primary RPC provider.
	HeliusAPIKey string

	// Network determines which endpoints to use (mainnet, devnet).
	Network string
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

	// Ankr (free, no rate limits advertised)
	endpoints = append(endpoints, RPCEndpoint{
		Name:     "Ankr",
		URL:      "https://rpc.ankr.com/solana",
		Priority: priority,
	})
	priority++

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

	// Ankr devnet
	endpoints = append(endpoints, RPCEndpoint{
		Name:     "Ankr Devnet",
		URL:      "https://rpc.ankr.com/solana_devnet",
		Priority: priority,
	})
	priority++

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
			endpoints = DefaultDevnetEndpoints(cfg.HeliusAPIKey)
		case "mainnet", "mainnet-beta":
			endpoints = DefaultMainnetEndpoints(cfg.HeliusAPIKey)
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
		endpoints: endpoints,
		clients:   clients,
		network:   network,
		failures:  make(map[int]time.Time),
	}
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

// SendTransaction submits a transaction with automatic failover.
func (c *RPCFallbackClient) SendTransaction(ctx context.Context, tx *solanago.Transaction) (solanago.Signature, error) {
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

// GetEndpoint returns the primary endpoint URL (first in chain).
func (c *RPCFallbackClient) GetEndpoint() string {
	if len(c.endpoints) == 0 {
		return ""
	}
	return c.endpoints[0].URL
}
