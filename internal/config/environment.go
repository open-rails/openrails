package config

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
)

// A PSP's environment: whether its credentials move real money. Every PSP
// declares one and OpenRails verifies it with the provider before use.
const (
	// EnvironmentLive moves real money.
	EnvironmentLive = "live"
	// EnvironmentTest is the provider's real system with no money moving: an
	// NMI account in test mode, Stripe test keys.
	EnvironmentTest = "test"
	// EnvironmentSandbox is the provider's separate sandbox system:
	// sandbox.nmi.com, CCBill's sandbox, Solana devnet.
	EnvironmentSandbox = "sandbox"
)

// LiveEnvironment reports whether environment moves real money.
func LiveEnvironment(environment string) bool { return environment == EnvironmentLive }

// EnvironmentClass is the money class a custodian records beside its PSPs:
// live, or test for both non-live environments.
func EnvironmentClass(environment string) string {
	if LiveEnvironment(environment) {
		return EnvironmentLive
	}
	return EnvironmentTest
}

// ValidatePSPEnvironment refuses a missing environment and one the rail does
// not have.
func ValidatePSPEnvironment(psp, rail, environment string) error {
	accepted := rails.Environments(models.Rail(rail))
	if strings.TrimSpace(environment) == "" {
		return fmt.Errorf("psps.%s.environment is required: is the PSP live (real money) or not (%s)", psp, strings.Join(accepted, ", "))
	}
	if !slices.Contains(accepted, environment) {
		return fmt.Errorf("psps.%s.environment: %s has no %q environment (%s)", psp, rail, environment, strings.Join(accepted, ", "))
	}
	return nil
}

// ValidateDeploymentEnvironments refuses a deployment whose PSPs mix live and
// non-live environments. environments names each PSP (merchant/key).
func ValidateDeploymentEnvironments(environments map[string]string) error {
	var live, other []string
	for _, name := range slices.Sorted(maps.Keys(environments)) {
		if LiveEnvironment(environments[name]) {
			live = append(live, name)
		} else {
			other = append(other, name+" ("+environments[name]+")")
		}
	}
	if len(live) > 0 && len(other) > 0 {
		return fmt.Errorf("a deployment is all live or all non-live: live PSPs %s beside non-live PSPs %s", strings.Join(live, ", "), strings.Join(other, ", "))
	}
	return nil
}

// DeclaredEnvironments names the environment of every PSP the merchants
// declare, as merchant/key.
func DeclaredEnvironments(merchants []MerchantDeclaration) map[string]string {
	out := map[string]string{}
	for _, m := range merchants {
		for key, p := range m.PSPs {
			out[m.Slug+"/"+strings.ToLower(strings.TrimSpace(key))] = p.Environment
		}
	}
	return out
}

// FakeProviders names what cfg wires that only a non-live deployment may
// use: loopback provider gateways and an http HyperSwitch fixture.
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

// SolanaNetwork is the network a Solana PSP in environment pays on: devnet
// for sandbox, mainnet for live.
func SolanaNetwork(environment string) string {
	if environment == EnvironmentSandbox {
		return "devnet"
	}
	return "mainnet"
}
