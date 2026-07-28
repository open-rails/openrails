// Package webhookauth holds the transport-level authentication gates shared by
// EVERY webhook ingress surface — the HTTP handlers and the embedded Service
// API. One gate, one place: SEC-19 found the embedded surface still running the
// pre-hardening version of the CCBill IP check because each surface owned its
// own copy.
package webhookauth

import (
	"context"
	"net"
	"strings"

	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/shared/iputil"
)

// LiveRailProbe answers "does a live PSP exist on this rail anywhere in the
// catalog?". A nil probe, an error, or LiveRailUnknown all mean "assume yes".
type LiveRailProbe func(ctx context.Context) (merchants.LiveRailPresence, error)

// CCBillIPAllowed is THE CCBill webhook source-IP gate. CCBill signs nothing —
// no HMAC, no shared secret on the callback — so the source IP is the only
// transport-level authentication that exists for this rail.
//
// Accepted when EITHER:
//  1. the source is inside CCBill's documented ranges, or
//  2. all three hold: the operator explicitly declared the source in
//     ccbill_webhook_ip_allowlist, the posture is sandbox (test_mode), and the
//     catalog PROVES no live CCBill PSP exists anywhere.
//
// Anything unproven — no probe wired, probe error, LiveRailUnknown — refuses.
// There is no test_mode-alone bypass any more (SEC-19): it read as protective
// while in fact accepting every source IP on earth, and the live-account guard
// that was supposed to constrain it could never fire under production RLS.
func CCBillIPAllowed(ctx context.Context, cfg *config.Config, probe LiveRailProbe, clientIP string) bool {
	if iputil.IsValidCCBillIP(clientIP) {
		return true
	}
	if cfg == nil || !cfg.IsTestMode() {
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
