// Package catalogpolicy controls ordinary catalog writes independently of
// provider credential custody. Operator authority is internal to the library.
package catalogpolicy

import (
	"context"
	"fmt"
	"net/http"
	"sync"

	"github.com/open-rails/openrails/internal/requestauth"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

type operatorKey struct{}

var ErrUpdatesDisabled = apperr.New(http.StatusForbidden, "catalog_updates_disabled", "ordinary catalog updates are disabled")

// Exposure is whether catalog writes reach callers other than the process
// owner: HTTP and delegated credentials. Mounting the admin API decides it
// (Routes.Permissions.CatalogWrite, unless Config.Catalog is the truth; on a standalone
// server, a writable secret backend); until then, and without that mount,
// only the owner writes. The zero value is undecided and closed.
type Exposure struct {
	mu      sync.Mutex
	decided bool
	enabled bool
}

// Decide records one mount's choice. Every mount in a process must agree:
// a conflicting one fails.
func (e *Exposure) Decide(enabled bool) error {
	if e == nil {
		return fmt.Errorf("catalog policy is not initialized")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.decided && e.enabled != enabled {
		return fmt.Errorf("openrails: the admin API is already mounted with catalog edits %t; every mount must agree on Routes.Permissions.CatalogWrite", e.enabled)
	}
	e.decided, e.enabled = true, enabled
	return nil
}

// Enabled reports whether a mount published catalog writes.
func (e *Exposure) Enabled() bool {
	if e == nil {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.enabled
}

// OperatorContext is only for the trusted local operator construction boundary.
// HTTP handlers must never grant this capability.
func OperatorContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, operatorKey{}, true)
}

// Check protects mutations even when a caller bypasses HTTP route composition.
// The in-process host principal is the process owner: it writes its own
// catalog whatever was mounted; the exposure gates everyone else.
func Check(ctx context.Context, exposure *Exposure) error {
	if exposure.Enabled() || ctx.Value(operatorKey{}) == true {
		return nil
	}
	if _, host := requestauth.HostPrincipalFromContext(ctx); host {
		return nil
	}
	return ErrUpdatesDisabled
}
