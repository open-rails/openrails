// Package gin is the thin gin compatibility shim over the framework-neutral
// OpenRails HTTP surface (#670). Hosts that mount billing onto an existing gin
// engine use RegisterAPI (the AuthKit-style front door, #623) or MountHandler
// (the net/http escape hatch). Everything here delegates to the neutral
// net/http handlers; gin appears only at the mount boundary (gin.WrapH in
// RegisterAPI).
package gin

import (
	"net/http"

	"github.com/open-rails/openrails/pkg/embedded"
)

// StandaloneHandler returns the full standalone HTTP surface for the embedded
// app.
//
// Deprecated: use embedded.StandaloneHandler — the surface is gin-free (#670);
// this delegating alias is kept for embedding hosts.
func StandaloneHandler(e *embedded.Embedded) (http.Handler, error) {
	return embedded.StandaloneHandler(e)
}

// Handler returns the full standalone HTTP surface.
//
// Deprecated: use embedded.StandaloneHandler. Embedded hosts should usually use
// RegisterAPI / MountHandler, or pkg/embedded.NewHTTPHandler.
func Handler(e *embedded.Embedded) (http.Handler, error) {
	return embedded.StandaloneHandler(e)
}
