package embedhttp

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/http/router"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
)

// BuildCustomerRoutes mounts the customer surface, /v1/me, whose customers
// auth says who they are, each at the merchant its credential is bound to.
func BuildCustomerRoutes(a *app.App, auth billingauth.Authenticator) (*router.Table, error) {
	if httproutes.IsNilAuth(auth) {
		return nil, fmt.Errorf("openrails: the customer routes need Routes.Auth")
	}
	providers, err := ConfiguredProviderRoutes(context.Background(), a.Runtime)
	if err != nil {
		return nil, err
	}
	table := &router.Table{}
	httproutes.RegisterCustomerRoutes(router.NewMux(table, CustomerPrefix, a.Runtime), a.Runtime,
		httproutes.CustomerMount{Auth: auth, Providers: providers})
	return wrapCustomerRoutes(a.Runtime, table, auth), nil
}

// BuildMappedCustomerRoutes uses a host-authorized binding without entering
// the ordinary customer issuer/native-network enrollment path.
func BuildMappedCustomerRoutes(a *app.App, selection config.CustomerRoutes) (*router.Table, error) {
	if a == nil || a.Runtime == nil || httproutes.IsNilAuth(selection.Auth) || selection.Resolve == nil {
		return nil, fmt.Errorf("openrails: CustomerRoutes requires an initialized engine, Auth and Resolve")
	}
	providers, err := ConfiguredProviderRoutes(context.Background(), a.Runtime)
	if err != nil {
		return nil, err
	}
	table := &router.Table{}
	httproutes.RegisterMappedCustomerRoutes(router.NewMux(table, CustomerPrefix, a.Runtime), a.Runtime,
		httproutes.CustomerMount{Auth: selection.Auth, Providers: providers}, selection.Resolve)
	return wrapCustomerRoutes(a.Runtime, table, selection.Auth), nil
}

// CustomerPrefix is the customer surface's path beneath the mount.
const CustomerPrefix = "/v1/me"

// ValidateRouteTable rejects duplicate and ambiguous native registrations before
// an adapter mutates its host router. ServeMux validates wildcard syntax too.
func ValidateRouteTable(table *router.Table) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("openrails HTTP: conflicting configured routes: %v", recovered)
		}
	}()
	mux := http.NewServeMux()
	wildcards := make(map[string]string)
	subtrees := make(map[string]string)
	descendants := make(map[string]string)
	for _, entry := range table.Entries {
		// Gin shares one wildcard name at each method/path-tree position, even
		// when the routes diverge later. Validate this before any host mutation;
		// ServeMux alone permits names that Gin cannot register together.
		prefix := entry.Method
		for _, part := range strings.Split(entry.Path, "/") {
			// Native catch-all routes own every descendant for their method.
			// ServeMux instead allows more-specific routes below that subtree.
			if previous, ok := subtrees[prefix]; ok {
				return fmt.Errorf("openrails HTTP: conflicting native subtree routes %q and %q", previous, entry.Path)
			}
			if strings.HasPrefix(part, "{") && strings.HasSuffix(part, "...}") {
				if previous, ok := descendants[prefix]; ok {
					return fmt.Errorf("openrails HTTP: conflicting native subtree routes %q and %q", previous, entry.Path)
				}
				subtrees[prefix] = entry.Path
			} else {
				descendants[prefix] = entry.Path
			}
			if strings.HasPrefix(part, "{") && strings.HasSuffix(part, "}") {
				prefix += "/{}"
				if previous, ok := wildcards[prefix]; ok && previous != part {
					return fmt.Errorf("openrails HTTP: conflicting native wildcard names %q and %q at %s", previous, part, prefix)
				}
				wildcards[prefix] = part
			} else {
				prefix += "/" + part
			}
		}
		mux.Handle(entry.Method+" "+entry.Path, entry.Handler)
	}
	return nil
}
