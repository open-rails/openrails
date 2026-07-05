// Package adminconsole serves the merchant admin console SPA (#740) from a
// caller-supplied fs.FS (#754: the engine ships no frontend bytes — whoever
// builds the binary owns the embed). Standalone builds opt in with
// `-tags console_assets`; embedded hosts pass embed.WithAdminConsole(assets).
// Build assets with scripts/build-admin-console.sh (in-repo: `task admin-build`).
package adminconsole

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"strings"

	"github.com/open-rails/openrails/pkg/api"
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
	// NLWidgetsEnabled mirrors the #741 fail-closed LLM gate: false hides the
	// natural-language widget box entirely (the generate endpoint would 501).
	// The manual widget builder is always available.
	NLWidgetsEnabled bool `json:"nl_widgets_enabled"`
	// AskEnabled mirrors the #756 metrics Q&A gate (llm.ask_enabled AND an LLM
	// key): false renders the Ask panel as a pointed empty-state (the ask
	// endpoint would 501). Distinct consent from NLWidgetsEnabled because /ask
	// sends aggregate query results to the LLM provider.
	AskEnabled bool `json:"ask_enabled"`
}

// Present reports whether assets hold a servable console build: a non-nil
// fs.FS with index.html at its root. The mount rule (#754) keys on this —
// enabled + !Present is a boot error, not a degraded mount.
func Present(assets fs.FS) bool {
	if assets == nil {
		return false
	}
	info, err := fs.Stat(assets, "index.html")
	return err == nil && !info.IsDir()
}

// Handler serves the console from assets (a Vite build rooted at index.html):
// /admin/config.json from cfg, static files, and index.html as SPA fallback
// for client-side routes. Mount at "GET /admin/" (redirect bare /admin
// separately). Callers should gate mounting on Present(assets); if mounted
// without a build anyway, every request answers 503 naming the build step.
func Handler(cfg Config, assets fs.FS) http.Handler {
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
	present := Present(assets)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rel := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/admin"), "/")

		if rel == "config.json" {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			_, _ = w.Write(configJSON)
			return
		}

		if !present {
			writeError(w, http.StatusServiceUnavailable,
				"admin console assets missing: build the SPA (scripts/build-admin-console.sh) and rebuild with -tags console_assets, or pass the assets FS to the engine")
			return
		}

		if rel != "" && fs.ValidPath(rel) {
			if info, err := fs.Stat(assets, rel); err == nil && !info.IsDir() {
				if strings.HasPrefix(rel, "assets/") {
					// Vite emits content-hashed filenames under assets/.
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				// #nosec G703 -- fs.ValidPath (stdlib-recommended fs.FS traversal
				// guard) already rejected "..", empty, and rooted elements above;
				// gosec's default taint sanitizer list doesn't know this stdlib func.
				http.ServeFileFS(w, r, assets, rel)
				return
			}
		}

		// SPA fallback: client-side routes render from index.html.
		w.Header().Set("Cache-Control", "no-store")
		http.ServeFileFS(w, r, assets, "index.html")
	})
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(api.SimpleErrorResponse(status, msg))
}
