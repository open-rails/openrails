package iputil

import (
	"fmt"
	"net"
	"sync"

	log "github.com/sirupsen/logrus"
)

// DefaultCCBillIPRanges are CCBill's documented webhook source ranges.
// Source: https://ccbill.com/doc/webhooks
//
// These are used as a fallback only. The allowlist is intended to be supplied
// at boot via configuration (rails.ccbill.allowed_cidrs / env var) so the
// ranges can be rotated without a code deploy. See Configure.
var DefaultCCBillIPRanges = []string{
	"64.38.212.0/24", // 64.38.212.1 - 64.38.212.254
	"64.38.215.0/24", // 64.38.215.1 - 64.38.215.254
	"64.38.240.0/24", // 64.38.240.1 - 64.38.240.254
	"64.38.241.0/24", // 64.38.241.1 - 64.38.241.254
}

var (
	// rangesMu guards parsedCCBillRanges, which may be replaced at boot by Configure.
	rangesMu sync.RWMutex
	// parsedCCBillRanges holds the pre-parsed CIDR networks for efficient lookups.
	parsedCCBillRanges []*net.IPNet
)

// init pre-parses the default ranges so the allowlist is usable even if Configure
// is never called (e.g. in tests or tooling that does not load full config).
func init() {
	parsed, err := parseCIDRs(DefaultCCBillIPRanges)
	if err != nil {
		log.WithError(err).Fatal("Failed to parse default CCBill IP ranges")
	}
	parsedCCBillRanges = parsed
}

// parseCIDRs parses a slice of CIDR strings, failing on the first invalid entry.
func parseCIDRs(cidrs []string) ([]*net.IPNet, error) {
	parsed := make([]*net.IPNet, 0, len(cidrs))
	for _, cidr := range cidrs {
		_, ipNet, err := net.ParseCIDR(cidr)
		if err != nil {
			return nil, fmt.Errorf("invalid CCBill CIDR %q: %w", cidr, err)
		}
		parsed = append(parsed, ipNet)
	}
	return parsed, nil
}

// Configure replaces the active CCBill allowlist with the supplied CIDR ranges.
// It is intended to be called once at boot from configuration. All entries are
// parsed up front and an error is returned (fail-fast) if any entry is invalid,
// so the caller can abort startup rather than running with a partial allowlist.
//
// When cidrs is empty, the documented DefaultCCBillIPRanges are used so a missing
// config value fails closed to CCBill's published ranges rather than allowing all.
func Configure(cidrs []string) error {
	source := cidrs
	if len(source) == 0 {
		source = DefaultCCBillIPRanges
	}
	parsed, err := parseCIDRs(source)
	if err != nil {
		return err
	}
	rangesMu.Lock()
	parsedCCBillRanges = parsed
	rangesMu.Unlock()
	log.WithField("cidr_count", len(parsed)).Info("CCBill webhook IP allowlist configured")
	return nil
}

// IsValidCCBillIP checks if the given IP address is within CCBill's authorized IP ranges
func IsValidCCBillIP(clientIP string) bool {
	if clientIP == "" {
		return false
	}

	// Parse the client IP
	ip := net.ParseIP(clientIP)
	if ip == nil {
		log.WithField("client_ip", clientIP).Warn("Failed to parse client IP address")
		return false
	}

	rangesMu.RLock()
	ranges := parsedCCBillRanges
	rangesMu.RUnlock()

	// Check if IP falls within any of the configured CCBill ranges
	for _, ipNet := range ranges {
		if ipNet.Contains(ip) {
			return true
		}
	}

	return false
}
