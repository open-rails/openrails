// Package catalogpolicy controls ordinary catalog writes independently of
// provider credential custody. Operator authority is internal to the library.
package catalogpolicy

import (
	"context"
	"net/http"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

type operatorKey struct{}

var ErrUpdatesDisabled = apperr.New(http.StatusForbidden, "catalog_updates_disabled", "ordinary catalog updates are disabled")

// Enabled reports the ordinary catalog mutation capability. Missing config
// never enables writes.
func Enabled(cfg *config.Config) bool { return cfg != nil && cfg.AllowCatalogUpdates }

// OperatorContext is only for the trusted local operator construction boundary.
// HTTP handlers and public Clients must never grant this capability.
func OperatorContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, operatorKey{}, true)
}

// Check protects mutations even when a caller bypasses HTTP route composition.
func Check(ctx context.Context, cfg *config.Config) error {
	if Enabled(cfg) || ctx.Value(operatorKey{}) == true {
		return nil
	}
	return ErrUpdatesDisabled
}
