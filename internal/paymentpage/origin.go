package paymentpage

import (
	"fmt"
	"golang.org/x/net/idna"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

// Origin validates an isolated payment origin. Cookie-bearing application,
// billing API and identity-provider origins must be passed in forbidden.
func Origin(raw string, forbidden ...string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.User != nil || u.Host == "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/") ||
		!(u.Scheme == "https" || u.Scheme == "http" && loopback(u.Hostname())) {
		return "", fmt.Errorf("the payment origin must be an https origin with no path (http on loopback)")
	}
	origin, err := canonicalOrigin(u)
	if err != nil {
		return "", err
	}
	for _, other := range forbidden {
		parsed, err := url.Parse(strings.TrimSpace(other))
		if err != nil || parsed.Host == "" {
			continue
		}
		otherOrigin, err := canonicalOrigin(parsed)
		if err != nil {
			return "", fmt.Errorf("invalid application origin: %w", err)
		}
		if otherOrigin == origin {
			return "", fmt.Errorf("the payment origin must be its own, separate from billing and sign-in")
		}
	}
	return origin, nil
}

// Browsers treat explicit default ports and IDNA host spellings as the same
// origin. Compare that identity before allowing a separate payment surface.
func canonicalOrigin(u *url.URL) (string, error) {
	scheme, host, port := strings.ToLower(u.Scheme), strings.ToLower(u.Hostname()), u.Port()
	if ip, err := netip.ParseAddr(host); err == nil {
		host = ip.String()
	} else {
		var err error
		host, err = idna.Lookup.ToASCII(host)
		if err != nil {
			return "", fmt.Errorf("invalid payment origin hostname: %w", err)
		}
		// Browsers accept shortened, octal and hexadecimal IPv4 addresses.
		// Reject these ambiguous spellings instead of treating them as DNS names.
		last := strings.TrimSuffix(host, ".")
		if i := strings.LastIndexByte(last, '.'); i >= 0 {
			last = last[i+1:]
		}
		numeric := last != "" && strings.Trim(last, "0123456789") == ""
		hex := strings.HasPrefix(last, "0x") && strings.Trim(strings.TrimPrefix(last, "0x"), "0123456789abcdef") == ""
		if numeric || hex {
			return "", fmt.Errorf("the payment origin requires a canonical IP address")
		}
	}
	if port != "" {
		n, err := strconv.ParseUint(port, 10, 16)
		if err != nil {
			return "", fmt.Errorf("invalid payment origin port")
		}
		port = strconv.FormatUint(n, 10)
	}
	if scheme == "https" && port == "443" || scheme == "http" && port == "80" {
		port = ""
	}
	if port != "" {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return scheme + "://" + host, nil
}

// Host recognizes the payment origin in a request's Host header. Browsers'
// spellings of one origin match (case, a trailing dot, the scheme's default
// port); forwarded headers play no part.
type Host struct{ scheme, host string }

// NewHost is the Host of a payment origin (Origin's rules).
func NewHost(origin string) (Host, error) {
	canonical, err := Origin(origin)
	if err != nil {
		return Host{}, err
	}
	scheme, host, _ := strings.Cut(canonical, "://")
	return Host{scheme: scheme, host: host}, nil
}

// Match reports whether raw, a request's Host, names the payment origin;
// valid is false for anything but a plain host[:port].
func (h Host) Match(raw string) (match, valid bool) {
	u, err := url.Parse("//" + raw)
	if err != nil || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.HasSuffix(raw, ":") {
		return false, false
	}
	host, port := strings.ToLower(strings.TrimSuffix(u.Hostname(), ".")), u.Port()
	if h.scheme == "https" && port == "443" || h.scheme == "http" && port == "80" {
		port = ""
	}
	if port != "" {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return host == h.host, true
}
