package hostedcheckout

import (
	"fmt"
	"golang.org/x/net/idna"
	"net"
	"net/url"
	"strings"
)

// Origin validates an isolated payment origin. Cookie-bearing application,
// billing API and identity-provider origins must be passed in forbidden.
func Origin(raw string, forbidden ...string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.User != nil || u.Host == "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/") ||
		!(u.Scheme == "https" || u.Scheme == "http" && loopback(u.Hostname())) {
		return "", fmt.Errorf("hosted checkout URL must be an https origin with no path (http on loopback)")
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
		if err == nil && otherOrigin == origin {
			return "", fmt.Errorf("hosted checkout must use its own origin, separate from billing and sign-in")
		}
	}
	return origin, nil
}

// Browsers treat explicit default ports and IDNA host spellings as the same
// origin. Compare that identity before allowing a separate payment surface.
func canonicalOrigin(u *url.URL) (string, error) {
	scheme, host, port := strings.ToLower(u.Scheme), strings.ToLower(u.Hostname()), u.Port()
	if net.ParseIP(host) == nil {
		var err error
		host, err = idna.Lookup.ToASCII(host)
		if err != nil {
			return "", fmt.Errorf("invalid hosted checkout hostname: %w", err)
		}
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
