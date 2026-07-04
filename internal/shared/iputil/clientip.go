package iputil

import (
	"net"
	"strings"
)

// TrustedProxies is the ONE proxy-aware client-IP resolver (#746). Every
// consumer that needs "the request's real client IP" — rate-limit subject
// keys, abuse tracking, webhook IPAddress recording, the CCBill webhook IP
// allowlist — resolves through an instance of this built from the SAME
// config.Config.TrustedProxies list, so proxy trust is enforced identically
// everywhere instead of each call site trusting (or not trusting)
// X-Forwarded-For on its own.
//
// Semantics: a nil/empty TrustedProxies trusts NOTHING — every resolution
// returns the raw socket peer, so a spoofed X-Forwarded-For has zero effect.
// When the immediate socket peer falls inside a trusted CIDR,
// ResolveClientIP walks X-Forwarded-For right-to-left, skipping trusted
// hops, to the first untrusted address — the standard reverse-proxy
// algorithm: everything to the right of that address was appended by a
// proxy we trust, so it is the closest honest claim of "who is the client".
type TrustedProxies struct {
	nets []*net.IPNet
}

// ParseTrustedProxies builds a resolver from configured CIDR strings.
// Malformed entries are silently skipped here — config.Validate is
// responsible for rejecting them at boot (a typo must never boot into a
// partially-trusting state); this constructor stays permissive so a resolver
// built from an already-validated config is always usable.
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

// ResolveClientIP returns the real client IP for one request. remoteAddr is
// the transport-level peer (http.Request.RemoteAddr — "host:port" or a bare
// host); forwardedFor is the raw X-Forwarded-For header value (possibly
// empty, possibly several comma-separated hops). Nil-safe: a nil
// *TrustedProxies always returns the socket peer, matching "empty = trust
// nothing".
func (t *TrustedProxies) ResolveClientIP(remoteAddr, forwardedFor string) string {
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
	// Every hop (including the direct peer) is a trusted proxy — no
	// untrusted address was ever observed on the wire; the direct peer is
	// the best available answer.
	return peer
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
