package iputil

import (
	"net"
	"strings"

	log "github.com/sirupsen/logrus"
)

// CCBill IP ranges as provided in their documentation
// Source: https://ccbill.com/doc/webhooks
var ccbillIPRanges = []string{
	"64.38.212.0/24", // 64.38.212.1 - 64.38.212.254
	"64.38.215.0/24", // 64.38.215.1 - 64.38.215.254
	"64.38.240.0/24", // 64.38.240.1 - 64.38.240.254
	"64.38.241.0/24", // 64.38.241.1 - 64.38.241.254
}

// parsedCCBillRanges holds the pre-parsed CIDR networks for performance
var parsedCCBillRanges []*net.IPNet

// init pre-parses the CIDR ranges for efficient lookups
func init() {
	for _, cidr := range ccbillIPRanges {
		_, ipNet, err := net.ParseCIDR(cidr)
		if err != nil {
			log.WithError(err).WithField("cidr", cidr).Fatal("Failed to parse CCBill IP range")
		}
		parsedCCBillRanges = append(parsedCCBillRanges, ipNet)
	}
}

// IsValidCCBillIP checks if the given IP address is within CCBill's authorized IP ranges
func IsValidCCBillIP(clientIP string) bool {
	if clientIP == "" {
		return false
	}

	// Handle potential proxy headers format like "ip1, ip2"
	// Take the first IP if comma-separated
	if strings.Contains(clientIP, ",") {
		clientIP = strings.TrimSpace(strings.Split(clientIP, ",")[0])
	}

	// Parse the client IP
	ip := net.ParseIP(clientIP)
	if ip == nil {
		log.WithField("client_ip", clientIP).Warn("Failed to parse client IP address")
		return false
	}

	// Check if IP falls within any of the CCBill ranges
	for _, ipNet := range parsedCCBillRanges {
		if ipNet.Contains(ip) {
			return true
		}
	}

	return false
}
