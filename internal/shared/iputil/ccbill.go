package iputil

import (
	"fmt"
	"net"

	log "github.com/sirupsen/logrus"
)

// DefaultCCBillIPRanges are CCBill's documented webhook source ranges.
// Source: https://ccbill.com/doc/webhooks
//
// This IS the allowlist (#710): CCBill sends every merchant's webhooks from
// these provider-wide ranges, so there is nothing per-deployment or
// per-merchant to declare. A range rotation ships as a code change.
var DefaultCCBillIPRanges = []string{
	"64.38.212.0/24", // 64.38.212.1 - 64.38.212.254
	"64.38.215.0/24", // 64.38.215.1 - 64.38.215.254
	"64.38.240.0/24", // 64.38.240.1 - 64.38.240.254
	"64.38.241.0/24", // 64.38.241.1 - 64.38.241.254
}

// parsedCCBillRanges holds the pre-parsed CIDR networks for efficient lookups.
var parsedCCBillRanges = mustParseCIDRs(DefaultCCBillIPRanges)

func mustParseCIDRs(cidrs []string) []*net.IPNet {
	parsed := make([]*net.IPNet, 0, len(cidrs))
	for _, cidr := range cidrs {
		_, ipNet, err := net.ParseCIDR(cidr)
		if err != nil {
			panic(fmt.Sprintf("invalid CCBill CIDR %q: %v", cidr, err))
		}
		parsed = append(parsed, ipNet)
	}
	return parsed
}

// IsValidCCBillIP checks if the given IP address is within CCBill's authorized IP ranges
func IsValidCCBillIP(clientIP string) bool {
	if clientIP == "" {
		return false
	}

	ip := net.ParseIP(clientIP)
	if ip == nil {
		log.WithField("client_ip", clientIP).Warn("Failed to parse client IP address")
		return false
	}

	for _, ipNet := range parsedCCBillRanges {
		if ipNet.Contains(ip) {
			return true
		}
	}

	return false
}
