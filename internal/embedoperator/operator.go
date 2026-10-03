// Package operator provides explicit local maintenance tools for an embedded
// engine: filesystem manifests, provider imports, restore identity, and rebuilds.
// Applications use openrails.Client for ordinary billing and catalog operations.
// These tools require process ownership and cannot be invoked through HTTP.
package operator

import (
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/app"
)

// Operator holds local maintenance authority for an owned runtime.
// It exposes no database handle or engine services.
type Operator struct{ app *app.App }

// New attaches maintenance tools to an owned runtime. Invalid or uninitialized
// runtimes are rejected by each operation before accessing engine state.
func New(runtime *embed.Runtime) *Operator {
	return &Operator{app: app.HostGraph(runtime)}
}
