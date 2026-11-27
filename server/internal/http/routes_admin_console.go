package server

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/server/internal/adminconsole"
)

// The standalone product serves its enabled console at /admin. Its JSON
// directory and New merchant authenticate separately; static assets and
// config.json contain no credentials or private directory data.
func (s *Server) registerAdminConsoleRoutes(mux *router.Table) error {
	if s.consoleOff {
		return nil
	}
	path := adminconsole.Path
	for _, entry := range mux.Entries {
		if entry.Path == path || strings.HasPrefix(entry.Path, path+"/") {
			return fmt.Errorf("console path %q overlaps %s %s", path, entry.Method, entry.Path)
		}
	}
	cfg := adminconsole.Config{
		AuthBaseURL: s.controlPlane.AuthAPIBase(), APIBaseURL: StandaloneV1Prefix,
		Exchange: &adminconsole.Exchange{TokenEndpoint: s.controlPlane.TokenEndpoint(), ClientID: s.controlPlane.ConsoleClientID(), Resource: s.resource, Scope: billing.ScopeMerchant},
	}
	console, err := adminconsole.Handler(s.Log, path, cfg, s.consoleAssets)
	if err != nil {
		return err
	}
	// Keep the product subtree one native exposure for host router adapters.
	// The directory authenticates here before the SPA's fallback can see it.
	consoleTree := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == path+"/merchants" {
			s.consoleMerchants(w, r)
			return
		}
		console.ServeHTTP(w, r)
	})
	s.handle(mux, http.MethodGet+" "+path, consoleTree)
	s.handle(mux, http.MethodGet+" "+path+"/{asset...}", consoleTree)
	s.handle(mux, http.MethodPost+" "+path+"/merchants", http.HandlerFunc(s.consoleCreateMerchant))
	return nil
}
