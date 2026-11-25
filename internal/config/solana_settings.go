package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/open-rails/openrails/billing"

	solanago "github.com/gagliardetto/solana-go"
)

// A Solana PSP is its treasury, the address every payment to the merchant
// lands in, on the network its environment names. Its only secret is an
// optional RPC endpoint.
const SolanaSecretRPCURL = "rpc_url"

// ValidateSolanaTreasury refuses a treasury that is not a Solana address.
func ValidateSolanaTreasury(treasury string) error {
	treasury = strings.TrimSpace(treasury)
	if treasury == "" {
		return errors.New("treasury is required: the Solana address the merchant's payments land in")
	}
	key, err := solanago.PublicKeyFromBase58(treasury)
	if err != nil || key.IsZero() {
		return fmt.Errorf("treasury %q is not a Solana address (a base58 32-byte public key)", treasury)
	}
	return nil
}

// ValidateSolanaRPCURL refuses an rpc_url that is not an https URL (http only
// on a loopback host of a non-live PSP). The URL usually carries the
// provider's key, so no message repeats it.
func ValidateSolanaRPCURL(environment, raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !u.IsAbs() || u.Host == "" {
		return errors.New("secrets.rpc_url is not an absolute URL")
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		ip := net.ParseIP(u.Hostname())
		if ip != nil && ip.IsLoopback() && !LiveEnvironment("solana", environment) {
			return nil
		}
	}
	return errors.New("secrets.rpc_url must be https (http only on a loopback host of a non-live PSP)")
}

// SolanaRailConfig is a Solana PSP's runtime configuration.
type SolanaRailConfig struct {
	// Network is the PSP's environment: mainnet, devnet or testnet.
	Network string
	// RPCURL is the PSP's own RPC endpoint, tried before the network's public
	// one. A secret.
	RPCURL string
	// Tokens are the tokens the PSP accepts on Network, by symbol.
	Tokens map[string]TokenConfig
}

// Cranker modes.
const (
	CrankerVaultTransit = "vault_transit"
	CrankerLocalKeypair = "local_keypair"
)

// SolanaCrankerName names a cranker without its secret: its pin's key.
func SolanaCrankerName(c billing.SolanaCrankerConfig) string {
	if strings.TrimSpace(c.Mode) == CrankerVaultTransit {
		return CrankerVaultTransit + ":" + strings.TrimSpace(c.Key)
	}
	if key, err := solanago.PrivateKeyFromBase58(strings.TrimSpace(c.PrivateKey)); err == nil {
		return CrankerLocalKeypair + ":" + key.PublicKey().String()
	}
	return CrankerLocalKeypair
}

// SolanaTransitSigns reports whether a listed cranker signs in Vault Transit.
func SolanaTransitSigns(cfg *billing.Config) bool {
	for _, c := range SolanaCrankers(cfg) {
		if strings.TrimSpace(c.Mode) == CrankerVaultTransit {
			return true
		}
	}
	return false
}

// SolanaCrankers are cfg's crankers in order; none when it declares none.
func SolanaCrankers(cfg *billing.Config) []billing.SolanaCrankerConfig {
	if cfg == nil {
		return nil
	}
	return cfg.Solana.Crankers
}

// validateSolanaCranker refuses a cranker that cannot sign, or one listed
// before.
func validateSolanaCranker(cfg *billing.Config, listed []billing.SolanaCrankerConfig, c billing.SolanaCrankerConfig) error {
	switch strings.TrimSpace(c.Mode) {
	case CrankerVaultTransit:
		if strings.TrimSpace(c.Key) == "" || c.PrivateKey != "" {
			return errors.New("vault_transit names its Transit key and no private_key")
		}
	case CrankerLocalKeypair:
		if c.Key != "" {
			return errors.New("local_keypair takes private_key, not key")
		}
		if _, err := solanago.PrivateKeyFromBase58(strings.TrimSpace(c.PrivateKey)); err != nil {
			return errors.New("private_key is not a base58 Solana keypair")
		}
	default:
		return fmt.Errorf("mode must be %s or %s", CrankerVaultTransit, CrankerLocalKeypair)
	}
	for _, other := range listed {
		if SolanaCrankerName(other) == SolanaCrankerName(c) {
			return fmt.Errorf("%s is listed twice", SolanaCrankerName(c))
		}
	}
	return nil
}
