// Package routebundle binds framework-neutral native route registrations.
package routebundle

import (
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/open-rails/openrails/internal/http/router"
)

type Route struct {
	Method  string
	Path    string
	Handler http.Handler
}

// FromTable binds a table's registrations for a host router. A router that
// matches in registration order (Fiber) must see a literal segment before a
// wildcard at the same position, so the routes are ordered that way whatever
// order they were declared in.
func FromTable(table *router.Table) []Route {
	routes := make([]Route, 0, len(table.Entries))
	for _, entry := range table.Entries {
		routes = append(routes, Route{Method: entry.Method, Path: entry.Path, Handler: BindPathValues(entry.Path, entry.Handler)})
	}
	sort.SliceStable(routes, func(i, j int) bool { return moreSpecific(routes[i].Path, routes[j].Path) })
	return routes
}

// moreSpecific reports whether path a must be matched before path b: at the
// first segment where exactly one of them is a wildcard, the literal wins.
func moreSpecific(a, b string) bool {
	as, bs := strings.Split(a, "/"), strings.Split(b, "/")
	for i := 0; i < len(as) && i < len(bs); i++ {
		if ra, rb := segmentRank(as[i]), segmentRank(bs[i]); ra != rb {
			return ra < rb
		}
	}
	return false
}

func segmentRank(segment string) int {
	switch {
	case strings.HasPrefix(segment, "{") && strings.HasSuffix(segment, "...}"):
		return 2
	case strings.HasPrefix(segment, "{") && strings.HasSuffix(segment, "}"):
		return 1
	}
	return 0
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
