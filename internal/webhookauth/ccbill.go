// Package webhookauth holds the transport-level webhook authentication gates.
package webhookauth

import (
	"context"
	"log/slog"
	"net"
	"strings"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/logx"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/shared/iputil"
)

// LiveRailProbe answers "is this live?": the CCBill account a webhook names,
// or a request's merchant. A nil probe, an error, or LiveRailUnknown all mean
// "assume yes".
type LiveRailProbe func(ctx context.Context) (merchants.LiveRailPresence, error)

// CCBillIPAllowed is the CCBill webhook source-IP gate. CCBill signs nothing,
// so the source IP is this rail's only transport authentication.
//
// Accepted when EITHER:
//  1. the source is inside CCBill's documented ranges, or
//  2. both hold: the source is declared in
//     provider_sandbox.ccbill_webhook_sources, and
//     the database proves the account the webhook names moves no money.
//
// Anything unproven (no probe, probe error, LiveRailUnknown) refuses.
func CCBillIPAllowed(ctx context.Context, logger *slog.Logger, cfg *billing.Config, probe LiveRailProbe, clientIP string) bool {
	if iputil.IsValidCCBillIP(logger, clientIP) {
		return true
	}
	if !iputil.IPInAnyCIDR(clientIP, config.SandboxCCBillWebhookSources(cfg)) {
		return false
	}
	// Log only the parsed form: validates the header-derived value (no log
	// injection) and never echoes raw request bytes.
	logIP := "invalid"
	if ip := net.ParseIP(strings.TrimSpace(clientIP)); ip != nil {
		logIP = ip.String()
	}
	if probe == nil {
		logger.WarnContext(ctx, "ccbill webhook: declared allowlist entry refused - no account probe available", "client_ip", logIP)
		return false
	}
	presence, err := probe(ctx)
	if err != nil {
		logger.WarnContext(ctx, "ccbill webhook: account probe failed; refusing declared allowlist entry", logx.Err(err), "client_ip", logIP)
		return false
	}
	if presence != merchants.LiveRailAbsent {
		logger.WarnContext(ctx, "ccbill webhook: declared allowlist entry refused - the account is live or could not be ruled out", "client_ip", logIP, "live_psps", presence.String())
		return false
	}
	logger.DebugContext(ctx, "ccbill webhook: declared allowlist entry accepted - a sandbox account", "client_ip", logIP)
	return true
}
