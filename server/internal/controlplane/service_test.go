package controlplane

import (
	"slices"
	"strings"
	"testing"

	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/iam"

	"github.com/open-rails/openrails/billing"
)

// AuthKit's configuration is derived from the engine's: a hook adjusts
// policy, and the invariants still win.
func TestAuthKitIsDerivedFromTheEngine(t *testing.T) {
	engine := billing.DefaultConfig()
	engine.PublicURL = "https://billing.example.com/"
	engine.Database.Schema = "shop"
	engine.TrustedProxies = []string{"10.0.0.0/8"}
	a, err := AuthKit(&engine, Options{Console: true, Configure: func(c *authkit.Config) {
		c.APIKeyPrefix = "elsewhere"
		c.Resource.Scopes = map[string][]string{billing.ScopeSelf: {"merchant:*"}, "host:own": {}}
		c.Usernames = []string{"shopper"}
	}})
	if err != nil {
		t.Fatal(err)
	}
	base := "https://billing.example.com"
	for got, want := range map[string]string{
		a.Issuer: base, a.Resource.ID: base, a.FrontendURL: base + "/admin", a.Database.Schema: "shop_auth",
	} {
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
	if a.Registration != iam.RegistrationModeInviteOnly || a.TwoFactor != iam.TwoFactorRequired {
		t.Errorf("staff join by invitation and need a second factor: %q %q", a.Registration, a.TwoFactor)
	}
	if !slices.Equal(a.Usernames, []string{"shopper"}) {
		t.Error("the hook's policy stays")
	}
	if a.APIKeyPrefix != APIKeyPrefix || len(a.Resource.Scopes[billing.ScopeSelf]) != 0 || a.Resource.Scopes["host:own"] == nil {
		t.Errorf("invariants win over the hook: %q %v", a.APIKeyPrefix, a.Resource.Scopes)
	}
	if !slices.Equal(a.HTTP.TrustedProxies, engine.TrustedProxies) || a.HTTP.DirectPeerIP {
		t.Errorf("AuthKit's client IP follows trusted_proxies: %+v", a.HTTP)
	}
	if !slices.Contains(a.HTTP.Groups, iam.RouteInvitations) || !slices.Contains(a.HTTP.Groups, iam.RouteSetup) {
		t.Errorf("staff join by invitation, the first owner by the console's setup link: %v", a.HTTP.Groups)
	}
	if !slices.ContainsFunc(a.AuthorizationServer.Clients, func(c authkit.OAuthClientConfig) bool { return c.ID == ConsoleClientID }) {
		t.Error("the console's client is declared")
	}

	if a, err := AuthKit(&engine, Options{}); err != nil || a.FrontendURL != "" || slices.Contains(a.HTTP.Groups, iam.RouteSetup) {
		t.Errorf("without the console AuthKit's links land at the issuer's origin, and no setup link has a page: %q %v %v", a.FrontendURL, a.HTTP.Groups, err)
	}
	for name, c := range map[string]struct {
		base string
		hook func(*authkit.Config)
	}{
		"http on a public host": {"http://billing.example.com", nil},
		"open registration":     {base, func(c *authkit.Config) { c.Registration = iam.RegistrationModeOpen }},
	} {
		engine.PublicURL = c.base
		if _, err := AuthKit(&engine, Options{Configure: c.hook}); err == nil {
			t.Errorf("%s: want refused", name)
		}
	}
}

// A run-time registration names only https URLs on public hosts.
func TestRunTimeKeyURLsArePublic(t *testing.T) {
	for _, raw := range []string{"https://app.example.com/jwks.json", "https://203.0.114.1/jwks"} {
		if err := PublicHTTPS(raw); err != nil {
			t.Errorf("%s: %v", raw, err)
		}
	}
	for _, raw := range []string{"http://app.example.com/jwks.json", "https://127.0.0.1/jwks", "https://10.1.2.3/jwks", "https://[::1]/jwks", "https://localhost/jwks", "https://keys/jwks", "https://keys.ns.svc.cluster.local/jwks", "https://metadata.internal/", "ftp://app.example.com"} {
		if err := PublicHTTPS(raw); err == nil || !strings.Contains(err.Error(), raw) {
			t.Errorf("%s: want refused, got %v", raw, err)
		}
	}
}
