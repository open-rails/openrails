package server

import (
	"github.com/gin-gonic/gin"

	"github.com/open-rails/openrails/internal/handlers"
)

func (s *Server) registerDebugRoutes(e *gin.Engine) {
	if s.cfg == nil || !s.cfg.IsDev() {
		return
	}

	debug := e.Group("/debug")
	mobius := debug.Group("/mobius")
	mobius.GET("/tokenization", s.wrap(handlers.DebugMobiusTokenization))
	mobius.GET("/collect-stub.js", s.wrap(handlers.DebugMobiusCollectStubJS))
}
