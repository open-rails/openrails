package webhookauth

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/merchants"
)

// SEC-19: CCBill signs nothing, so the source IP is its only authentication.
// Outside CCBill's ranges a source passes only when declared AND sandbox AND the
// catalog proves no live CCBill PSP exists; anything unproven refuses.
func TestCCBillIPAllowed(t *testing.T) {
	probe := func(p merchants.LiveRailPresence, err error) LiveRailProbe {
		return func(context.Context) (merchants.LiveRailPresence, error) { return p, err }
	}
	cfg := func(posture config.CredentialPosture, allowlist ...string) *config.Config {
		return &config.Config{TestMode: posture, CCBillWebhookIPAllowlist: allowlist}
	}
	sandbox, live := config.CredentialPostureSandbox, config.CredentialPostureLive
	absent := probe(merchants.LiveRailAbsent, nil)
	down := errors.New("db down")
	const declared, forged = "127.0.0.1", "203.0.113.9"

	for _, tc := range []struct {
		name  string
		cfg   *config.Config
		probe LiveRailProbe
		ip    string
		want  bool
	}{
		{"ccbill range, no config", nil, nil, "64.38.212.5", true},
		{"ccbill range, live psp present", cfg(sandbox), probe(merchants.LiveRailPresent, nil), "64.38.240.1", true},
		{"declared + sandbox + proven absent", cfg(sandbox, "127.0.0.1/32"), absent, declared, true},
		{"no config", nil, absent, forged, false},
		{"live posture", cfg(live, "127.0.0.1/32"), absent, declared, false},
		{"not declared", cfg(sandbox), absent, declared, false},
		{"other source than declared", cfg(sandbox, "127.0.0.1/32"), absent, forged, false},
		{"forwarded list is not an address", cfg(sandbox, "127.0.0.1/32"), absent, "127.0.0.1, 64.38.212.5", false},
		{"probe unknown", cfg(sandbox, "127.0.0.1/32"), probe(merchants.LiveRailUnknown, nil), declared, false},
		{"probe errored with absent", cfg(sandbox, "127.0.0.1/32"), probe(merchants.LiveRailAbsent, down), declared, false},
		{"no probe wired", cfg(sandbox, "127.0.0.1/32"), nil, declared, false},
		{"live psp present", cfg(sandbox, "127.0.0.1/32"), probe(merchants.LiveRailPresent, nil), declared, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, CCBillIPAllowed(context.Background(), tc.cfg, tc.probe, tc.ip))
		})
	}
	var zero merchants.LiveRailPresence
	require.Equal(t, merchants.LiveRailUnknown, zero, "the zero value must be the fail-closed one")
}
