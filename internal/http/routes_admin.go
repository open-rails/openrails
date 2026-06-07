package server

import (
	"github.com/gin-gonic/gin"
	authpolicy "github.com/open-rails/openrails/internal/auth/policy"
	"github.com/open-rails/openrails/internal/http/router/ginrouter"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
)

func (s *Server) registerAdminRoutesAt(e *gin.Engine, apiPrefix string) {
	admin := e.Group(apiPrefix + "/admin")
	opts := httproutes.Options{
		Authenticator: s.embeddedAuthenticator(),
	}
	// #312: the control plane is the live admin authority — admin routes require
	// the openrails:admin permission held in the caller's own tenant. Guard
	// against passing a typed-nil pointer as a non-nil interface (which would make
	// the gate fire with a nil checker).
	if s.controlPlane != nil {
		opts.AdminPermissionChecker = authpolicy.AdminPermissionChecker(s.controlPlane)
	}
	httproutes.RegisterAdminRoutes(ginrouter.New(admin, s.runtime), s.runtime, opts)
}

func (s *Server) registerAdminRoutesOn(e *gin.Engine) {
	s.registerAdminRoutesAt(e, StandaloneV1Prefix)
}
