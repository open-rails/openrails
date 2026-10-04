package routes

import (
	"encoding/base64"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db/gen"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/catalogscope"
	handlers "github.com/open-rails/openrails/internal/http/handlers"
	"github.com/open-rails/openrails/internal/http/middleware"
	"github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/internal/modules/catalog"
)

// RegisterOwnedCatalogRoutes exposes only creator product/price operations.
// Provider configuration, entitlement grants, meters and batch application retain
// their merchant-administrator surfaces and are never mounted in this group.
func RegisterOwnedCatalogRoutes(rr router.Router, rt *app.Runtime, opts Options) {
	readActions := rr
	rr = withCatalogWritePolicy(rr, rt, opts)
	scope := []router.Middleware{}
	if rt != nil && rt.DB != nil {
		scope = append(scope, middleware.MerchantDBConnMW(rt.DB))
	}
	scope = append(scope, ownerCatalogScopeMW(rt, opts.Gate))
	read := append([]router.Middleware{opts.merchantActionPermissionMW(billing.MerchantCatalogOwnRead)}, scope...)
	write := append([]router.Middleware{opts.merchantActionPermissionMW(billing.MerchantCatalogOwnUpdate)}, scope...)
	rr.Handle(http.MethodGet, "", h(handlers.OwnCatalog), read...)
	rr.Handle(http.MethodPut, "", h(handlers.OwnCatalog), write...)
	rr.Handle(http.MethodGet, "/products", h(handlers.AdminListProducts), read...)
	readActions.Handle(http.MethodPost, "/offers/lookup", h(handlers.ListOffersForEntitlements), read...)
	rr.Handle(http.MethodPost, "/products", h(handlers.AdminCreateProduct), write...)
	rr.Handle(http.MethodGet, "/products/by-key/:key", h(handlers.AdminGetProductByKey), read...)
	rr.Handle(http.MethodPut, "/products/by-key/:key", h(handlers.AdminEnsureProduct), write...)
	rr.Handle(http.MethodGet, "/products/:id", h(handlers.AdminGetProduct), read...)
	rr.Handle(http.MethodPatch, "/products/:id", h(handlers.AdminUpdateProduct), write...)
	rr.Handle(http.MethodPost, "/products/:id/activate", h(handlers.AdminActivateProduct), write...)
	rr.Handle(http.MethodPost, "/products/:id/deactivate", h(handlers.AdminDeactivateProduct), write...)
	rr.Handle(http.MethodGet, "/prices", h(handlers.AdminListPrices), read...)
	rr.Handle(http.MethodPost, "/prices", h(handlers.AdminCreatePrice), write...)
	rr.Handle(http.MethodGet, "/prices/by-key/:key", h(handlers.AdminGetPriceByKey), read...)
	rr.Handle(http.MethodGet, "/prices/by-key/:key/history", h(handlers.AdminGetPriceKeyHistory), read...)
	rr.Handle(http.MethodGet, "/prices/:id", h(handlers.AdminGetPrice), read...)
	rr.Handle(http.MethodPatch, "/prices/:id", h(handlers.AdminUpdatePrice), write...)
	rr.Handle(http.MethodPost, "/prices/:id/activate", h(handlers.AdminActivatePrice), write...)
	rr.Handle(http.MethodPost, "/prices/:id/deactivate", h(handlers.AdminDeactivatePrice), write...)
	rr.Handle(http.MethodPost, "/prices/:id/key", h(handlers.AdminSetPriceKey), write...)
}

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

// RegisterCatalogCollectionRoutes is the separately authorized merchant-admin
// collection. Supplying an owner subject here is permitted only by that grant.
func RegisterCatalogCollectionRoutes(rr router.Router, rt *app.Runtime, opts Options) {
	rr = withCatalogWritePolicy(rr, rt, opts)
	var dbMW []router.Middleware
	if rt != nil && rt.DB != nil {
		dbMW = append(dbMW, middleware.MerchantDBConnMW(rt.DB))
	}
	read := append([]router.Middleware{opts.merchantActionPermissionMW(billing.MerchantCatalogRead)}, dbMW...)
	write := append([]router.Middleware{opts.merchantActionPermissionMW(billing.MerchantCatalogUpdate)}, dbMW...)
	rr.Handle(http.MethodGet, "", h(handlers.ListCatalogs), read...)
	rr.Handle(http.MethodPost, "", h(handlers.EnsureCatalogForOwner), write...)
	rr.Handle(http.MethodGet, "/by-owner", h(handlers.GetCatalogForOwner), read...)
	rr.Handle(http.MethodGet, "/:id", h(handlers.GetCatalog), read...)
}
