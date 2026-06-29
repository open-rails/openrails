package gin

import (
	"net/http"
	"strings"

	"github.com/open-rails/openrails/internal/http/embedhttp"
	"github.com/open-rails/openrails/pkg/embedded"
)

// MountOptions configures the combined embedded surface (MountHandler).
type MountOptions struct {
	// RouteSets selects the gin-free user surface (checkout, merchant-admin,
	// webhooks). The self surface (/me, /customers) is always included.
	RouteSets []embedded.RouteSet
	// CredentialMode controls whether merchant settings/provider credential
	// mutation routes may be mounted. Empty defaults to fixed credentials.
	CredentialMode embedded.CredentialMode
	// MountPrefix is the host path the whole surface is mounted under (e.g.
	// "/api/openrails"); incoming paths are MountPrefix + "/v1/...". Empty means
	// paths already arrive canonical ("/v1/...").
	MountPrefix string
}

// MountHandler returns the FULL embedded billing surface as ONE handler: the gin
// self surface (/me + /customers) and the gin-free user surface (opts.RouteSets)
// dispatched internally and served under opts.MountPrefix. The host mounts this
// once and rewrites nothing — the routing assembly that previously had to live in
// each embedder's glue now lives here.
func MountHandler(e *embedded.Embedded, opts MountOptions) (http.Handler, error) {
	self, err := SelfHandler(e)
	if err != nil {
		return nil, err
	}
	asm := embedhttp.FromApp(e.App())
	if a := e.App(); a != nil && a.Runtime != nil {
		asm.ConfiguredMerchant = a.Runtime.ConfiguredMerchant
	}
	user := asm.NewHTTPHandler(embedhttp.Options{RouteSets: opts.RouteSets, CredentialMode: embedhttp.CredentialMode(opts.CredentialMode)})

	// base is the canonical embedded mount ("/billing"); both handlers serve at
	// base + "/v1/...".
	base := strings.TrimSuffix(embedhttp.EmbeddedV1Prefix, "/v1")
	return combinedMount(opts.MountPrefix, base, self, user), nil
}

// combinedMount strips mountPrefix, rewrites to the canonical base, and routes
// the /v1/me + /v1/customers subtrees to the self handler; everything else
// (catalog/checkout, merchant-admin, webhooks) to the user handler.
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
		if rest == "/v1/me" || rest == "/v1/customers" ||
			strings.HasPrefix(rest, "/v1/me/") || strings.HasPrefix(rest, "/v1/customers/") {
			self.ServeHTTP(w, r2)
			return
		}
		user.ServeHTTP(w, r2)
	})
}
