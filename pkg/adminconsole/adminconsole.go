// Package adminconsole serves the embedded merchant admin console SPA (#740).
// Standalone mounts it at /admin/ behind the admin_console.enabled config
// gate; embedded hosts may mount Handler on their own mux at /admin/ with
// their host AuthKit issuer in Config.
package adminconsole

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"strings"

	"github.com/open-rails/openrails/pkg/api"
	webadmin "github.com/open-rails/openrails/web"
)

// Config is the SPA bootstrap document served at /admin/config.json.
type Config struct {
	// AuthBaseURL is the base under which the AuthKit authhttp routes live
	// (capabilities, password/login, token, me, OIDC). Standalone default:
	// "/auth" (the control plane mount). Embedded: the host AuthKit base,
	// possibly another origin.
	AuthBaseURL string `json:"auth_base_url"`
	// APIBaseURL is the merchant API base. Standalone default "/v1";
	// embedded hosts typically "/billing/v1".
	APIBaseURL string `json:"api_base_url"`
}

// BuiltAssets reports whether a real Vite build is embedded (vs the
// committed placeholder index).
func BuiltAssets() bool { return webadmin.HasBuiltAssets() }

// Handler serves the console: /admin/config.json from cfg, static files from
// the embedded dist, and index.html as SPA fallback for client-side routes.
// Mount at "GET /admin/" (redirect bare /admin separately). When the embedded
// dist is only the placeholder it fails loud: every request answers 503.
func Handler(cfg Config) http.Handler {
	if cfg.AuthBaseURL == "" {
		cfg.AuthBaseURL = "/auth"
	}
	if cfg.APIBaseURL == "" {
		cfg.APIBaseURL = "/v1"
	}
	configJSON, err := json.Marshal(cfg)
	if err != nil {
		panic(err) // static struct, cannot fail
	}
	dist := webadmin.DistFS()
	built := webadmin.HasBuiltAssets()

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rel := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/admin"), "/")

		if rel == "config.json" {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			_, _ = w.Write(configJSON)
			return
		}

		if !built {
			writeError(w, http.StatusServiceUnavailable,
				"admin console assets are not built into this binary; run `task admin-build` and rebuild")
			return
		}

		if rel != "" && fs.ValidPath(rel) {
			if info, err := fs.Stat(dist, rel); err == nil && !info.IsDir() {
				if strings.HasPrefix(rel, "assets/") {
					// Vite emits content-hashed filenames under assets/.
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				http.ServeFileFS(w, r, dist, rel)
				return
			}
		}

		// SPA fallback: client-side routes render from index.html.
		w.Header().Set("Cache-Control", "no-store")
		http.ServeFileFS(w, r, dist, "index.html")
	})
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(api.SimpleErrorResponse(status, msg))
}
