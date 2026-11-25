package server

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/server/internal/adminconsole"
)

// The standalone product serves its enabled console at /admin. Its JSON
// directory is authenticated separately; static assets and the sign-in
// buttons contain no credentials or private directory data.
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
	var issuers *adminconsole.Directory
	if !s.consoleOwnOnly {
		// An issuer whose sign-in does not work is hidden and reported where
		// the configuration's other refusals are (readiness, metrics, logs).
		report := func(issuer string, err error) {
			if err != nil {
				s.runtime.Refuse(consoleIssuerSubject+issuer, err)
			} else {
				s.runtime.Unrefuse(consoleIssuerSubject + issuer)
			}
		}
		callback := publicOrigin(s.cfg.PublicURL) + path + adminconsole.CallbackPath
		issuers = adminconsole.NewDirectory(s.Log, s.controlPlane.RootConsoleSources, s.resource, callback, report)
	}
	console, err := adminconsole.Handler(s.Log, path, cfg, issuers, s.consoleAssets)
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
	return nil
}

// consoleIssuerSubject prefixes a trusted issuer the console's sign-in does
// not offer, among the configuration's refusals.
const consoleIssuerSubject = "console.issuers."

// publicOrigin is raw's scheme and host: where browsers reach the console.
func publicOrigin(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}
