package middleware

import (
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/doujins-org/ginapi/response"
	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// CORS middleware with billing service specific settings
func CORS(allowedOrigins []string) gin.HandlerFunc {
	corsConfig := cors.DefaultConfig()

	// If no origins specified, allow all (development mode)
	if len(allowedOrigins) == 0 {
		corsConfig.AllowAllOrigins = true
	} else {
		corsConfig.AllowOrigins = allowedOrigins
	}

	// Billing service specific headers
	corsConfig.AllowHeaders = append(corsConfig.AllowHeaders,
		"Authorization",
		"X-Request-ID",
		"X-Forwarded-For",
		"X-Real-IP",
		"X-Idempotency-Key",
		"X-E2E-Run-ID",
		"Accept-Language",
	)

	corsConfig.AllowMethods = []string{
		http.MethodGet,
		http.MethodPost,
		http.MethodPut,
		http.MethodDelete,
		http.MethodOptions,
	}

	corsConfig.ExposeHeaders = []string{
		"X-Request-ID",
		"X-RateLimit-Remaining",
		"X-RateLimit-Reset",
	}

	corsConfig.AllowCredentials = true
	corsConfig.MaxAge = 12 * time.Hour

	return cors.New(corsConfig)
}

// InternalIPWhitelist restricts access to internal networks only (not used by default)
func InternalIPWhitelist() gin.HandlerFunc {
	// Define internal network ranges
	internalNetworks := []*net.IPNet{
		{IP: net.ParseIP("10.0.0.0"), Mask: net.CIDRMask(8, 32)},     // 10.0.0.0/8
		{IP: net.ParseIP("172.16.0.0"), Mask: net.CIDRMask(12, 32)},  // 172.16.0.0/12
		{IP: net.ParseIP("192.168.0.0"), Mask: net.CIDRMask(16, 32)}, // 192.168.0.0/16
		{IP: net.ParseIP("127.0.0.0"), Mask: net.CIDRMask(8, 32)},    // 127.0.0.0/8 (loopback)
	}

	return func(c *gin.Context) {
		clientIP := getClientIP(c)

		// Parse client IP
		ip := net.ParseIP(clientIP)
		if ip == nil {
			log.WithField("client_ip", clientIP).Error("Failed to parse client IP")
			response.ForbiddenWithMessage(c, "Access denied")
			c.Abort()
			return
		}

		// Check if IP is in internal networks
		isInternal := false
		for _, network := range internalNetworks {
			if network.Contains(ip) {
				isInternal = true
				break
			}
		}

		if !isInternal {
			log.WithField("client_ip", clientIP).Warn("External IP attempted to access internal endpoint")
			response.ForbiddenWithMessage(c, "Access denied")
			c.Abort()
			return
		}

		c.Next()
	}
}

// WebhookIPWhitelist middleware restricts webhook endpoints to allowed IPs
func WebhookIPWhitelist(allowedIPs []string) gin.HandlerFunc {
	// Parse allowed IPs and networks
	var allowedNetworks []*net.IPNet
	var allowedAddresses []net.IP

	for _, ipStr := range allowedIPs {
		if strings.Contains(ipStr, "/") {
			// CIDR notation
			_, network, err := net.ParseCIDR(ipStr)
			if err != nil {
				log.WithError(err).WithField("ip", ipStr).Error("Failed to parse CIDR")
				continue
			}
			allowedNetworks = append(allowedNetworks, network)
		} else {
			// Single IP address
			ip := net.ParseIP(ipStr)
			if ip == nil {
				log.WithField("ip", ipStr).Error("Failed to parse IP address")
				continue
			}
			allowedAddresses = append(allowedAddresses, ip)
		}
	}

	return func(c *gin.Context) {
		clientIP := getClientIP(c)

		// Parse client IP
		ip := net.ParseIP(clientIP)
		if ip == nil {
			log.WithField("client_ip", clientIP).Error("Failed to parse client IP")
			response.ForbiddenWithMessage(c, "Access denied")
			c.Abort()
			return
		}

		// Check against allowed addresses
		for _, allowedIP := range allowedAddresses {
			if ip.Equal(allowedIP) {
				c.Next()
				return
			}
		}

		// Check against allowed networks
		for _, network := range allowedNetworks {
			if network.Contains(ip) {
				c.Next()
				return
			}
		}

		log.WithField("client_ip", clientIP).Warn("Webhook request from non-whitelisted IP")
		response.ForbiddenWithMessage(c, "Access denied")
		c.Abort()
	}
}

// SecurityHeaders adds security headers to responses
func SecurityHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Security headers for billing service
		c.Header("X-Frame-Options", "DENY")
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("X-XSS-Protection", "1; mode=block")
		c.Header("Referrer-Policy", "strict-origin-when-cross-origin")
		c.Header("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; font-src 'self'")

		// Remove server information
		c.Header("Server", "")

		c.Next()
	}
}

// getClientIP extracts the real client IP from the request
func getClientIP(c *gin.Context) string {
	// Check X-Forwarded-For header (from load balancers)
	if xForwardedFor := c.GetHeader("X-Forwarded-For"); xForwardedFor != "" {
		ips := strings.Split(xForwardedFor, ",")
		if len(ips) > 0 {
			return strings.TrimSpace(ips[0])
		}
	}

	// Check X-Real-IP header (from reverse proxies)
	if xRealIP := c.GetHeader("X-Real-IP"); xRealIP != "" {
		return xRealIP
	}

	// Fallback to connection remote address
	return c.ClientIP()
}
