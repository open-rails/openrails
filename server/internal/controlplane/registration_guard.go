package controlplane

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/open-rails/authkit/iam"

	"github.com/open-rails/openrails/internal/shared/httpx"
)

// RegistrationGuard is AuthKit's Deps.Wrap for the routes that register a
// trusted application or an OAuth client at run time: every URL the server
// would fetch or post to (a jwks_uri, a backchannel_logout_uri, or the issuer
// whose metadata names the keys) must be https on a public host. Only an
// operator declares one on a private network: the merchant manifest, the
// bootstrap manifest, or the system identity in code (SSRF).
func RegistrationGuard(route iam.Route, next http.Handler) http.Handler {
	g, ok := guarded(route)
	if !ok {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			iam.WriteError(w, fmt.Errorf("%w: %v", g.refusal, err))
			return
		}
		var in map[string]json.RawMessage
		if json.Unmarshal(body, &in) == nil {
			if err := g.check(in); err != nil {
				iam.WriteError(w, fmt.Errorf("%w: %v", g.refusal, err))
				return
			}
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		next.ServeHTTP(w, r)
	})
}

// guard is what one registration route's body may name.
type guard struct {
	refusal iam.Error
	// fields are the URLs the server fetches or posts to.
	fields []string
	// issuer: a new application with neither a jwks_uri nor static keys reads
	// its keys from its issuer's metadata, so the issuer is fetched too.
	issuer bool
}

// guarded is the guard of a run-time registration route.
func guarded(route iam.Route) (guard, bool) {
	path := strings.TrimSuffix(route.Path, "/")
	switch {
	case route.Method == http.MethodPost && strings.HasSuffix(path, "/remote-applications"):
		return guard{iam.ErrInvalidRemoteApplication, []string{"jwks_uri"}, true}, true
	case route.Method == http.MethodPatch && strings.HasSuffix(path, "/remote-applications/{id}"):
		return guard{iam.ErrInvalidRemoteApplication, []string{"jwks_uri"}, false}, true
	case route.Method == http.MethodPost && strings.HasSuffix(path, "/oauth-clients"),
		route.Method == http.MethodPatch && strings.HasSuffix(path, "/oauth-clients/{client_id}"):
		return guard{iam.ErrInvalidOAuthClient, []string{"jwks_uri", "backchannel_logout_uri"}, false}, true
	}
	return guard{}, false
}

func (g guard) check(in map[string]json.RawMessage) error {
	for _, field := range g.fields {
		var raw *string
		if json.Unmarshal(in[field], &raw) == nil && raw != nil && strings.TrimSpace(*raw) != "" {
			if err := PublicHTTPS(*raw); err != nil {
				return fmt.Errorf("%s: %w", field, err)
			}
		}
	}
	if g.issuer && emptyJSON(in["jwks_uri"]) && emptyJSON(in["public_keys"]) {
		var issuer string
		if err := json.Unmarshal(in["issuer"], &issuer); err == nil {
			if err := PublicHTTPS(issuer); err != nil {
				return fmt.Errorf("issuer, whose metadata names its keys: %w", err)
			}
		}
	}
	return nil
}

func emptyJSON(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s == "" || s == "null" || s == `""` || s == "[]"
}

// PublicHTTPS refuses a URL that is not https on a public host: a loopback,
// private or reserved address, or a name only a private network resolves.
func PublicHTTPS(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return fmt.Errorf("%q is not an https URL", raw)
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if ip := net.ParseIP(host); ip != nil {
		if httpx.IsBlockedIP(ip) {
			return fmt.Errorf("%q names a private or reserved address; only the operator declares those", raw)
		}
		return nil
	}
	if !strings.Contains(host, ".") || host == "localhost" {
		return fmt.Errorf("%q names a host only a private network resolves; only the operator declares those", raw)
	}
	for _, suffix := range []string{".localhost", ".local", ".internal", ".localdomain", ".lan", ".home.arpa", ".svc", ".cluster.local"} {
		if strings.HasSuffix(host, suffix) {
			return fmt.Errorf("%q names a host only a private network resolves; only the operator declares those", raw)
		}
	}
	return nil
}
