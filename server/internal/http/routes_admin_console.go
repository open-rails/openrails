package server

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/server/internal/adminconsole"
	"github.com/open-rails/openrails/server/internal/controlplane"
	"github.com/open-rails/openrails/server/internal/hostconfig"
)

// The standalone product always serves its console at /admin. Its JSON
// directory is authenticated separately; static assets and sign-in choices
// contain no credentials or private directory data.
func (s *Server) registerAdminConsoleRoutes(mux *router.Table) error {
	path := hostconfig.ConsolePath
	for _, entry := range mux.Entries {
		if entry.Path == path || strings.HasPrefix(entry.Path, path+"/") {
			return fmt.Errorf("console path %q overlaps %s %s", path, entry.Method, entry.Path)
		}
	}
	cfg := adminconsole.Config{
		AuthBaseURL: s.controlPlane.AuthAPIBase(), APIBaseURL: StandaloneV1Prefix,
		LocalSignIn: s.controlPlane.LocalSignIn(), Issuers: []adminconsole.Issuer{},
	}
	if s.adminConsole != nil {
		keys := make([]string, 0, len(s.adminConsole.Issuers))
		for key := range s.adminConsole.Issuers {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		for _, key := range keys {
			declaration := s.adminConsole.Issuers[key]
			issuer, err := s.consoleIssuer(context.Background(), key, declaration)
			if err != nil {
				return err
			}
			cfg.Issuers = append(cfg.Issuers, issuer)
		}
	}
	if cfg.LocalSignIn && s.resource != "" {
		cfg.Exchange = &adminconsole.Exchange{TokenEndpoint: s.controlPlane.TokenEndpoint(), ClientID: controlplane.ConsoleClientID, Resource: s.resource, Scope: billing.ScopeMerchant}
	}
	console, err := adminconsole.Handler(path, cfg, s.consoleAssets)
	if err != nil {
		return err
	}
	s.handle(mux, http.MethodGet+" "+path+"/merchants", http.HandlerFunc(s.consoleMerchants))
	s.handle(mux, http.MethodGet+" "+path, console)
	s.handle(mux, http.MethodGet+" "+path+"/{asset...}", console)
	return nil
}

func (s *Server) consoleIssuer(ctx context.Context, key string, declaration hostconfig.ConsoleIssuer) (adminconsole.Issuer, error) {
	url, name, err := hostconfig.ResolveConsoleIssuer(&declaration, s.resource)
	if err != nil {
		return adminconsole.Issuer{}, err
	}
	if key == "" || key == "local" {
		return adminconsole.Issuer{}, fmt.Errorf("admin_console.issuers key %q must name a merchant or root", key)
	}
	scope := strings.Join(strings.Fields(declaration.Scope), " ")
	if scope == "" {
		scope = hostconfig.ConsoleScope
	}
	out := adminconsole.Issuer{Key: key, URL: url, ClientID: strings.TrimSpace(declaration.ClientID), Name: name, Resource: s.resource, Scope: scope}
	if key != "root" {
		mid, err := billing.ParseMerchantID(key)
		if err != nil {
			mid, _, err = s.controlPlane.MerchantByName(ctx, key)
		}
		if err != nil {
			return out, fmt.Errorf("admin_console.issuers[%q]: %w", key, err)
		}
		merchant, err := s.merchants.Get(ctx, mid)
		if err != nil {
			return out, fmt.Errorf("admin_console.issuers[%q]: %w", key, err)
		}
		out.Merchant = &adminconsole.Merchant{ID: mid.String(), Slug: merchant.Slug, DisplayName: s.merchants.DisplayName(ctx, mid)}
	}
	return out, nil
}
