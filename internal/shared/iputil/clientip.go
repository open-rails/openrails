package iputil

import (
	"net"
	"net/http"
	"strings"
)

// TrustedProxies is the one proxy-aware client-IP resolver, built from the
// configured trusted proxies, so rate limits, abuse tracking, request logs,
// webhook IP recording and the CCBill allowlist all trust proxies identically.
//
// Empty trusts nothing: every resolution returns the socket peer. When the
// peer is trusted, ClientIP walks X-Forwarded-For right-to-left past trusted
// hops to the first untrusted address. Cloudflare's proxies are trusted too,
// and theirs is the only CF-IPCountry read.
type TrustedProxies struct {
	nets       []*net.IPNet
	cloudflare []*net.IPNet
}

// ParseTrustedProxies builds a resolver from the trusted proxies' and
// Cloudflare's CIDR strings. Malformed entries are skipped: config.Validate
// rejects them at boot.
func ParseTrustedProxies(trusted, cloudflare []string) *TrustedProxies {
	tp := &TrustedProxies{cloudflare: parseCIDRs(cloudflare)}
	tp.nets = append(parseCIDRs(trusted), tp.cloudflare...)
	return tp
}

func parseCIDRs(cidrs []string) []*net.IPNet {
	var out []*net.IPNet
	for _, raw := range cidrs {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if _, ipNet, err := net.ParseCIDR(raw); err == nil {
			out = append(out, ipNet)
		}
	}
	return out
}

func contains(nets []*net.IPNet, ip net.IP) bool {
	if ip == nil {
		return false
	}
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

func (t *TrustedProxies) trusts(ip net.IP) bool {
	return t != nil && contains(t.nets, ip)
}

// ClientIP returns r's real client IP. Every X-Forwarded-For line counts,
// joined in order as repeated fields combine (RFC 9110 §5.3): a proxy that
// appends its own line (HAProxy's option forwardfor) must not leave a
// client-written first line in charge. Nil-safe: a nil *TrustedProxies
// always returns the socket peer, matching "empty = trust nothing".
func (t *TrustedProxies) ClientIP(r *http.Request) string {
	return t.resolve(r.RemoteAddr, strings.Join(r.Header.Values("X-Forwarded-For"), ","))
}

// Country is the client's ISO country code from the CF-IPCountry header of
// the Cloudflare proxy the client connected to, or "" when no Cloudflare
// proxy received the client or the country is unknown.
func (t *TrustedProxies) Country(r *http.Request) string {
	if t == nil || !contains(t.cloudflare, t.receiver(r.RemoteAddr, strings.Join(r.Header.Values("X-Forwarded-For"), ","))) {
		return ""
	}
	country := strings.ToUpper(strings.TrimSpace(r.Header.Get("CF-IPCountry")))
	if len(country) != 2 || country == "XX" || country == "T1" {
		return ""
	}
	return country
}

// ForwardedHTTPS reports whether a trusted proxy says the client connection
// was HTTPS: the socket peer is trusted and every X-Forwarded-Proto value it
// relayed is https. An untrusted peer's header counts for nothing.
func (t *TrustedProxies) ForwardedHTTPS(r *http.Request) bool {
	if !t.trusts(net.ParseIP(hostOnly(r.RemoteAddr))) {
		return false
	}
	protos := strings.Split(strings.Join(r.Header.Values("X-Forwarded-Proto"), ","), ",")
	for _, proto := range protos {
		if !strings.EqualFold(strings.TrimSpace(proto), "https") {
			return false
		}
	}
	return true
}

// receiver is the trusted proxy the client connected to, walking as resolve
// does; nil when the peer is the client or every hop is trusted.
func (t *TrustedProxies) receiver(remoteAddr, forwardedFor string) net.IP {
	peer := net.ParseIP(hostOnly(remoteAddr))
	if !t.trusts(peer) {
		return nil
	}
	hops := splitForwardedFor(forwardedFor)
	for i := len(hops) - 1; i >= 0; i-- {
		hop := net.ParseIP(hops[i])
		if !t.trusts(hop) {
			return peer
		}
		peer = hop
	}
	return nil
}

// resolve walks forwardedFor, the joined X-Forwarded-For hops, back from
// remoteAddr, the transport-level peer ("host:port" or a bare host).
func (t *TrustedProxies) resolve(remoteAddr, forwardedFor string) string {
	peer := hostOnly(remoteAddr)
	if !t.trusts(net.ParseIP(peer)) {
		return peer
	}
	hops := splitForwardedFor(forwardedFor)
	for i := len(hops) - 1; i >= 0; i-- {
		if !t.trusts(net.ParseIP(hops[i])) {
			return hops[i]
		}
	}
	// Every hop is trusted: the direct peer is the best answer.
	return peer
}

// IPInAnyCIDR reports whether clientIP falls inside any of cidrs. For
// operator-declared source allowlists, NOT proxy trust. Malformed entries are
// skipped — config.Validate rejects them at boot.
func IPInAnyCIDR(clientIP string, cidrs []string) bool {
	ip := net.ParseIP(strings.TrimSpace(clientIP))
	if ip == nil {
		return false
	}
	for _, raw := range cidrs {
		if _, ipNet, err := net.ParseCIDR(strings.TrimSpace(raw)); err == nil && ipNet.Contains(ip) {
			return true
		}
	}
	return false
}

func hostOnly(remoteAddr string) string {
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		return host
	}
	return remoteAddr
}

func splitForwardedFor(header string) []string {
	if strings.TrimSpace(header) == "" {
		return nil
	}
	parts := strings.Split(header, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
