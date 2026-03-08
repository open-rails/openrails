package server

import (
	"github.com/gin-gonic/gin"
)

func (s *Server) registerDebugRoutes(e *gin.Engine) {
	if s.cfg == nil || !s.cfg.IsDev() {
		return
	}

	debug := e.Group("/debug")
	mobius := debug.Group("/mobius")
	mobius.GET("/tokenization", s.debugMobiusTokenization)
	mobius.GET("/collect-stub.js", s.debugMobiusCollectStubJS)
}
