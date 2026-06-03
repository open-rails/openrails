package router

import (
	"net/http"
	"strings"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/http/request"
)

// muxRouter adapts a *http.ServeMux to the neutral Router. It is the embedded
// backend and imports ZERO gin (issue #282): every registered handler becomes an
// http.HandlerFunc that constructs request.NewHTTP(w, r, rt) and runs the neutral
// middleware chain then the handler.
//
// Patterns use Go 1.22 ServeMux method+path matching ("GET /billing/v1/me/{id}").
// gin-style params (":id") in the registration paths are converted to ServeMux
// wildcards ("{id}") so the same Register*Routes code drives both backends. The
// resulting r.PathValue("id") is read by request.NewHTTP's bindURI/param.
type muxRouter struct {
	mux    *http.ServeMux
	rt     *app.Runtime
	prefix string
	mw     []Middleware
}

// NewMux builds a neutral Router over a ServeMux rooted at basePrefix (e.g.
// "/billing/v1"). basePrefix is prepended to every registered pattern.
func NewMux(mux *http.ServeMux, basePrefix string, rt *app.Runtime) Router {
	return &muxRouter{mux: mux, rt: rt, prefix: basePrefix}
}

func (m *muxRouter) Handle(method, path string, h Handler, mw ...Middleware) {
	all := make([]Middleware, 0, len(m.mw)+len(mw))
	all = append(all, m.mw...)
	all = append(all, mw...)
	final := Chain(h, all)

	pattern := method + " " + toMuxPath(m.prefix+path)
	rt := m.rt
	m.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		final(request.NewHTTP(w, r, rt))
	})
}

func (m *muxRouter) Group(prefix string, mw ...Middleware) Router {
	all := make([]Middleware, 0, len(m.mw)+len(mw))
	all = append(all, m.mw...)
	all = append(all, mw...)
	return &muxRouter{
		mux:    m.mux,
		rt:     m.rt,
		prefix: m.prefix + prefix,
		mw:     all,
	}
}

// toMuxPath converts a gin-style path (":id") to a Go 1.22 ServeMux pattern
// ("{id}"). Each ":name" segment becomes "{name}". Other segments pass through.
func toMuxPath(p string) string {
	if !strings.Contains(p, ":") {
		return p
	}
	segments := strings.Split(p, "/")
	for i, seg := range segments {
		if strings.HasPrefix(seg, ":") {
			segments[i] = "{" + seg[1:] + "}"
		}
	}
	return strings.Join(segments, "/")
}
