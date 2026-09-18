package config

import (
	"errors"
	"testing"
)

func providerSandboxConfig(posture CredentialPosture, sandbox ProviderSandboxConfig) *Config {
	return &Config{
		Env: "dev", TestMode: posture, ProviderWriteMode: ProviderWriteModeFull,
		MerchantSource: MerchantSourceAPI, SecretBackend: SecretBackendDB,
		DB:              &DBConfig{URL: "postgresql://openrails:openrails@127.0.0.1:5432/openrails?sslmode=disable"},
		ProviderSandbox: &sandbox,
	}
}

// Provider credentials are sent to a sandbox destination, so each one
// (stripe_api_url, nmi_gateway_url) must be a literal loopback IP under a
// sandbox posture.
func TestProviderSandboxDestinationsAcceptOnlyLiteralLoopback(t *testing.T) {
	for _, destination := range []string{
		"http://127.0.0.1:9092",
		"https://127.0.0.1",
		"http://127.0.0.1:9092/v1",
		"http://127.255.0.7:80",
		"http://[::1]:9092",
		"http://[::ffff:127.0.0.1]:9092",
		" http://127.0.0.1:9092 ",
	} {
		for key, sandbox := range map[string]ProviderSandboxConfig{"stripe_api_url": {StripeAPIURL: destination}, "nmi_gateway_url": {NMIGatewayURL: destination}} {
			if err := Validate(providerSandboxConfig(CredentialPostureSandbox, sandbox)); err != nil {
				t.Errorf("%s %q: want accepted, got %v", key, destination, err)
			}
		}
	}
	if err := Validate(providerSandboxConfig(CredentialPostureLive, ProviderSandboxConfig{})); err != nil {
		t.Errorf("an empty sandbox is inert under live: %v", err)
	}
}

func TestProviderSandboxDestinationRefusals(t *testing.T) {
	for name, destination := range map[string]string{
		"hostname":            "http://localhost:9092",
		"dns name":            "https://api.stripe.com",
		"private address":     "http://10.0.0.5:9092",
		"link local":          "http://169.254.169.254",
		"public address":      "http://93.184.216.34",
		"ipv6 non-loopback":   "http://[2001:db8::1]:9092",
		"unspecified address": "http://0.0.0.0:9092",
		"userinfo":            "http://user:pass@127.0.0.1:9092",
		"empty userinfo":      "http://@127.0.0.1:9092",
		"scheme":              "ftp://127.0.0.1:9092",
		"relative":            "127.0.0.1:9092",
		"no host":             "http:///v1",
		"garbage":             "http://[::1",
	} {
		for key, sandbox := range map[string]ProviderSandboxConfig{"stripe_api_url": {StripeAPIURL: destination}, "nmi_gateway_url": {NMIGatewayURL: destination}} {
			if err := Validate(providerSandboxConfig(CredentialPostureSandbox, sandbox)); !errors.Is(err, ErrProviderSandboxGateway) {
				t.Errorf("%s %s %q: want ErrProviderSandboxGateway, got %v", key, name, destination, err)
			}
		}
	}
	for key, sandbox := range map[string]ProviderSandboxConfig{"stripe_api_url": {StripeAPIURL: "http://127.0.0.1:9092"}, "nmi_gateway_url": {NMIGatewayURL: "http://127.0.0.1:9091"}} {
		if err := Validate(providerSandboxConfig(CredentialPostureLive, sandbox)); !errors.Is(err, ErrProviderSandboxGateway) {
			t.Errorf("%s under live: want ErrProviderSandboxGateway, got %v", key, err)
		}
	}
}
