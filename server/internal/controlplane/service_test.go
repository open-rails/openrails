package controlplane

import (
	"slices"
	"strings"
	"testing"

	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/iam"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth"
)

// AuthKit's configuration is derived from the engine's: a hook adjusts
// policy, and the invariants still win.
func TestAuthKitIsDerivedFromTheEngine(t *testing.T) {
	engine := billing.DefaultConfig()
	engine.PublicBillingBaseURL = "https://billing.example.com/"
	engine.Database.Schema = "shop"
	engine.TrustedProxies = []string{"10.0.0.0/8"}
	a, err := AuthKit(&engine, Options{KeysPath: "/etc/keys", Console: true, Configure: func(c *authkit.Config) {
		c.Token.IssuedAudiences = []string{"elsewhere"}
		c.Resource.Scopes = map[string][]string{billing.ScopeSelf: {"merchant:*"}, "host:own": {}}
		c.Username.MinLength = 6
	}})
	if err != nil {
		t.Fatal(err)
	}
	base := "https://billing.example.com"
	for got, want := range map[string]string{
		a.Token.Issuer: base, a.Resource.ID: base, a.Resource.PublicURL: base, a.Frontend.BaseURL: base,
		a.Keys.Path: "/etc/keys", a.Database.Schema: "shop_auth", a.Frontend.InvitePath: "/admin/invite",
	} {
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
	if a.Token.AllowPrivateNetworkJWKS {
		t.Error("a public issuer reads no keys on a private network unless the operator declares one")
	}
	if a.Registration.NativeUserMode != iam.RegistrationModeInviteOnly || a.TwoFactor.Mode != iam.TwoFactorRequired || a.Token.RefreshTokenDuration != StaffSessionLifetime {
		t.Errorf("staff join by invitation, need a second factor and stay 30 days: %+v %+v", a.Registration, a.TwoFactor)
	}
	if a.Username.MinLength != 6 {
		t.Error("the hook's policy stays")
	}
	if !slices.Equal(a.Token.IssuedAudiences, []string{billingauth.TokenAudience}) || len(a.Resource.Scopes[billing.ScopeSelf]) != 0 || a.Resource.Scopes["host:own"] == nil {
		t.Errorf("invariants win over the hook: %v %v", a.Token.IssuedAudiences, a.Resource.Scopes)
	}
	if !slices.Equal(a.HTTP.TrustedProxies, engine.TrustedProxies) || a.HTTP.DirectPeerIP {
		t.Errorf("AuthKit's client IP follows trusted_proxies: %+v", a.HTTP)
	}
	if !slices.ContainsFunc(a.AuthorizationServer.Clients, func(c authkit.OAuthClientConfig) bool { return c.ID == ConsoleClientID }) {
		t.Error("the console's client is declared")
	}

	engine.PublicBillingBaseURL = "http://localhost:3053"
	if a, err := AuthKit(&engine, Options{}); err != nil || !a.Token.AllowPrivateNetworkJWKS {
		t.Errorf("a loopback issuer is local: %v", err)
	}
	for name, c := range map[string]struct {
		base string
		hook func(*authkit.Config)
	}{
		"http on a public host": {"http://billing.example.com", nil},
		"open registration":     {base, func(c *authkit.Config) { c.Registration.NativeUserMode = iam.RegistrationModeOpen }},
	} {
		engine.PublicBillingBaseURL = c.base
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
