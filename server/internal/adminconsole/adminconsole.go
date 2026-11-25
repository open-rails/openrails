// Package adminconsole serves the standalone server's admin console SPA.
package adminconsole

import (
	"encoding/json"
	"fmt"
	"html"
	"io/fs"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

	"github.com/open-rails/openrails/internal/api"
)

// Config is the SPA bootstrap document served at <path>/config.json.
type Config struct {
	// AuthBaseURL is the base under which the AuthKit authhttp routes live
	// (capabilities, password/login, token, me, OIDC). Default: "/auth/v1".
	AuthBaseURL string `json:"auth_base_url"`
	// APIBaseURL is the admin API base, default "/v1".
	APIBaseURL string `json:"api_base_url"`
	// Exchange is where the console trades its session's token for one bound
	// to the merchant staff choose (RFC 8693, the merchant's id as group_id).
	Exchange *Exchange `json:"exchange"`
}

// Exchange is the console's token exchange: its public client at the token
// endpoint, for the deployment's resource and the merchant scope.
type Exchange struct {
	TokenEndpoint string `json:"token_endpoint"`
	ClientID      string `json:"client_id"`
	Resource      string `json:"resource"`
	Scope         string `json:"scope"`
}

// Merchant is the merchant a console mount serves.
type Merchant struct {
	ID          string `json:"id"`
	Slug        string `json:"slug"`
	DisplayName string `json:"display_name,omitempty"`
	Role        string `json:"role,omitempty"`
}

// Path is where the standalone server serves the console, apart from /v1.
const Path = "/admin"

// ClientID is the console's public OAuth client at the server's AuthKit, the
// token exchange's client.
const ClientID = "openrails-console"

// Present reports whether assets hold a servable console build: a non-nil
// fs.FS with index.html at its root. A source-only build without assets answers an explicit 503 at the console.
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
// at path, a validated mount path such as "/billing/admin":
// path/config.json from cfg, static files, and index.html — its <base href>
// rewritten to path/ — as the SPA fallback for client routes; bare path
// redirects to path/. Mount it at path without stripping the prefix (ServeMux
// "path/", gin "path/*any" and chi Mount all keep it): the build's URLs resolve
// against path, so a request outside it is a mount mistake, answered 500 and
// logged. Without a build
// every request answers 503 naming the build step.
func Handler(logger *slog.Logger, path string, cfg Config, assets fs.FS) (http.Handler, error) {
	if cfg.AuthBaseURL == "" {
		cfg.AuthBaseURL = "/auth/v1"
	}
	if cfg.APIBaseURL == "" {
		cfg.APIBaseURL = "/v1"
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
			logger.ErrorContext(r.Context(), "admin console: request is outside its configured path; mount it on the root router without stripping the prefix", "request_path", r.URL.Path, "console_path", path)
			writeError(w, http.StatusInternalServerError, "admin console is mounted outside its configured path")
			return
		}

		if rel == "config.json" {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			_ = json.NewEncoder(w).Encode(cfg)
			return
		}

		if !present {
			writeError(w, http.StatusServiceUnavailable,
				"admin console assets missing: build the SPA (scripts/build-admin-console.sh) before go build, or pass the build as server.Deps.ConsoleAssets")
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
