// Package routebundle binds framework-neutral native route registrations.
package routebundle

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/internal/requestauth"
)

type Route struct {
	Method  string
	Path    string
	Handler http.Handler
}

func FromTable(table *router.Table) []Route {
	routes := make([]Route, 0, len(table.Entries))
	for _, entry := range table.Entries {
		routes = append(routes, Route{Method: entry.Method, Path: entry.Path, Handler: withVerificationMemo(BindPathValues(entry.Path, entry.Handler))})
	}
	return routes
}

func BindPathValues(pattern string, next http.Handler) http.Handler {
	parts := strings.Split(strings.TrimPrefix(pattern, "/"), "/")
	if strings.HasSuffix(pattern, "...}") {
		// Only root-anchored console assets use subtree routes. Their handler
		// reads the original URL; customer exposure prefixes forbid subtrees.
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Mount groups add leading segments; route patterns describe the suffix.
		path := strings.Split(strings.Trim(r.URL.EscapedPath(), "/"), "/")
		if len(path) < len(parts) {
			http.NotFound(w, r)
			return
		}
		path = path[len(path)-len(parts):]
		for i, part := range parts {
			if strings.HasPrefix(part, "{") && strings.HasSuffix(part, "}") {
				value, err := url.PathUnescape(path[i])
				if err != nil {
					http.Error(w, "invalid path", 400)
					return
				}
				r.SetPathValue(part[1:len(part)-1], value)
			}
		}
		next.ServeHTTP(w, r)
	})
}

func withVerificationMemo(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { next.ServeHTTP(w, requestauth.Begin(r)) })
}
