package httpx

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// #SEC-21: any URL a merchant (or any request) supplies and we then FETCH is an
// SSRF primitive — it makes OpenRails issue HTTP from its own network position.
// Policy enforces one rule set in one place:
//
//   - the check runs at the DIALER, on the RESOLVED address, so a hostname that
//     resolves internally — including a DNS-rebinding record that flips between
//     retries — is blocked at connect time, not merely at parse time;
//   - every redirect hop is re-validated, so a public host cannot 302 into
//     link-local;
//   - callers must not surface the raw dial error, which is an internal-network
//     oracle. Use FailureDetail.

// ErrBlockedAddress is returned when a connection targets a non-public address.
var ErrBlockedAddress = errors.New("httpx: destination address is not publicly routable")

// ErrUnsupportedScheme is returned for a non-http(s) URL.
var ErrUnsupportedScheme = errors.New("httpx: url must be http or https")

// blockedCIDRs covers ranges net.IP's own predicates miss: CGNAT (100.64/10),
// IETF protocol assignments (192.0.0.0/24, which holds the 192.0.0.170 NAT64
// address), the benchmarking range, TEST-NETs, and the NAT64 well-known prefix.
var blockedCIDRs = func() []*net.IPNet {
	raw := []string{
		"0.0.0.0/8",
		"100.64.0.0/10",
		"192.0.0.0/24",
		"198.18.0.0/15",
		"192.0.2.0/24", "198.51.100.0/24", "203.0.113.0/24",
		"64:ff9b::/96",
		"2001:db8::/32",
	}
	out := make([]*net.IPNet, 0, len(raw))
	for _, c := range raw {
		if _, n, err := net.ParseCIDR(c); err == nil {
			out = append(out, n)
		}
	}
	return out
}()

// IsBlockedIP reports whether ip is anything other than a publicly routable
// unicast address: loopback, RFC1918 private, link-local (169.254/16, fe80::/10),
// unique-local (fc00::/7), CGNAT (100.64/10), multicast, unspecified, broadcast,
// or a reserved range.
func IsBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4 // normalize IPv4-mapped IPv6 (::ffff:127.0.0.1) before checking
	}
	if ip.IsUnspecified() || ip.IsLoopback() || ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.Equal(net.IPv4bcast) {
		return true
	}
	for _, n := range blockedCIDRs {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// AllowFunc re-admits a resolved address that the public-routability check
// would reject. Production wiring leaves it nil; tests set it so they can point
// at their own loopback server without disabling the policy for anything else.
type AllowFunc func(net.IP) bool

// AllowLoopback is the test escape hatch: loopback only, nothing else.
func AllowLoopback(ip net.IP) bool { return ip != nil && ip.IsLoopback() }

// Policy is the outbound-fetch guard. The zero value is the strict production
// policy (public unicast destinations only).
type Policy struct{ Allow AllowFunc }

func (p Policy) allowed(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if p.Allow != nil && p.Allow(ip) {
		return true
	}
	return !IsBlockedIP(ip)
}

// ValidateURL checks a host-supplied URL before it is stored or fetched:
// http(s) only, a host present, and — when the host is a literal IP or
// localhost — an allowed address. A HOSTNAME is deliberately not resolved
// here: resolution at validation time is advisory (it can change before the
// request), which is why Client re-checks at the dialer.
func (p Policy) ValidateURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("httpx: invalid url: %w", err)
	}
	return p.validateParsed(u)
}

func (p Policy) validateParsed(u *url.URL) error {
	if u.Scheme != "http" && u.Scheme != "https" {
		return ErrUnsupportedScheme
	}
	host := u.Hostname()
	if host == "" {
		return errors.New("httpx: url has no host")
	}
	if ip := net.ParseIP(host); ip != nil {
		if !p.allowed(ip) {
			return ErrBlockedAddress
		}
		return nil
	}
	// "localhost" is resolver-dependent; decide it here so the rejection is
	// intelligible rather than an opaque dial failure.
	if h := strings.ToLower(host); h == "localhost" || strings.HasSuffix(h, ".localhost") {
		if !p.allowed(net.IPv4(127, 0, 0, 1)) {
			return ErrBlockedAddress
		}
	}
	return nil
}

// maxRedirects bounds redirect chains; each hop is re-validated.
const maxRedirects = 5

// Client returns an http.Client that can only reach addresses this policy
// allows. Enforcement is at the dialer (post-DNS, per connection) so DNS
// rebinding between retries cannot slip past, and CheckRedirect re-validates
// every hop. Environment proxies are deliberately ignored: the socket peer
// would be the proxy, whose DNS/network view this dialer cannot validate.
func (p Policy) Client(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	dialer := &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}
	dialer.Control = func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			host = address
		}
		if !p.allowed(net.ParseIP(host)) {
			return ErrBlockedAddress
		}
		return nil
	}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext:           dialer.DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConnsPerHost:   4,
			IdleConnTimeout:       30 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: time.Second,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("httpx: stopped after %d redirects", maxRedirects)
			}
			return p.validateParsed(req.URL)
		},
	}
}

// FailureDetail maps an outbound-request error to text safe to hand back to the
// tenant who configured the destination. A raw dial error is an internal-network
// oracle (open vs closed port, resolvable vs not), so it collapses to a fixed
// message; the caller logs the real error separately.
func FailureDetail(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrBlockedAddress):
		return "destination address is not publicly routable"
	case errors.Is(err, ErrUnsupportedScheme):
		return "url must be http or https"
	default:
		return "delivery failed: the destination could not be reached"
	}
}
