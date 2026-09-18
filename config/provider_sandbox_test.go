package config

import (
	"errors"
	"testing"
)

func sandboxConfig(posture CredentialPosture, gateway string) *Config {
	return &Config{
		Env: "dev", TestMode: posture, ProviderWriteMode: ProviderWriteModeFull,
		MerchantSource: MerchantSourceAPI, SecretBackend: SecretBackendDB,
		DB:              &DBConfig{URL: "postgresql://openrails:openrails@127.0.0.1:5432/openrails?sslmode=disable"},
		ProviderSandbox: &ProviderSandboxConfig{NMIGatewayURL: gateway},
	}
}

// A provider sandbox gateway receives store credentials, so only a literal
// loopback destination under a sandbox posture may be declared.
func TestProviderSandboxGatewayAcceptsOnlyLiteralLoopback(t *testing.T) {
	for _, gateway := range []string{
		"http://127.0.0.1:9091",
		"https://127.0.0.1",
		"http://127.0.0.1:9091/nmi",
		"http://127.255.0.7:80",
		"http://[::1]:9091",
		"http://[::ffff:127.0.0.1]:9091",
		" http://127.0.0.1:9091 ",
	} {
		if err := Validate(sandboxConfig(CredentialPostureSandbox, gateway)); err != nil {
			t.Errorf("%q: want accepted, got %v", gateway, err)
		}
	}
	if err := Validate(sandboxConfig(CredentialPostureSandbox, "")); err != nil {
		t.Errorf("empty gateway must be inert: %v", err)
	}
	if err := Validate(sandboxConfig(CredentialPostureLive, "")); err != nil {
		t.Errorf("empty gateway under live must be inert: %v", err)
	}
}

func TestProviderSandboxGatewayRefusals(t *testing.T) {
	for name, cfg := range map[string]*Config{
		"live posture":        sandboxConfig(CredentialPostureLive, "http://127.0.0.1:9091"),
		"hostname":            sandboxConfig(CredentialPostureSandbox, "http://localhost:9091"),
		"dns name":            sandboxConfig(CredentialPostureSandbox, "https://sandbox.nmi.com"),
		"private address":     sandboxConfig(CredentialPostureSandbox, "http://10.0.0.5:9091"),
		"link local":          sandboxConfig(CredentialPostureSandbox, "http://169.254.169.254"),
		"public address":      sandboxConfig(CredentialPostureSandbox, "http://93.184.216.34"),
		"ipv6 non-loopback":   sandboxConfig(CredentialPostureSandbox, "http://[2001:db8::1]:9091"),
		"unspecified address": sandboxConfig(CredentialPostureSandbox, "http://0.0.0.0:9091"),
		"userinfo":            sandboxConfig(CredentialPostureSandbox, "http://user:pass@127.0.0.1:9091"),
		"empty userinfo":      sandboxConfig(CredentialPostureSandbox, "http://@127.0.0.1:9091"),
		"scheme":              sandboxConfig(CredentialPostureSandbox, "ftp://127.0.0.1:9091"),
		"relative":            sandboxConfig(CredentialPostureSandbox, "127.0.0.1:9091"),
		"no host":             sandboxConfig(CredentialPostureSandbox, "http:///nmi"),
		"garbage":             sandboxConfig(CredentialPostureSandbox, "http://[::1"),
	} {
		err := Validate(cfg)
		if err == nil {
			t.Errorf("%s: want refusal", name)
			continue
		}
		if !errors.Is(err, ErrProviderSandboxGateway) {
			t.Errorf("%s: want ErrProviderSandboxGateway, got %v", name, err)
		}
	}
}
