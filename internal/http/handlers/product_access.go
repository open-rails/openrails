package handlers

import (
	"fmt"
	"net/http"
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
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
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

// ListProductAccess is one page of a customer's product-access windows,
// newest first; ?live=true keeps those live now.
func ListProductAccess(r *httprequest.Request) {
	customer, ok := productAccessCustomer(r)
	if !ok {
		return
	}
	listProductAccess(r, customer.UUID())
}

// SelfListProductAccess is one page of the customer's own product-access
// windows.
func SelfListProductAccess(r *httprequest.Request) {
	payer, ok := selfAccountPayer(r)
	if !ok {
		return
	}
	listProductAccess(r, payer.UUID())
}

func listProductAccess(r *httprequest.Request, customer uuid.UUID) {
	page, ok := r.Page()
	if !ok {
		return
	}
	limit, err := pagination.Limit(page)
	if err != nil {
		writeRefusal(r, err, "invalid page")
		return
	}
	liveOnly := false
	if raw := strings.TrimSpace(r.Query("live")); raw != "" {
		if liveOnly, err = strconv.ParseBool(raw); err != nil {
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
	rows, more, err := svc.ListPage(r.Request.Context(), customer, afterID, limit, liveOnly)
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

// CheckProductAccess answers, for each requested product, whether the
// customer holds it now: bought, subscribed or granted.
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

// CreateProductAccess grants a batch of products free, across any customers,
// all or none in one transaction. A grant with no end also needs
// merchant:access:grant-permanent.
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
	indefinite := false
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
		indefinite = indefinite || (item.Hours == nil && item.EndsAt == nil)
	}
	if indefinite && !permitPermanentGrant(r, gate) {
		return
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
	for i, row := range granted {
		out.Items[i] = productAccessGrant(row, now)
		convergeAfterMutation(r, row.CustomerID)
	}
	r.JSON(http.StatusCreated, out)
}

// DeleteProductAccess revokes one of the customer's product-access windows: a
// free grant in the grant ledger, so its future windows end with it.
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
	found, err := svc.RevokeProductAccess(r.Request.Context(), customer.UUID(), id.UUID(), models.AccessRevokeAdmin)
	if err != nil {
		r.InternalError("failed to revoke product access", err)
		return
	}
	if !found {
		r.APIError(api.Coded(billing.CodeResourceNotFound, "product access not found or already revoked"))
		return
	}
	convergeAfterMutation(r, customer.UUID())
	r.Status(http.StatusNoContent)
}

func validProductAccessKey(key string) bool {
	return strings.TrimSpace(key) != "" && utf8.ValidString(key) && !strings.ContainsRune(key, 0)
}
