package config

import (
	"fmt"
	"slices"
	"strings"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
)

// A PSP's environment is its rail's own: nmi live | test | sandbox, stripe
// live | test, ccbill live | sandbox, solana mainnet | devnet | testnet. Each
// rail's real-money environment is the default, and OpenRails verifies the
// declared one with the provider before use.
const (
	// EnvironmentLive moves real money (nmi, stripe, ccbill).
	EnvironmentLive = "live"
	// EnvironmentTest is the provider's real system with no money moving: an
	// NMI account in test mode, Stripe test keys.
	EnvironmentTest = "test"
	// EnvironmentSandbox is the provider's separate sandbox system:
	// sandbox.nmi.com, CCBill's sandbox.
	EnvironmentSandbox = "sandbox"
	// EnvironmentMainnet is Solana's real-money network.
	EnvironmentMainnet = "mainnet"
	// EnvironmentDevnet is Solana's devnet.
	EnvironmentDevnet = "devnet"
	// EnvironmentTestnet is Solana's testnet.
	EnvironmentTestnet = "testnet"
)

// A money class is what a PSP's environment means for money: live, or test
// for every environment that moves none. A PSP's identity and its custodians
// use it. A merchant's PSPs mix classes freely.
const (
	ClassLive = "live"
	ClassTest = "test"
)

// LiveEnvironment reports whether environment moves real money on rail.
func LiveEnvironment(rail, environment string) bool {
	return rails.LiveEnvironment(models.Rail(strings.ToLower(strings.TrimSpace(rail))), environment)
}

// EnvironmentClass is the money class of a PSP on rail in environment.
func EnvironmentClass(rail, environment string) string {
	if LiveEnvironment(rail, environment) {
		return ClassLive
	}
	return ClassTest
}

// ResolvePSPEnvironment is the environment a PSP on rail declares: its
// rail's real-money one when omitted. One the rail does not have is refused,
// naming the ones it has.
func ResolvePSPEnvironment(psp, rail, environment string) (string, error) {
	rail = strings.ToLower(strings.TrimSpace(rail))
	accepted := rails.Environments(models.Rail(rail))
	environment = strings.TrimSpace(environment)
	if environment == "" {
		return rails.DefaultEnvironment(models.Rail(rail)), nil
	}
	if !slices.Contains(accepted, environment) {
		return "", fmt.Errorf("psps.%s.environment: %s has no %q environment (%s)", psp, rail, environment, strings.Join(accepted, ", "))
	}
	return environment, nil
}

// FakeProviders names what cfg wires that no live PSP may sit beside:
// loopback provider gateways and an http HyperSwitch fixture.
func FakeProviders(cfg *Config) []string {
	if cfg == nil {
		return nil
	}
	var out []string
	for key, gateway := range map[string]string{"nmi_gateway_url": SandboxNMIGatewayURL(cfg), "stripe_api_url": SandboxStripeAPIURL(cfg), "solana_rpc_url": SandboxSolanaRPCURL(cfg), "ccbill_datalink_url": SandboxCCBillDataLinkURL(cfg)} {
		if gateway != "" {
			out = append(out, "provider_sandbox."+key)
		}
	}
	if cfg.HyperSwitch != nil && cfg.HyperSwitch.AllowLoopbackHTTP {
		out = append(out, "hyperswitch.allow_loopback_http")
	}
	slices.Sort(out)
	return out
}
