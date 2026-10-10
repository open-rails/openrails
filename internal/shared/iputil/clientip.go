package iputil

import (
	"net"
	"net/http"
	"strings"
)

// TrustedProxies is the one proxy-aware client-IP resolver, built from the
// configured trusted proxies, so rate limits, abuse tracking, webhook IP
// recording and the CCBill allowlist all trust proxies identically.
//
// Empty trusts nothing: every resolution returns the socket peer. When the
// peer is trusted, ClientIP walks X-Forwarded-For right-to-left past trusted
// hops to the first untrusted address.
type TrustedProxies struct {
	nets []*net.IPNet
}

// ParseTrustedProxies builds a resolver from CIDR strings. Malformed entries
// are skipped: config.Validate rejects them at boot.
func ParseTrustedProxies(cidrs []string) *TrustedProxies {
	tp := &TrustedProxies{}
	for _, raw := range cidrs {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if _, ipNet, err := net.ParseCIDR(raw); err == nil {
			tp.nets = append(tp.nets, ipNet)
		}
	}
	return tp
}

func (t *TrustedProxies) trusts(ip net.IP) bool {
	if t == nil || ip == nil {
		return false
	}
	for _, n := range t.nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// ClientIP returns r's real client IP. Every X-Forwarded-For line counts,
// joined in order as repeated fields combine (RFC 9110 §5.3): a proxy that
// appends its own line (HAProxy's option forwardfor) must not leave a
// client-written first line in charge. Nil-safe: a nil *TrustedProxies
// always returns the socket peer, matching "empty = trust nothing".
func (t *TrustedProxies) ClientIP(r *http.Request) string {
	return t.resolve(r.RemoteAddr, strings.Join(r.Header.Values("X-Forwarded-For"), ","))
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
