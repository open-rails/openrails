// Package webhookauth holds the transport-level webhook authentication gates.
package webhookauth

import (
	"context"
	"net"
	"strings"

	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/shared/iputil"
)

// LiveRailProbe answers "does a live PSP exist on this rail anywhere in the
// catalog?". A nil probe, an error, or LiveRailUnknown all mean "assume yes".
type LiveRailProbe func(ctx context.Context) (merchants.LiveRailPresence, error)

// CCBillIPAllowed is the CCBill webhook source-IP gate. CCBill signs nothing,
// so the source IP is this rail's only transport authentication.
//
// Accepted when EITHER:
//  1. the source is inside CCBill's documented ranges, or
//  2. all three hold: the source is declared in ccbill_webhook_ip_allowlist,
//     the posture is sandbox (test_mode), and the catalog proves no live
//     CCBill PSP exists anywhere.
//
// Anything unproven (no probe, probe error, LiveRailUnknown) refuses; test_mode
// alone never bypasses the gate.
func CCBillIPAllowed(ctx context.Context, cfg *config.Config, probe LiveRailProbe, clientIP string) bool {
	if iputil.IsValidCCBillIP(clientIP) {
		return true
	}
	if cfg == nil || !config.IsTestMode(cfg) {
		return false
	}
	if !iputil.IPInAnyCIDR(clientIP, cfg.CCBillWebhookIPAllowlist) {
		return false
	}
	// Log only the parsed form: validates the header-derived value (no log
	// injection) and never echoes raw request bytes.
	logIP := "invalid"
	if ip := net.ParseIP(strings.TrimSpace(clientIP)); ip != nil {
		logIP = ip.String()
	}
	if probe == nil {
		log.WithField("client_ip", logIP).Warn("ccbill webhook: declared allowlist entry refused - no live-psp probe available")
		return false
	}
	presence, err := probe(ctx)
	if err != nil {
		log.WithError(err).WithField("client_ip", logIP).Warn("ccbill webhook: live-psp probe failed; refusing declared allowlist entry")
		return false
	}
	if presence != merchants.LiveRailAbsent {
		log.WithFields(log.Fields{"client_ip": logIP, "live_psps": presence.String()}).
			Warn("ccbill webhook: declared allowlist entry refused - live ccbill psp exists or could not be ruled out")
		return false
	}
	log.WithField("client_ip", logIP).Debug("ccbill webhook: declared allowlist entry accepted - sandbox posture, no live ccbill psp")
	return true
}
