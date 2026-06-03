// Package ginrouter holds the gin adapter for the neutral router.Router (issue
// #282). It lives in its OWN package so that internal/http/router — and thus the
// embedded net/http path that uses router.NewMux — imports ZERO gin. gin enters
// the build only through this package (used by the standalone gin server and the
// embedded gin RegisterXxxRoutes helpers).
package ginrouter

import (
	"github.com/gin-gonic/gin"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/http/request/ginreq"
	"github.com/open-rails/openrails/internal/http/router"
)

// ginRouter adapts a *gin.RouterGroup to the neutral router.Router. Every
// registered handler is wrapped into a gin.HandlerFunc that constructs
// request.New(c, rt) and runs the neutral middleware chain then the handler.
// Paths stay gin-style (":id").
type ginRouter struct {
	group *gin.RouterGroup
	rt    *app.Runtime
	mw    []router.Middleware
}

// New builds a neutral router.Router over a gin route group.
func New(group *gin.RouterGroup, rt *app.Runtime) router.Router {
	return &ginRouter{group: group, rt: rt}
}

func (g *ginRouter) Handle(method, path string, h router.Handler, mw ...router.Middleware) {
	all := make([]router.Middleware, 0, len(g.mw)+len(mw))
	all = append(all, g.mw...)
	all = append(all, mw...)
	final := router.Chain(h, all)
	g.group.Handle(method, path, func(c *gin.Context) {
		final(ginreq.New(c, g.rt))
	})
}

func (g *ginRouter) Group(prefix string, mw ...router.Middleware) router.Router {
	all := make([]router.Middleware, 0, len(g.mw)+len(mw))
	all = append(all, g.mw...)
	all = append(all, mw...)
	return &ginRouter{
		group: g.group.Group(prefix),
		rt:    g.rt,
		mw:    all,
	}
}
