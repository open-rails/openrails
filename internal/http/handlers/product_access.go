package handlers

import (
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/grants"
	"github.com/open-rails/openrails/internal/modules/productaccess"
	"github.com/open-rails/openrails/internal/pagination"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

// maxGrantNote bounds the note kept with a free grant.
const maxGrantNote = 1000

// productAccessGrant is the one projection of a product-access window onto the
// wire.
func productAccessGrant(row gen.ListProductAccessPageRow, now time.Time) billing.ProductAccessGrant {
	out := billing.ProductAccessGrant{
		ID: billing.ProductAccessID(row.ID), CustomerID: billing.CustomerID(row.CustomerID),
		ProductID: billing.ProductID(row.ProductID), ProductKey: row.ProductKey, ProductName: row.ProductName,
		SourceType: billing.ProductAccessSourceType(row.SourceType), SourceID: billing.SourceRef(row.SourceType, row.SourceID),
		StartsAt: row.StartsAt, EndsAt: row.EndsAt, RevokedAt: row.RevokedAt, RevokeReason: row.RevokeReason,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt, Quantity: models.DerefIntPtr(row.Quantity),
	}
	if row.PaymentID != nil {
		pid := billing.PaymentID(*row.PaymentID)
		out.PaymentID = &pid
	}
	if row.SourceType == string(models.AccessSourceGrant) {
		if row.GrantReason != nil {
			reason := billing.GrantReason(*row.GrantReason)
			out.GrantReason = &reason
		}
		out.GrantedBy, out.Note = row.Actor, row.Note
	}
	switch {
	case row.RevokedAt != nil:
		out.Status = "revoked"
	case row.StartsAt.After(now):
		out.Status = "scheduled"
	case row.EndsAt != nil && !row.EndsAt.After(now):
		out.Status = "expired"
	default:
		out.Status = "active"
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

// ListProductAccess is one page of product-access windows, newest first:
// of the customers ?customer_id= and the products ?product_id= name (comma
// separated, any when absent); ?live=true keeps those live now, ?ids reads
// named ones.
func ListProductAccess(r *httprequest.Request) {
	ids, ok := listIDs(r, billing.ParseProductAccessID)
	if !ok {
		return
	}
	svc := productAccessService(r)
	if svc == nil {
		r.ErrorCode(billing.CodeInternalError, "product access service unavailable")
		return
	}
	if ids != nil {
		if !requireMerchantRoutePrincipal(r) {
			return
		}
		rows, err := svc.ListByIDs(r.Request.Context(), uuidutil.Of(ids))
		if err != nil {
			r.InternalError("failed to list product access", err)
			return
		}
		now := r.Clock.Now()
		out := billing.ListPage[billing.ProductAccessGrant]{Items: make([]billing.ProductAccessGrant, len(rows))}
		for i, row := range rows {
			out.Items[i] = productAccessGrant(row, now)
		}
		r.SuccessJSON(out)
		return
	}
	customers, ok := queryCustomerIDs(r, "customer_id")
	if !ok {
		return
	}
	for _, id := range customers {
		if !requireServiceCustomerScope(r, id) {
			return
		}
	}
	if len(customers) == 0 && !requireMerchantRoutePrincipal(r) {
		return
	}
	products, ok := queryProductIDs(r, "product_id")
	if !ok {
		return
	}
	listProductAccess(r, productaccess.Filter{Customers: uuidutil.Of(customers), Products: uuidutil.Of(products)})
}

func listProductAccess(r *httprequest.Request, filter productaccess.Filter) {
	page, ok := r.Page()
	if !ok {
		return
	}
	limit, err := pagination.Limit(page)
	if err != nil {
		writeRefusal(r, err, "invalid page")
		return
	}
	if raw := strings.TrimSpace(r.Query("live")); raw != "" {
		if filter.LiveOnly, err = strconv.ParseBool(raw); err != nil {
			r.APIError(api.Coded(billing.CodeInvalidQuery, "live must be true or false").WithParam("live"))
			return
		}
	}
	var after uuid.UUID
	started, err := pagination.Decode(page.Cursor, &after)
	if err != nil {
		writeRefusal(r, err, "invalid cursor")
		return
	}
	var afterID *uuid.UUID
	if started {
		afterID = &after
	}
	svc := productAccessService(r)
	if svc == nil {
		r.ErrorCode(billing.CodeInternalError, "product access service unavailable")
		return
	}
	rows, more, err := svc.ListPage(r.Request.Context(), filter, afterID, limit)
	if err != nil {
		r.InternalError("failed to list product access", err)
		return
	}
	now := r.Clock.Now()
	out := billing.ListPage[billing.ProductAccessGrant]{Items: make([]billing.ProductAccessGrant, len(rows))}
	for i, row := range rows {
		out.Items[i] = productAccessGrant(row, now)
	}
	if more {
		out.Next = pagination.Encode(rows[len(rows)-1].ID)
	}
	r.SuccessJSON(out)
}

// queryProductIDs reads a comma-separated product id filter: 1 to
// billing.MaxBatchItems distinct ids, or none when the request names none.
func queryProductIDs(r *httprequest.Request, name string) ([]billing.ProductID, bool) {
	raw := strings.TrimSpace(r.Query(name))
	if raw == "" {
		return nil, true
	}
	parts := strings.Split(raw, ",")
	if len(parts) > billing.MaxBatchItems {
		r.APIError(api.Coded(billing.CodeInvalidQuery, fmt.Sprintf("%s names at most %d products", name, billing.MaxBatchItems)).WithParam(name))
		return nil, false
	}
	out := make([]billing.ProductID, 0, len(parts))
	for _, part := range parts {
		id, err := billing.ParseProductID(strings.TrimSpace(part))
		if err != nil || id.IsZero() {
			r.APIError(api.Coded(billing.CodeInvalidQuery, fmt.Sprintf("%s holds an invalid id %q", name, part)).WithParam(name))
			return nil, false
		}
		if !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out, true
}

// CreateProductAccess grants a batch of products free, across any customers,
// all or none in one transaction.
func CreateProductAccess(r *httprequest.Request) {
	var req billing.CreateProductAccessBatchParams
	if !r.BindJSON(&req) {
		return
	}
	if !batchItems(r, len(req.Items), billing.MaxBatchItems) {
		return
	}
	key := strings.TrimSpace(r.Header("Idempotency-Key"))
	if key == "" {
		key = uuidutil.NewV7().String()
	} else if len(key) > 200 {
		r.ErrorCode(billing.CodeInvalidParam, "Idempotency-Key is at most 200 bytes")
		return
	}
	admin, ok := r.Staff()
	if !ok {
		r.ErrorCode(billing.CodeAuthenticationRequired, "missing admin identity")
		return
	}
	now := r.Clock.Now()
	batch := make([]productaccess.Grant, len(req.Items))
	for i, item := range req.Items {
		param := func(field string) string { return apperr.ItemParam(i, field) }
		switch {
		case item.CustomerID.IsZero():
			r.APIError(api.Coded(billing.CodeInvalidParam, "customer_id is required").WithParam(param("customer_id")))
			return
		case item.ProductID.IsZero():
			r.APIError(api.Coded(billing.CodeInvalidParam, "product_id is required").WithParam(param("product_id")))
			return
		case item.Hours != nil && item.EndsAt != nil:
			r.APIError(api.Coded(billing.CodeInvalidParam, "hours and ends_at are mutually exclusive").WithParam(param("hours")))
			return
		case item.Hours != nil && (*item.Hours <= 0 || int64(*item.Hours) > maxGrantHours):
			r.APIError(api.Coded(billing.CodeInvalidParam, fmt.Sprintf("hours must be between 1 and %d", maxGrantHours)).WithParam(param("hours")))
			return
		case item.EndsAt != nil && !item.EndsAt.After(now):
			r.APIError(api.Coded(billing.CodeInvalidParam, "ends_at must be in the future").WithParam(param("ends_at")))
			return
		case item.Note != nil && (utf8.RuneCountInString(*item.Note) > maxGrantNote || strings.TrimSpace(*item.Note) == ""):
			r.APIError(api.Coded(billing.CodeInvalidParam, fmt.Sprintf("note must hold 1 to %d characters", maxGrantNote)).WithParam(param("note")))
			return
		}
		reason := grants.ReasonStaff
		switch item.Reason {
		case "":
		case billing.GrantReasonComp, billing.GrantReasonStaff, billing.GrantReasonImport:
			reason = grants.GrantReason(item.Reason)
		default:
			r.APIError(api.Coded(billing.CodeInvalidParam, "reason must be comp, staff or import").WithParam(param("reason")))
			return
		}
		if !requireServiceCustomerScope(r, identity.CustomerID(item.CustomerID)) {
			return
		}
		batch[i] = productaccess.Grant{
			CustomerID: item.CustomerID.UUID(), ProductID: item.ProductID.UUID(), Hours: item.Hours,
			Reason: reason, Note: item.Note, Actor: admin.Subject, IdempotencyKey: fmt.Sprintf("%s:%d", key, i),
		}
		if item.EndsAt != nil {
			end := item.EndsAt.UTC()
			batch[i].EndsAt = &end
		}
	}
	svc := productAccessService(r)
	if svc == nil {
		r.ErrorCode(billing.CodeInternalError, "product access service unavailable")
		return
	}
	granted, err := svc.GrantProducts(r.Request.Context(), batch)
	if err != nil {
		writeRefusal(r, err, "failed to grant product access")
		return
	}
	out := billing.CreateProductAccessBatchResult{Items: make([]billing.ProductAccessGrant, len(granted))}
	// A grant writes its window in its own transaction; the converge sweep
	// is the backstop. An inline converge would scan every window of a heavy
	// buyer on each grant.
	for i, row := range granted {
		out.Items[i] = productAccessGrant(row, now)
	}
	r.JSON(http.StatusCreated, out)
}

// RevokeProductAccess revokes a product-access window with the reason; a window
// not yet started is removed. Revoking again changes nothing.
func RevokeProductAccess(r *httprequest.Request) {
	id, err := billing.ParseProductAccessID(r.Param("id"))
	if err != nil || id.IsZero() {
		r.APIError(api.Coded(billing.CodeInvalidParam, "invalid product access id").WithParam("id"))
		return
	}
	if !requireMerchantRoutePrincipal(r) {
		return
	}
	var params billing.RevokeProductAccessParams
	if !r.BindJSON(&params) {
		return
	}
	params.Reason = strings.TrimSpace(params.Reason)
	if params.Reason == "" || utf8.RuneCountInString(params.Reason) > 500 {
		r.APIError(api.Coded(billing.CodeInvalidParam, "reason is required (at most 500 characters)").WithParam("reason"))
		return
	}
	svc := productAccessService(r)
	if svc == nil {
		r.ErrorCode(billing.CodeInternalError, "product access service unavailable")
		return
	}
	found, err := svc.RevokeProductAccess(r.Request.Context(), id.UUID(), models.AccessRevokeReason(params.Reason))
	if err != nil {
		r.InternalError("failed to revoke product access", err)
		return
	}
	if !found {
		r.ErrorCode(billing.CodeResourceNotFound, "product access not found")
		return
	}
	r.NoContent()
}
