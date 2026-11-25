// Package router registers route handlers on a net/http ServeMux. Handlers
// and middleware operate on *request.Request.
package router

import (
	"log/slog"
	"net/http"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/http/request"
)

// Handler is a route handler.
type Handler func(*request.Request)

// Middleware wraps a Handler with around-the-handler behavior: it may run logic
// before calling next, short-circuit (e.g. AbortCode + return without calling
// next), and run deferred cleanup after next returns (e.g. release a pinned DB
// connection).
type Middleware func(next Handler) Handler

// Chain composes mw around h so that mw[0] runs first (outermost).
func Chain(h Handler, mw []Middleware) Handler {
	for i := len(mw) - 1; i >= 0; i-- {
		h = mw[i](h)
	}
	return h
}

type Registrar interface{ Handle(string, http.Handler) }

// Router registers handlers on a ServeMux under a path prefix. Paths are Go
// 1.22 ServeMux patterns ("/v1/me/invoices/{id}").
type Router struct {
	mux    Registrar
	rt     *app.Runtime
	prefix string
	record func(pattern string)
	Log    *slog.Logger
}

// NewMux builds a Router over mux rooted at prefix (e.g. "/v1"), which is
// prepended to every registered path.
func NewMux(mux Registrar, prefix string, rt *app.Runtime) *Router {
	return &Router{mux: mux, rt: rt, prefix: prefix, Log: rt.Log}
}

// NewMuxRecorded is NewMux plus a recorder invoked with each final ServeMux
// pattern ("GET /v1/me/balance"), for an assembly that exposes its route
// table.
func NewMuxRecorded(mux Registrar, prefix string, rt *app.Runtime, record func(pattern string)) *Router {
	return &Router{mux: mux, rt: rt, prefix: prefix, record: record, Log: rt.Log}
}

// Handle registers h for method+path, wrapped by mw in order (mw[0] is the
// outermost wrapper, runs first).
func (m *Router) Handle(method, path string, h Handler, mw ...Middleware) {
	final := Chain(h, mw)
	pattern := method + " " + m.prefix + path
	if m.record != nil {
		m.record(pattern)
	}
	rt := m.rt
	m.mux.Handle(pattern, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		final(request.New(m.Log, w, r, rt))
	}))
}
