// Package adminconsole serves the merchant admin console SPA (#740) from
// web/admin's build (#754): web/admin/dist when it was built before go build
// (scripts/build-admin-console.sh, in-repo `task admin-build`), else nothing.
package adminconsole

import (
	"encoding/json"
	"fmt"
	"html"
	"io/fs"
	"net/http"
	"regexp"
	"strings"

	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/api"
)

// Config is the SPA bootstrap document served at <path>/config.json.
type Config struct {
	// AuthBaseURL is the base under which the AuthKit authhttp routes live
	// (capabilities, password/login, token, me, OIDC). Standalone default:
	// "/auth/v1" (the control plane mount). Embedded: the host's AuthKit JSON
	// API, "/api/v1" by default, possibly on another origin.
	AuthBaseURL string `json:"auth_base_url"`
	// APIBaseURL is the merchant API base. Standalone default "/v1";
	// embedded hosts typically "/billing/v1".
	APIBaseURL string `json:"api_base_url"`
	// NLWidgetsEnabled mirrors the #741 fail-closed LLM gate: false hides the
	// natural-language widget box entirely (the generate endpoint is not mounted).
	// The manual widget builder is always available.
	NLWidgetsEnabled bool `json:"nl_widgets_enabled"`
	// AskEnabled mirrors the #756 metrics Q&A gate (llm.ask_enabled AND an LLM
	// key): false renders the Ask panel as a pointed empty-state (the ask
	// endpoint is not mounted). Distinct consent from NLWidgetsEnabled because /ask
	// sends aggregate query results to the LLM provider.
	AskEnabled bool `json:"ask_enabled"`
	// CatalogCopilotEnabled mirrors the #779 catalog copilot Q&A gate
	// (llm.catalog_copilot_enabled AND an LLM key): false renders the catalog
	// copilot panel as a pointed empty-state.
	CatalogCopilotEnabled bool `json:"catalog_copilot_enabled"`
	// CatalogDraftingEnabled mirrors the #779 Phase 2 gate
	// (llm.catalog_drafting_enabled): false hides the drafting UI entirely —
	// the copilot panel stays Q&A-only.
	CatalogDraftingEnabled bool `json:"catalog_drafting_enabled"`
	// NewMerchantURL is AdminConsoleConfig.NewMerchantURL: the host page behind
	// the console's "New merchant" action. Empty hides the action.
	NewMerchantURL string `json:"new_merchant_url"`
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

// baseTag is the mount placeholder in the build's index.html (#1127). The
// build's URLs are relative to it, so one build serves any path.
var baseTag = regexp.MustCompile(`<base\s+href="/admin/"\s*/?>`)

// Handler serves the console from assets (a Vite build rooted at index.html)
// at path, a validated AdminConsoleConfig.MountPath such as "/billing/admin":
// path/config.json from cfg, static files, and index.html — its <base href>
// rewritten to path/ — as the SPA fallback for client routes; bare path
// redirects to path/. Mount it at path without stripping the prefix (ServeMux
// "path/", gin "path/*any" and chi Mount all keep it): the build's URLs resolve
// against path, so a request outside it is a mount mistake, answered 500 and
// logged. Callers should gate mounting on Present(assets); without a build
// every request answers 503 naming the build step.
func Handler(path string, cfg Config, assets fs.FS) (http.Handler, error) {
	if cfg.AuthBaseURL == "" {
		cfg.AuthBaseURL = "/auth/v1"
	}
	if cfg.APIBaseURL == "" {
		cfg.APIBaseURL = "/v1"
	}
	configJSON, err := json.Marshal(cfg)
	if err != nil {
		panic(err) // static struct, cannot fail
	}
	present := Present(assets)
	var index []byte
	if present {
		raw, err := fs.ReadFile(assets, "index.html")
		if err != nil {
			return nil, fmt.Errorf("admin console: read index.html: %w", err)
		}
		if n := len(baseTag.FindAllIndex(raw, -1)); n != 1 {
			return nil, fmt.Errorf(`admin console: the build's index.html has %d <base href="/admin/"> tags, want 1; rebuild it (scripts/build-admin-console.sh)`, n)
		}
		index = baseTag.ReplaceAllLiteral(raw, []byte(`<base href="`+html.EscapeString(path+"/")+`">`))
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == path {
			target := path + "/"
			if r.URL.RawQuery != "" {
				target += "?" + r.URL.RawQuery
			}
			http.Redirect(w, r, target, http.StatusMovedPermanently)
			return
		}
		rel, ok := strings.CutPrefix(r.URL.Path, path+"/")
		if !ok {
			log.Errorf("admin console: request %q is outside its configured path %q; mount Client.AdminConsole() at Config.AdminConsole.Path without stripping the prefix", r.URL.Path, path)
			writeError(w, http.StatusInternalServerError, "admin console is mounted outside its configured path")
			return
		}

		if rel == "config.json" {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			_, _ = w.Write(configJSON)
			return
		}

		if !present {
			writeError(w, http.StatusServiceUnavailable,
				"admin console assets missing: build the SPA (scripts/build-admin-console.sh) before go build, or pass the build as Deps.ConsoleAssets")
			return
		}

		if rel != "" && rel != "index.html" && fs.ValidPath(rel) {
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
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(index)
	}), nil
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(api.SimpleErrorResponse(status, msg))
}
