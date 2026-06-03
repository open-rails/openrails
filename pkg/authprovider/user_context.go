// Package authprovider holds the gin-free auth re-exports shared by the embedded
// and standalone surfaces. The gin-typed Provider interface, the
// Authenticator→Provider bridge, and the gin-context helpers live in the
// pkg/authprovider/ginauth subpackage so that gin-free importers (and therefore
// pkg/embedded) do not transitively pull in github.com/gin-gonic/gin (#285).
package authprovider

import (
	"github.com/open-rails/openrails/pkg/billingauth"
)

// UserContext is the framework-neutral identity contract. The canonical
// definition now lives in the gin-free pkg/billingauth; this alias preserves the
// historical authprovider import path. New gin-free code should depend on
// pkg/billingauth directly.
type UserContext = billingauth.UserContext

// Re-exported gin-free helpers (canonical home: pkg/billingauth). Kept here so
// existing importers of authprovider keep compiling during the library split.
var (
	// SetUserContext returns a child context with user context attached.
	SetUserContext = billingauth.SetUserContext
	// FromContext extracts user context from a standard context.
	FromContext = billingauth.FromContext
	// ErrUnauthenticated is returned when authentication is required but absent.
	ErrUnauthenticated = billingauth.ErrUnauthenticated
)
