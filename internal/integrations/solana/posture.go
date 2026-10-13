package solana

import (
	"context"
	"fmt"
	"strings"

	"github.com/open-rails/openrails/internal/providerposture"
	"github.com/open-rails/openrails/internal/shared/redact"
)

// Cluster genesis hashes are the authoritative identity of a Solana network.
const (
	mainnetGenesisHash = "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d"
	devnetGenesisHash  = "EtWTRABZaYq6iMfeYKouRu166VU2xqa1wcaWoxPkrZBG"
	testnetGenesisHash = "4uhcVJyU9pJkvQyS88uRDiswHXSCkY3zQawwpjk2NsNY"
)

// genesisNetworks names each cluster by its genesis hash.
var genesisNetworks = map[string]string{mainnetGenesisHash: "mainnet", devnetGenesisHash: "devnet", testnetGenesisHash: "testnet"}

// GenesisHash is network's genesis hash; "" for an unknown network.
func GenesisHash(network string) string {
	for hash, name := range genesisNetworks {
		if name == network {
			return hash
		}
	}
	return ""
}

// PostureKey binds a verdict to the exact RPC chain and its credentials.
func (c *RPCFallbackClient) PostureKey() providerposture.Key {
	urls := make([]string, 0, len(c.endpoints))
	secrets := make([]string, 0, len(c.endpoints))
	for _, ep := range c.endpoints {
		urls = append(urls, ep.URL)
		if ep.target != nil {
			secrets = append(secrets, ep.target.String())
		} else {
			secrets = append(secrets, ep.URL)
		}
	}
	key := providerposture.Key{Rail: "solana", AccountID: c.network, Endpoint: strings.Join(urls, " "), Credential: providerposture.Fingerprint(secrets...)}
	if c.network == "mainnet" {
		key.Expect = providerposture.Live
	}
	return key
}

// CheckPosture requires every endpoint in the chain to report the genesis hash
// of the PSP's network: mainnet's is live, devnet's and testnet's simulated.
func (c *RPCFallbackClient) CheckPosture(ctx context.Context) (providerposture.Verdict, error) {
	if len(c.clients) == 0 {
		return providerposture.Unknown, fmt.Errorf("solana: no RPC endpoints")
	}
	for i, client := range c.clients {
		hash, err := client.GetGenesisHash(ctx)
		if err != nil {
			return providerposture.Unknown, fmt.Errorf("solana genesis read (%s): %s", c.endpoints[i].Name, redact.Secrets(err.Error()))
		}
		network := genesisNetworks[hash.String()]
		switch {
		case network == "":
			return providerposture.Unknown, fmt.Errorf("solana endpoint %s has an unrecognized genesis hash", c.endpoints[i].Name)
		case network == c.network:
		case network == "mainnet":
			return providerposture.Live, fmt.Errorf("solana endpoint %s is on mainnet; the PSP declares %s", c.endpoints[i].Name, c.network)
		case c.network == "mainnet":
			return providerposture.Simulated, fmt.Errorf("solana endpoint %s is on %s; the PSP declares mainnet", c.endpoints[i].Name, network)
		default:
			return providerposture.Mismatched, fmt.Errorf("solana endpoint %s is on %s; the PSP declares %s", c.endpoints[i].Name, network, c.network)
		}
	}
	if c.network == "mainnet" {
		return providerposture.Live, nil
	}
	return providerposture.Simulated, nil
}

// VerifyPosture verifies the chain now and records the verdict.
func (c *RPCFallbackClient) VerifyPosture(ctx context.Context) providerposture.Status {
	return providerposture.Process().Verify(ctx, c.PostureKey(), c.CheckPosture)
}

// requireArmed refuses a submission until every endpoint is proven on the
// PSP's network.
func (c *RPCFallbackClient) requireArmed(ctx context.Context) error {
	return providerposture.Process().Require(ctx, c.PostureKey(), c.CheckPosture)
}
