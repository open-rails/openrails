package server

import (
	"github.com/gin-gonic/gin"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
)

func (s *Server) registerAdminRoutesAt(e *gin.Engine, apiPrefix string) {
	admin := e.Group(apiPrefix + "/admin")
	httproutes.RegisterAdminRoutes(admin, s.runtime, httproutes.Options{AuthProvider: s.authProvider})
}

func (s *Server) registerAdminRoutesOn(e *gin.Engine) {
	s.registerAdminRoutesAt(e, StandaloneV1Prefix)
}
