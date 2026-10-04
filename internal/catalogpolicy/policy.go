// Package catalogpolicy controls ordinary catalog writes independently of
// provider credential custody. Operator authority is internal to the library.
package catalogpolicy

import (
	"context"
	"net/http"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/requestauth"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

type operatorKey struct{}

var ErrUpdatesDisabled = apperr.New(http.StatusForbidden, "catalog_updates_disabled", "ordinary catalog updates are disabled")

// Enabled reports whether catalog mutations are published to callers other
// than the process owner (HTTP and delegated credentials). Missing config
// never enables them.
func Enabled(cfg *config.Config) bool { return cfg != nil && cfg.AllowCatalogUpdates }

// OperatorContext is only for the trusted local operator construction boundary.
// HTTP handlers must never grant this capability.
func OperatorContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, operatorKey{}, true)
}

// Check protects mutations even when a caller bypasses HTTP route composition.
// The in-process host principal is the process owner: it writes its own
// catalog whatever the flag says; AllowCatalogUpdates gates everyone else.
func Check(ctx context.Context, cfg *config.Config) error {
	if Enabled(cfg) || ctx.Value(operatorKey{}) == true {
		return nil
	}
	if _, host := requestauth.HostPrincipalFromContext(ctx); host {
		return nil
	}
	return ErrUpdatesDisabled
}

var ErrDeclared = apperr.New(http.StatusMethodNotAllowed, billing.CodeCatalogDeclared,
	"the catalog is declared by the host (Config.Catalog); change the declaration and restart")

// Declared reports whether the host declares the merchant's catalog.
func Declared(cfg *config.Config) bool { return cfg != nil && cfg.Catalog != nil }

// CheckDeclared refuses a write to a declared catalog from anyone, the host
// included: the next boot would overwrite it. Only the boot application,
// with operator authority, writes it.
func CheckDeclared(ctx context.Context, cfg *config.Config) error {
	if !Declared(cfg) || ctx.Value(operatorKey{}) == true {
		return nil
	}
	return ErrDeclared
}
