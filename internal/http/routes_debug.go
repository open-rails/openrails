package server

import (
	"github.com/gin-gonic/gin"
)

func (s *Server) registerDebugRoutes(e *gin.Engine) {
	if s.cfg == nil || !s.cfg.IsDev() {
		return
	}

	debug := e.Group("/debug")
	nmi := debug.Group("/nmi")
	nmi.GET("/tokenization", s.debugNMITokenization)
	nmi.GET("/collect-stub.js", s.debugNMICollectStubJS)
}
