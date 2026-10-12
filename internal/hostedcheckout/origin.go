package hostedcheckout

import (
	"fmt"
	"net/url"
	"strings"
)

// Origin validates an isolated payment origin. Cookie-bearing application,
// billing API and identity-provider origins must be passed in forbidden.
func Origin(raw string, forbidden ...string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.User != nil || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") ||
		!(u.Scheme == "https" || u.Scheme == "http" && loopback(u.Hostname())) {
		return "", fmt.Errorf("hosted checkout URL must be an https origin with no path (http on loopback)")
	}
	origin := strings.ToLower(u.Scheme + "://" + u.Host)
	for _, other := range forbidden {
		parsed, err := url.Parse(strings.TrimSpace(other))
		if err == nil && parsed.Host != "" && strings.ToLower(parsed.Scheme+"://"+parsed.Host) == origin {
			return "", fmt.Errorf("hosted checkout must use an origin separate from billing and sign-in")
		}
	}
	return origin, nil
}
