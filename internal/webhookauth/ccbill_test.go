package webhookauth

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/merchants"
)

func probe(p merchants.LiveRailPresence, err error) LiveRailProbe {
	return func(context.Context) (merchants.LiveRailPresence, error) { return p, err }
}

func sandbox(allowlist ...string) *config.Config {
	return &config.Config{TestMode: config.CredentialPostureSandbox, CCBillWebhookIPAllowlist: allowlist}
}

// SEC-19: the old gate keyed the bypass on test_mode alone and defaulted an
// unproven live-account probe to "no live accounts". Both are inverted now.
func TestCCBillIPAllowed(t *testing.T) {
	ctx := context.Background()
	const declared = "127.0.0.1"
	const forged = "203.0.113.9"

	// CCBill's own ranges always pass, whatever the posture or catalog says.
	require.True(t, CCBillIPAllowed(ctx, nil, nil, "64.38.212.5"))
	require.True(t, CCBillIPAllowed(ctx, sandbox(), probe(merchants.LiveRailPresent, nil), "64.38.240.1"))

	// No config, no posture, no bypass.
	require.False(t, CCBillIPAllowed(ctx, nil, probe(merchants.LiveRailAbsent, nil), forged))
	require.False(t, CCBillIPAllowed(ctx,
		&config.Config{TestMode: config.CredentialPostureLive, CCBillWebhookIPAllowlist: []string{"127.0.0.1/32"}},
		probe(merchants.LiveRailAbsent, nil), declared))

	// Sandbox + provably-no-live-PSP is not enough: the source must be declared.
	require.False(t, CCBillIPAllowed(ctx, sandbox(), probe(merchants.LiveRailAbsent, nil), declared))
	require.False(t, CCBillIPAllowed(ctx, sandbox("127.0.0.1/32"), probe(merchants.LiveRailAbsent, nil), forged))

	// All three conditions met.
	require.True(t, CCBillIPAllowed(ctx, sandbox("127.0.0.1/32"), probe(merchants.LiveRailAbsent, nil), declared))

	// FAIL CLOSED on anything unproven, even with the source declared.
	cfg := sandbox("127.0.0.1/32")
	require.False(t, CCBillIPAllowed(ctx, cfg, probe(merchants.LiveRailUnknown, nil), declared),
		"a probe that proves nothing must mean live PSPs may exist")
	require.False(t, CCBillIPAllowed(ctx, cfg, probe(merchants.LiveRailUnknown, errors.New("db down")), declared))
	require.False(t, CCBillIPAllowed(ctx, cfg, probe(merchants.LiveRailAbsent, errors.New("db down")), declared),
		"an errored probe's value must never be trusted")
	require.False(t, CCBillIPAllowed(ctx, cfg, nil, declared), "no probe wired = no bypass")
	require.False(t, CCBillIPAllowed(ctx, cfg, probe(merchants.LiveRailPresent, nil), declared))

	// The tri-state zero value is the fail-closed one.
	var zero merchants.LiveRailPresence
	require.Equal(t, merchants.LiveRailUnknown, zero)
}
