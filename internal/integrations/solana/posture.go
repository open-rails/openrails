package solana

import (
	"context"
	"fmt"
	"strings"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/providerposture"
)

// Cluster genesis hashes are the authoritative identity of a Solana network.
const (
	mainnetGenesisHash = "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d"
	devnetGenesisHash  = "EtWTRABZaYq6iMfeYKouRu166VU2xqa1wcaWoxPkrZBG"
	testnetGenesisHash = "4uhcVJyU9pJkvQyS88uRDiswHXSCkY3zQawwpjk2NsNY"
)

func (c *RPCFallbackClient) sandbox() bool {
	return c.network != "mainnet" && c.network != "mainnet-beta"
}

// PostureKey binds a verdict to the exact RPC chain and its credentials.
func (c *RPCFallbackClient) PostureKey() providerposture.Key {
	urls := make([]string, 0, len(c.endpoints))
	secrets := make([]string, 0, len(c.endpoints))
	for _, ep := range c.endpoints {
		urls = append(urls, ep.URL)
		secrets = append(secrets, ep.URL+"?"+ep.secret.Encode())
	}
	return providerposture.Key{Rail: "solana", AccountID: c.network, Endpoint: strings.Join(urls, " "), Credential: providerposture.Fingerprint(secrets...)}
}

// CheckPosture requires every endpoint in the chain to report a devnet or
// testnet genesis hash; mainnet is live and anything else is unknown.
func (c *RPCFallbackClient) CheckPosture(ctx context.Context) (providerposture.Verdict, error) {
	if len(c.clients) == 0 {
		return providerposture.Unknown, fmt.Errorf("solana: no RPC endpoints")
	}
	for i, client := range c.clients {
		hash, err := client.GetGenesisHash(ctx)
		if err != nil {
			return providerposture.Unknown, fmt.Errorf("solana genesis read (%s): %w", c.endpoints[i].Name, err)
		}
		switch hash.String() {
		case devnetGenesisHash, testnetGenesisHash:
		case mainnetGenesisHash:
			return providerposture.Live, fmt.Errorf("solana endpoint %s is mainnet", c.endpoints[i].Name)
		default:
			return providerposture.Unknown, fmt.Errorf("solana endpoint %s has an unrecognized genesis hash", c.endpoints[i].Name)
		}
	}
	return providerposture.Simulated, nil
}

// VerifyPosture verifies the chain now and records the verdict.
func (c *RPCFallbackClient) VerifyPosture(ctx context.Context) providerposture.Status {
	if !c.sandbox() || c.loopbackFixture {
		return providerposture.Status{Key: c.PostureKey(), Verdict: providerposture.Simulated}
	}
	return providerposture.Process().Verify(ctx, c.PostureKey(), c.CheckPosture)
}

func (c *RPCFallbackClient) requireArmed(ctx context.Context) error {
	if !c.sandbox() {
		return nil
	}
	if c.loopbackFixture {
		for _, ep := range c.endpoints {
			if err := config.ValidateLoopbackGatewayURL(ep.URL); err != nil {
				return fmt.Errorf("%w: loopback fixture: %w", providerposture.ErrDisarmed, err)
			}
		}
		return nil
	}
	return providerposture.Process().Require(ctx, c.PostureKey(), c.CheckPosture)
}
