package handlers

import (
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db/models"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/productaccess"
	"github.com/open-rails/openrails/internal/pagination"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// productAccessGrants enriches grants with their products' key and name from
// one batched load (best-effort: a missing product leaves them empty).
func productAccessGrants(r *httprequest.Request, grants []models.ProductAccessGrant) []billing.ProductAccessGrant {
	products := map[uuid.UUID]*models.Product{}
	if r.State != nil && r.State.ProductService != nil {
		if loaded, err := r.State.ProductService.GetByIDs(r.Request.Context(), models.DistinctProductIDs(grants)); err == nil {
			products = loaded
		}
	}
	out := make([]billing.ProductAccessGrant, 0, len(grants))
	for i := range grants {
		g := grants[i]
		resp := billing.ProductAccessGrant{
			ID:         billing.ProductAccessID(g.ID),
			CustomerID: billing.CustomerID(g.CustomerID),
			ProductID:  billing.ProductID(g.ProductID),
			SourceType: billing.EntitlementSourceType(g.SourceType),
			SourceID:   billing.SourceRef(string(g.SourceType), g.SourceID),
			Status:     string(g.Status),
			StartsAt:   g.StartsAt,
			EndsAt:     g.EndsAt,
			RevokedAt:  g.RevokedAt,
			CreatedAt:  g.CreatedAt,
			UpdatedAt:  g.UpdatedAt,
		}
		if g.PaymentID != nil {
			pid := billing.PaymentID(*g.PaymentID)
			resp.PaymentID = &pid
		}
		if g.RevokeReason != nil {
			reason := string(*g.RevokeReason)
			resp.RevokeReason = &reason
		}
		if prod := products[g.ProductID]; prod != nil {
			resp.ProductKey = prod.Key
			resp.ProductName = prod.DisplayName
		}
		out = append(out, resp)
	}
	return out
}

func productAccessService(r *httprequest.Request) *productaccess.Service {
	if r.State == nil {
		return nil
	}
	return r.State.ProductAccessService
}

// productAccessCustomer reads the {customer_id} path parameter.
func productAccessCustomer(r *httprequest.Request) (billing.CustomerID, bool) {
	customer := customerIDParam(r.Param("customer_id"))
	if customer.IsZero() {
		r.APIError(api.Coded(billing.CodeInvalidParam, "invalid customer_id").WithParam("customer_id"))
		return customer, false
	}
	return customer, requireServiceCustomerScope(r, identity.CustomerID(customer))
}

// ListProductAccess is one page of the products a customer has access to,
// newest grant first.
func ListProductAccess(r *httprequest.Request) {
	customer, ok := productAccessCustomer(r)
	if !ok {
		return
	}
	page, ok := r.Page()
	if !ok {
		return
	}
	limit, err := pagination.Limit(page)
	if err != nil {
		writeRefusal(r, err, "invalid page")
		return
	}
	limit = min(limit, billing.MaxProductAccessChecks)
	var after uuid.UUID
	if _, err := pagination.Decode(page.Cursor, &after); err != nil {
		writeRefusal(r, err, "invalid cursor")
		return
	}
	svc := productAccessService(r)
	if svc == nil {
		r.ErrorCode(billing.CodeInternalError, "product access service unavailable")
		return
	}
	grants, more, err := svc.ListAccessibleProductsPage(r.Request.Context(), customer.String(), after, limit)
	if err != nil {
		r.InternalError("failed to list accessible products", err)
		return
	}
	out := billing.ListPage[billing.ProductAccessGrant]{Items: productAccessGrants(r, grants)}
	if more && len(grants) > 0 {
		out.Next = pagination.Encode(grants[len(grants)-1].ID)
	}
	r.SuccessJSON(out)
}

// CheckProductAccess answers, for each requested product, whether the
// customer has access to it now.
func CheckProductAccess(r *httprequest.Request) {
	customer, ok := productAccessCustomer(r)
	if !ok {
		return
	}
	var req billing.CheckProductAccessParams
	if !r.BindJSON(&req) {
		return
	}
	if (req.ProductIDs == nil) == (req.ProductKeys == nil) {
		r.APIError(api.Coded(billing.CodeInvalidParam, "exactly one of product_ids and product_keys is required"))
		return
	}
	if n := len(req.ProductIDs) + len(req.ProductKeys); n == 0 || n > billing.MaxProductAccessChecks {
		r.APIError(api.Coded(billing.CodeInvalidParam, fmt.Sprintf("a check names 1 to %d products", billing.MaxProductAccessChecks)))
		return
	}
	for _, key := range req.ProductKeys {
		if !validProductAccessKey(key) {
			r.APIError(api.Coded(billing.CodeInvalidParam, "product_key is invalid").WithParam("product_keys"))
			return
		}
	}
	products := make([]uuid.UUID, 0, len(req.ProductIDs))
	for _, id := range req.ProductIDs {
		if id.IsZero() {
			r.APIError(api.Coded(billing.CodeInvalidParam, "invalid product_id").WithParam("product_ids"))
			return
		}
		products = append(products, id.UUID())
	}
	svc := productAccessService(r)
	if svc == nil {
		r.ErrorCode(billing.CodeInternalError, "product access service unavailable")
		return
	}
	access := map[string]bool{}
	if req.ProductKeys != nil {
		decisions, err := svc.CheckProductKeys(r.Request.Context(), customer.String(), req.ProductKeys)
		if err != nil {
			r.InternalError("failed to check product access", err)
			return
		}
		for key, decision := range decisions {
			access[key] = decision.HasAccess
		}
	} else {
		decisions, err := svc.CheckProducts(r.Request.Context(), customer.String(), products)
		if err != nil {
			r.InternalError("failed to check product access", err)
			return
		}
		for id, has := range decisions {
			access[billing.ProductID(id).String()] = has
		}
	}
	r.SuccessJSON(billing.ProductAccessCheck{Access: access})
}

// CreateProductAccess grants a batch of product accesses, across any
// customers, in one transaction. One admin's grant of a product to a customer
// is made once: a repeat answers the existing grant.
func CreateProductAccess(gate StaffCan) func(*httprequest.Request) {
	return func(r *httprequest.Request) { createProductAccess(r, gate) }
}

func createProductAccess(r *httprequest.Request, gate StaffCan) {
	var req billing.CreateProductAccessBatchParams
	if !r.BindJSON(&req) {
		return
	}
	if !batchItems(r, len(req.Items), billing.MaxBatchItems) {
		return
	}
	admin, ok := r.Staff()
	if !ok {
		r.ErrorCode(billing.CodeAuthenticationRequired, "missing admin identity")
		return
	}
	batch := make([]productaccess.GrantParams, len(req.Items))
	indefinite := false
	for i, item := range req.Items {
		switch {
		case item.CustomerID.IsZero():
			r.APIError(api.Coded(billing.CodeInvalidParam, "customer_id is required").WithParam(apperr.ItemParam(i, "customer_id")))
			return
		case item.ProductID.IsZero():
			r.APIError(api.Coded(billing.CodeInvalidParam, "product_id is required").WithParam(apperr.ItemParam(i, "product_id")))
			return
		}
		if !requireServiceCustomerScope(r, item.CustomerID) {
			return
		}
		productID := item.ProductID.UUID()
		batch[i] = productaccess.GrantParams{
			UserID:     item.CustomerID.String(),
			ProductID:  productID,
			SourceType: models.ProductAccessSourceAdmin,
			SourceID:   "admin:" + admin.Subject + ":" + productID.String(),
		}
		if item.EndsAt != nil {
			end := item.EndsAt.UTC()
			batch[i].EndsAt = &end
		} else {
			indefinite = true
		}
	}
	if indefinite && !permitPermanentGrant(r, gate) {
		return
	}
	svc := productAccessService(r)
	if svc == nil {
		r.ErrorCode(billing.CodeInternalError, "product access service unavailable")
		return
	}
	granted, err := svc.GrantProductAccessBatch(r.Request.Context(), batch)
	if err != nil {
		r.InternalError("failed to grant product access", err)
		return
	}
	grants := make([]models.ProductAccessGrant, len(granted))
	for i, grant := range granted {
		grants[i] = *grant
	}
	r.JSON(http.StatusCreated, billing.CreateProductAccessBatchResult{Items: productAccessGrants(r, grants)})
}

// DeleteProductAccess revokes one of the customer's product-access grants.
func DeleteProductAccess(r *httprequest.Request) {
	customer, ok := productAccessCustomer(r)
	if !ok {
		return
	}
	id, err := billing.ParseProductAccessID(r.Param("id"))
	if err != nil || id.IsZero() {
		r.APIError(api.Coded(billing.CodeInvalidParam, "invalid product access id").WithParam("id"))
		return
	}
	svc := productAccessService(r)
	if svc == nil {
		r.ErrorCode(billing.CodeInternalError, "product access service unavailable")
		return
	}
	grant, err := svc.GetGrant(r.Request.Context(), id.UUID())
	if err != nil {
		r.InternalError("failed to load grant", err)
		return
	}
	if grant == nil || grant.CustomerID != customer.UUID() {
		r.APIError(api.Coded(billing.CodeResourceNotFound, "product access not found"))
		return
	}
	found, err := svc.RevokeProductAccess(r.Request.Context(), id.UUID(), models.ProductAccessRevokeAdmin)
	if err != nil {
		r.InternalError("failed to revoke product access", err)
		return
	}
	if !found {
		r.APIError(api.Coded(billing.CodeResourceNotFound, "product access not found or already revoked"))
		return
	}
	r.Status(http.StatusNoContent)
}

func validProductAccessKey(key string) bool {
	return strings.TrimSpace(key) != "" && utf8.ValidString(key) && !strings.ContainsRune(key, 0)
}
