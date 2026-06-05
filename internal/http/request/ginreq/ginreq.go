// Package ginreq holds the gin backend for the framework-neutral Request. It is
// imported only by the standalone (gin) HTTP path. The Request type and its
// net/http backend live in internal/http/request, which stays gin-free so that
// gin-free importers (ultimately pkg/embedded) do not transitively pull in
// github.com/gin-gonic/gin (#285).
package ginreq

import (
	"mime/multipart"

	"github.com/gin-gonic/gin"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/pkg/authprovider"
	"github.com/open-rails/openrails/pkg/authprovider/ginauth"
)

// New builds a gin-backed Request (standalone server).
func New(c *gin.Context, runtime *app.Runtime) *request.Request {
	return request.NewWithTransport(runtime, c.Request, ginTransport{c: c})
}

// ginTransport implements request.Transport over a *gin.Context.
type ginTransport struct{ c *gin.Context }

func (g ginTransport) WriteJSON(code int, body any) { g.c.JSON(code, body) }
func (g ginTransport) AbortJSON(code int, body any) { g.c.AbortWithStatusJSON(code, body) }
func (g ginTransport) Bind(data any) error          { return g.c.Bind(data) }
func (g ginTransport) BindJSON(data any) error      { return g.c.ShouldBindJSON(data) }
func (g ginTransport) BindQuery(data any) error     { return g.c.ShouldBindQuery(data) }
func (g ginTransport) BindURI(data any) error       { return g.c.ShouldBindUri(data) }
func (g ginTransport) Param(key string) string      { return g.c.Param(key) }
func (g ginTransport) Query(key string) string      { return g.c.Query(key) }
func (g ginTransport) Get(key string) (any, bool)   { return g.c.Get(key) }
func (g ginTransport) Set(key string, value any)    { g.c.Set(key, value) }
func (g ginTransport) Next()                        { g.c.Next() }
func (g ginTransport) Header(key string) string      { return g.c.GetHeader(key) }
func (g ginTransport) SetHeader(key, value string)   { g.c.Header(key, value) }
func (g ginTransport) Redirect(code int, location string) {
	g.c.Redirect(code, location)
}
func (g ginTransport) PostForm(key string) string { return g.c.PostForm(key) }
func (g ginTransport) FormFile(key string) (multipart.File, *multipart.FileHeader, error) {
	return g.c.Request.FormFile(key)
}
func (g ginTransport) UserContext() (authprovider.UserContext, bool) {
	return ginauth.UserContextFromGin(g.c)
}
