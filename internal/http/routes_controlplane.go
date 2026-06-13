package server

import (
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"

	cpginroutes "github.com/open-rails/openrails/internal/controlplane/ginroutes"
)

// ControlPlaneAuthPrefix is where OpenRails mounts the selected AuthKit route
// groups (#224). It is a deliberate, narrow surface — NOT AuthKit DefaultAPI.
const ControlPlaneAuthPrefix = "/auth"

// registerControlPlaneAuthRoutes selectively mounts the AuthKit route groups
// OpenRails intentionally exposes (#224 task 4). In locked-down/self-hosted mode
// this is only the login/session/user groups; AuthKit DefaultAPI() is never
// mounted.
func (s *Server) registerControlPlaneAuthRoutes(e *gin.Engine) {
	group := e.Group(ControlPlaneAuthPrefix)
	n := cpginroutes.MountAuthRoutes(s.controlPlane, group)
	log.WithFields(log.Fields{
		"prefix":      ControlPlaneAuthPrefix,
		"routes":      n,
		"self_hosted": s.controlPlane.SelfHostedPosture(),
	}).Info("control plane: mounted selective AuthKit route groups")
}
