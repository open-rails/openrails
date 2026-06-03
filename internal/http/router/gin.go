package router

import (
	"github.com/gin-gonic/gin"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/http/request"
)

// ginRouter adapts a *gin.RouterGroup to the neutral Router. It is the
// standalone backend: every registered handler is wrapped into a
// gin.HandlerFunc that constructs request.New(c, rt) and runs the neutral
// middleware chain then the handler. Paths stay gin-style (":id").
type ginRouter struct {
	group *gin.RouterGroup
	rt    *app.Runtime
	// mw is the accumulated middleware inherited from parent Group calls. It is
	// applied (outermost-first) around every handler registered on this router.
	mw []Middleware
}

// NewGin builds a neutral Router over a gin route group.
func NewGin(group *gin.RouterGroup, rt *app.Runtime) Router {
	return &ginRouter{group: group, rt: rt}
}

func (g *ginRouter) Handle(method, path string, h Handler, mw ...Middleware) {
	all := make([]Middleware, 0, len(g.mw)+len(mw))
	all = append(all, g.mw...)
	all = append(all, mw...)
	final := chain(h, all)
	g.group.Handle(method, path, func(c *gin.Context) {
		final(request.New(c, g.rt))
	})
}

func (g *ginRouter) Group(prefix string, mw ...Middleware) Router {
	all := make([]Middleware, 0, len(g.mw)+len(mw))
	all = append(all, g.mw...)
	all = append(all, mw...)
	return &ginRouter{
		group: g.group.Group(prefix),
		rt:    g.rt,
		mw:    all,
	}
}
