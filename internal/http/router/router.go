// Package router is the framework-neutral routing abstraction that lets the
// embedded billing surface be assembled on net/http (gin-free) while the
// standalone server keeps using gin (issue #282).
//
// Route registration code (internal/http/routes) targets the neutral Router +
// Middleware below instead of *gin.RouterGroup. Two adapters implement Router:
//   - ginRouter, over *gin.RouterGroup (standalone — keeps gin).
//   - muxRouter, over *http.ServeMux (embedded — imports zero gin).
//
// Handlers and middleware both operate on *request.Request, which already has
// gin and net/http backends, so neither sees the underlying framework.
package router

import (
	"github.com/open-rails/openrails/internal/http/request"
)

// Handler is a framework-neutral route handler. It mirrors the existing handler
// signature func(*request.Request).
type Handler func(*request.Request)

// Middleware wraps a Handler with around-the-handler behavior: it may run logic
// before calling next, short-circuit (e.g. AbortJSON + return without calling
// next), and run deferred cleanup after next returns (e.g. release a pinned DB
// connection). It is the neutral analogue of a gin.HandlerFunc that calls
// c.Next().
type Middleware func(next Handler) Handler

// Router is the framework-neutral registration surface. Paths use gin-style
// params (":id"); the muxRouter adapter converts them to Go 1.22 ServeMux
// patterns ("{id}").
type Router interface {
	// Handle registers h for method+path, wrapped by mw in order (mw[0] is the
	// outermost wrapper, runs first).
	Handle(method, path string, h Handler, mw ...Middleware)
	// Group returns a child Router rooted at prefix with mw applied to every
	// route registered through it (in addition to the parent's middleware).
	Group(prefix string, mw ...Middleware) Router
}

// chain composes mw around h so that mw[0] runs first (outermost).
func chain(h Handler, mw []Middleware) Handler {
	for i := len(mw) - 1; i >= 0; i-- {
		h = mw[i](h)
	}
	return h
}
