package config

import "testing"

func TestProviderSandboxRequiresLiteralLoopbackAndSandboxPosture(t *testing.T) {
	for _, gateway := range []string{"http://127.0.0.1:8123", "https://[::1]:8123/"} {
		cfg := &Config{TestMode: CredentialPostureSandbox, ProviderSandbox: &ProviderSandboxConfig{NMIGatewayURL: gateway}}
		if _, err := cfg.SandboxNMIGatewayURL(); err != nil {
			t.Fatal(err)
		}
		cfg.TestMode = CredentialPostureLive
		if _, err := cfg.SandboxNMIGatewayURL(); err == nil {
			t.Fatal("live configuration accepted a sandbox gateway")
		}
	}
	for _, gateway := range []string{"https://example.com", "http://localhost:8123", "http://10.0.0.1", "http://user:pass@127.0.0.1", "http://127.0.0.1/path", "http://127.0.0.1?key=value", "http://127.0.0.1#fragment", "file:///etc/passwd"} {
		cfg := &Config{TestMode: CredentialPostureSandbox, ProviderSandbox: &ProviderSandboxConfig{NMIGatewayURL: gateway}}
		if _, err := cfg.SandboxNMIGatewayURL(); err == nil {
			t.Fatalf("accepted non-loopback sandbox destination %q", gateway)
		}
	}
}
