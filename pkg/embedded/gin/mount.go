package gin

import (
	"net/http"
	"strings"

	"github.com/open-rails/openrails/internal/http/embedhttp"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/embedded"
)

// MountOptions configures the combined embedded surface (MountHandler).
type MountOptions struct {
	// RouteSets selects the mounted billing surfaces. A zero slice uses the
	// embedded defaults.
	RouteSets []embedded.RouteSet
	// Authenticator protects checkout/user routes.
	Authenticator billingauth.Authenticator
	// Gate protects merchant routes.
	Gate billingauth.Gate
	// DelegatedAuthenticator protects /v1/me and /v1/customers routes.
	DelegatedAuthenticator billingauth.DelegatedAuthenticator
	// MountPrefix is the host path the whole surface is mounted under (e.g.
	// "/api/openrails"); incoming paths are MountPrefix + "/v1/...". Empty means
	// paths already arrive canonical ("/v1/...").
	MountPrefix string
}

// MountHandler returns the selected embedded billing surfaces as ONE handler.
// The host mounts this once and rewrites nothing.
func MountHandler(e *embedded.Embedded, opts MountOptions) (http.Handler, error) {
	// Resolve + record the full selection (incl. customer); the combined mount
	// serves customer via the self handler, so it is part of the advertised set.
	active := e.MountRouteSets(opts.RouteSets)
	var self http.Handler
	if routeSetSelected(active, embedded.RouteSetCustomer) {
		var err error
		self, err = SelfHandler(e, opts.DelegatedAuthenticator)
		if err != nil {
			return nil, err
		}
	}
	asm := embedhttp.FromApp(e.App())
	asm.Authenticator = opts.Authenticator
	asm.Gate = opts.Gate
	if a := e.App(); a != nil && a.Runtime != nil {
		asm.ConfiguredMerchant = a.Runtime.ConfiguredMerchant
	}
	userRouteSets := routeSetsWithoutCustomer(active)
	user := http.NotFoundHandler()
	if len(userRouteSets) > 0 {
		user = asm.NewHTTPHandler(embedhttp.Options{
			RouteSets:          userRouteSets,
			AdvertiseRouteSets: active,
		})
	}

	// base is the canonical embedded mount ("/billing"); both handlers serve at
	// base + "/v1/...".
	base := strings.TrimSuffix(embedhttp.EmbeddedV1Prefix, "/v1")
	return combinedMount(opts.MountPrefix, base, self, user), nil
}

func routeSetSelected(routeSets []embedded.RouteSet, want embedded.RouteSet) bool {
	if len(routeSets) == 0 {
		routeSets = embedded.EmbeddedDefaultRouteSets
	}
	for _, routeSet := range routeSets {
		if routeSet == want {
			return true
		}
	}
	return false
}

func routeSetsWithoutCustomer(routeSets []embedded.RouteSet) []embedded.RouteSet {
	if len(routeSets) == 0 {
		routeSets = embedded.EmbeddedDefaultRouteSets
	}
	out := make([]embedded.RouteSet, 0, len(routeSets))
	for _, routeSet := range routeSets {
		if routeSet != embedded.RouteSetCustomer {
			out = append(out, routeSet)
		}
	}
	return out
}

// combinedMount strips mountPrefix, rewrites to the canonical base, and routes
// selected customer subtrees to the self handler; everything else to the user
// handler.
func combinedMount(mountPrefix, base string, self, user http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, mountPrefix)
		if rest == "" {
			rest = "/"
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path = base + rest
		if r2.URL.RawPath != "" {
			r2.URL.RawPath = base + strings.TrimPrefix(r.URL.RawPath, mountPrefix)
		}
		if self != nil && (rest == "/v1/me" || rest == "/v1/customers" ||
			strings.HasPrefix(rest, "/v1/me/") || strings.HasPrefix(rest, "/v1/customers/")) {
			self.ServeHTTP(w, r2)
			return
		}
		user.ServeHTTP(w, r2)
	})
}
