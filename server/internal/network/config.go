package network

import (
	"errors"
	"slices"

	"github.com/open-rails/authkit"

	"github.com/open-rails/openrails/billing"
)

// groupScopes are what a merchant's client may ask an account for, with the
// consent screen's words.
var groupScopes = []authkit.GroupClientScope{
	{Name: billing.ScopeSelf, Description: "Use your openrails.dev account as your account at this merchant: buy, and see and manage your billing there."},
	{Name: ScopeLink, Description: "Link your existing account at this merchant to your openrails.dev account."},
}

// Configure sets the network's product constants on the server's AuthKit:
// the server's resource takes the network scopes, merchants' clients may ask
// for them, and an account accepts the network terms before approving one.
// The network needs the server's resource (auth.resource.id).
func Configure(cfg *authkit.Config) error {
	resource := cfg.Resource.ID
	if resource == "" {
		return errors.New("network: the customer network needs the server's resource: auth.resource.id")
	}
	as := &cfg.AuthorizationServer
	i := slices.IndexFunc(as.Resources, func(r authkit.ResourceServerConfig) bool { return r.ID == resource })
	if i < 0 {
		as.Resources = append(as.Resources, authkit.ResourceServerConfig{ID: resource})
		i = len(as.Resources) - 1
	}
	for _, s := range groupScopes {
		if !slices.Contains(as.Resources[i].Scopes, s.Name) {
			as.Resources[i].Scopes = append(as.Resources[i].Scopes, s.Name)
		}
		if !slices.ContainsFunc(as.GroupClients.Scopes, func(g authkit.GroupClientScope) bool { return g.Name == s.Name }) {
			s.Resource = resource
			as.GroupClients.Scopes = append(as.GroupClients.Scopes, s)
		}
	}
	if !slices.Contains(as.GroupClients.Agreements, TermsAgreement) {
		as.GroupClients.Agreements = append(as.GroupClients.Agreements, TermsAgreement)
	}
	return nil
}
