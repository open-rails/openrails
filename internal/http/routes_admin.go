package server

import (
	"github.com/gin-gonic/gin"
	authpolicy "github.com/open-rails/openrails/internal/auth/policy"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
)

func (s *Server) registerAdminRoutesAt(e *gin.Engine, apiPrefix string) {
	admin := e.Group(apiPrefix + "/admin")
	opts := httproutes.Options{AuthProvider: s.authProvider}
	// #224: when the control plane is enabled, make the operator org the live
	// admin authority. Guard against passing a typed-nil pointer as a non-nil
	// interface (which would make the gate fire with a nil checker).
	if s.controlPlane != nil {
		opts.OperatorPermissionChecker = authpolicy.OperatorPermissionChecker(s.controlPlane)
	}
	httproutes.RegisterAdminRoutes(admin, s.runtime, opts)
}

func (s *Server) registerAdminRoutesOn(e *gin.Engine) {
	s.registerAdminRoutesAt(e, StandaloneV1Prefix)
}
