package routes

import (
	"encoding/base64"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/catalogscope"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/http/handlers"
	"github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/internal/modules/catalog"
)

// ownerCatalogScopeMW pins a creator's own catalog on the request: the Gate's
// verified subject, or the OpenRails-Catalog-Owner an administrator selects.
func ownerCatalogScopeMW(rt *app.Runtime, gate billingauth.Gate) router.Middleware {
	return func(next router.Handler) router.Handler {
		return func(r *request.Request) {
			value, ok := r.Get(handlers.MerchantRoutePrincipalContextKey)
			principal, valid := value.(billingauth.Principal)
			if !ok || !valid {
				r.ErrorCode(billing.CodeCatalogOwnerRequired, "catalog owner identity required")
				return
			}
			subject := principal.Subject
			if subject == "" {
				// Only the identity returned by this Gate is a valid fallback.
				// Ambient user contexts, request headers and bodies are ignored.
				subject = principal.UserContext.UserID
			}
			if encoded := r.Header("OpenRails-Catalog-Owner"); encoded != "" {
				decoded, err := base64.RawURLEncoding.DecodeString(encoded)
				selected := string(decoded)
				if err != nil || catalogscope.ValidateSubject(selected) != nil {
					r.ErrorCode(billing.CodeInvalidParam, "invalid catalog owner selector")
					return
				}
				if selected != subject {
					permission := billing.MerchantCatalogRead
					if r.Request.Method != http.MethodGet && r.Request.Method != http.MethodHead {
						permission = billing.MerchantCatalogUpdate
					}
					authorized, err := gate.Authorize(r.Request.Context(), r.Request, permission)
					if err != nil || authorized.MerchantID != principal.MerchantID {
						r.ErrorCode(billing.CodePermissionRequired, "catalog owner selection requires administrator permission")
						return
					}
				}
				subject = selected
			}
			if err := catalogscope.ValidateSubject(subject); err != nil || principal.MerchantID.IsZero() {
				r.ErrorCode(billing.CodeCatalogOwnerRequired, "catalog owner identity required")
				return
			}
			if rt == nil || rt.DB == nil {
				r.ErrorCode(billing.CodeServiceUnavailable, "catalog unavailable")
				return
			}
			repo := catalog.NewCatalogRepo(rt.DB)
			var row gen.BillingCatalog
			var err error
			if r.Request.Method == http.MethodGet || r.Request.Method == http.MethodHead {
				row, err = repo.GetByOwner(r.Request.Context(), subject)
			} else {
				row, err = repo.Ensure(r.Request.Context(), &subject)
			}
			if errors.Is(err, pgx.ErrNoRows) {
				r.ErrorCode("catalog_not_found", "")
				return
			}
			if err != nil || row.MerchantID != principal.MerchantID.UUID() || row.OwnerSubject == nil || *row.OwnerSubject != subject {
				r.ErrorCode(billing.CodeInternalError, "catalog unavailable")
				return
			}
			ctx, err := catalogscope.WithOwner(r.Request.Context(), catalogscope.Scope{MerchantID: principal.MerchantID, CatalogID: row.ID, OwnerSubject: subject})
			if err != nil {
				r.ErrorCode("catalog_scope_mismatch", "")
				return
			}
			r.Request = r.Request.WithContext(ctx)
			next(r)
		}
	}
}
