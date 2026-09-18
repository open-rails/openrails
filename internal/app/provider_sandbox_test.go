package app

import (
	"context"
	"strings"
	"testing"

	"github.com/open-rails/openrails/config"
)

func TestSandboxGatewayRefusalPrecedesRuntimeConstruction(t *testing.T) {
	for _, tc := range []struct {
		posture config.CredentialPosture
		gateway string
	}{
		{config.CredentialPostureLive, "http://127.0.0.1:8123"},
		{config.CredentialPostureSandbox, "https://example.com"},
	} {
		cfg := &config.Config{Env: "dev", TestMode: tc.posture, ProviderSandbox: &config.ProviderSandboxConfig{NMIGatewayURL: tc.gateway}}
		_, err := buildRuntimeWithOverrides(context.Background(), cfg, nil)
		if err == nil || !strings.Contains(err.Error(), "provider_sandbox") {
			t.Fatalf("runtime reached database construction before refusing sandbox destination: %v", err)
		}
	}
}
