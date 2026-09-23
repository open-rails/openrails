package handlers

import (
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/open-rails/openrails"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db/models"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/productaccess"
)

// ProductAccessGrantResponse is the application-facing view of a durable product
// access grant (issue #250), enriched with product metadata so host apps can
// build a purchased-library view without querying the catalog or payment history.
type ProductAccessGrantResponse = openrails.ProductAccessGrant

type productAccessPath struct {
	ProductID openrails.ProductID `uri:"product_id" binding:"required"`
}

type adminUserProductAccessPath struct {
	UserID string `uri:"customer_id" binding:"required"`
}

type adminProductAccessGrantPath struct {
	UserID  string `uri:"customer_id" binding:"required"`
	GrantID string `uri:"id" binding:"required"`
}

type grantProductAccessRequest struct {
	ProductID openrails.ProductID `json:"product_id"`
	EndsAt    *string             `json:"ends_at,omitempty"` // RFC3339; omit for indefinite
}

// productAccessResponses enriches grants with product metadata. It loads each
// distinct product once via the ProductService (best-effort: a missing product
// just yields an unenriched row rather than failing the whole response).
func productAccessResponses(r *httprequest.Request, grants []models.ProductAccessGrant) []ProductAccessGrantResponse {
	out := make([]ProductAccessGrantResponse, 0, len(grants))
	cache := map[uuid.UUID]*models.Product{}
	for i := range grants {
		g := grants[i]
		resp := ProductAccessGrantResponse{
			ID:         g.ID.String(),
			CustomerID: openrails.CustomerID(g.CustomerID).String(),
			ProductID:  openrails.ProductID(g.ProductID).String(),
			SourceType: string(g.SourceType),
			SourceID:   openrails.SourceRef(string(g.SourceType), g.SourceID),
			Status:     string(g.Status),
			StartsAt:   g.StartsAt,
			EndsAt:     g.EndsAt,
			RevokedAt:  g.RevokedAt,
			CreatedAt:  g.CreatedAt,
			UpdatedAt:  g.UpdatedAt,
		}
		if g.PaymentID != nil {
			pid := openrails.PaymentID(*g.PaymentID).String()
			resp.PaymentID = &pid
		}
		if g.RevokeReason != nil {
			reason := string(*g.RevokeReason)
			resp.RevokeReason = &reason
		}
		if r.State != nil && r.State.ProductService != nil {
			prod, ok := cache[g.ProductID]
			if !ok {
				if p, err := r.State.ProductService.GetByID(r.Request.Context(), g.ProductID); err == nil {
					prod = p
				}
				cache[g.ProductID] = prod
			}
			if prod != nil {
				resp.ProductKey = prod.Key
				resp.ProductName = prod.DisplayName
			}
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

// --- User-facing (GET /v1/me/products, /v1/me/products/:product_id/access) ---

// GetMyProducts lists the authenticated user's accessible products (active
// grants), most recent first.
func GetMyProducts(r *httprequest.Request) {
	user := r.GetUser()
	if user == nil || user.ID == "" {
		r.ErrorJSON(http.StatusUnauthorized, "missing user identity")
		return
	}
	svc := productAccessService(r)
	if svc == nil {
		r.ErrorJSON(http.StatusInternalServerError, "product access service unavailable")
		return
	}
	listAccessibleProductsPage(r, svc, user.ID)
}

// GetMyProductAccess reports whether the authenticated user has access to a
// specific product.
func GetMyProductAccess(r *httprequest.Request) {
	user := r.GetUser()
	if user == nil || user.ID == "" {
		r.ErrorJSON(http.StatusUnauthorized, "missing user identity")
		return
	}
	var path productAccessPath
	if err := r.ShouldBindURI(&path); err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	if path.ProductID.IsZero() {
		r.ErrorJSON(http.StatusBadRequest, "invalid product_id format")
		return
	}
	productID := path.ProductID.UUID()
	svc := productAccessService(r)
	if svc == nil {
		r.ErrorJSON(http.StatusInternalServerError, "product access service unavailable")
		return
	}
	has, err := svc.HasProductAccess(r.Request.Context(), user.ID, productID)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to check product access")
		return
	}
	r.JSON(http.StatusOK, newProductAccessCheck(productID, user.ID, has))
}

func newProductAccessCheck(productID uuid.UUID, userID string, has bool) openrails.ProductAccessCheck {
	return openrails.ProductAccessCheck{CustomerID: openrails.CustomerID(identity.CustomerIDFromString(userID)).String(), ProductID: openrails.ProductID(productID).String(), HasAccess: has}
}

// --- API-key service (GET /v1/merchant/customers/:user_id/product-access) ---

// ServiceGetUserProductAccess lists a user's accessible products for a
// server-to-server (API-key) caller. Optional ?product_id=... narrows to a single
// has-access check.
func ServiceGetUserProductAccess(r *httprequest.Request) {
	user, err := openrails.ParseCustomerID(r.Param("user_id"))
	if err != nil || user.IsZero() {
		r.ErrorJSON(http.StatusBadRequest, "invalid customer_id")
		return
	}
	userID := user.String()
	svc := productAccessService(r)
	if svc == nil {
		r.ErrorJSON(http.StatusInternalServerError, "product access service unavailable")
		return
	}
	query := r.Request.URL.Query()
	if query.Has("product_id") && query.Has("product_key") {
		r.ErrorJSON(http.StatusBadRequest, "exactly one of product_id and product_key is required")
		return
	}
	if query.Has("product_key") {
		key := r.Query("product_key")
		if !validProductAccessKey(key) {
			r.ErrorJSON(http.StatusBadRequest, "product_key is invalid")
			return
		}
		decisions, err := svc.CheckProductKeys(r.Request.Context(), userID, []string{key})
		if err != nil {
			r.ErrorJSON(http.StatusInternalServerError, "failed to check product access")
			return
		}
		decision := decisions[key]
		out := openrails.ProductAccessCheck{CustomerID: userID, ProductKey: key, HasAccess: decision.HasAccess}
		if decision.ProductID != uuid.Nil {
			out.ProductID = openrails.ProductID(decision.ProductID).String()
		}
		r.JSON(http.StatusOK, out)
		return
	}
	if productIDStr := r.Query("product_id"); query.Has("product_id") {
		typedProductID, err := openrails.ParseProductID(productIDStr)
		if err != nil || typedProductID.IsZero() {
			r.ErrorJSON(http.StatusBadRequest, "invalid product_id format")
			return
		}
		productID := typedProductID.UUID()
		has, err := svc.HasProductAccess(r.Request.Context(), userID, productID)
		if err != nil {
			r.ErrorJSON(http.StatusInternalServerError, "failed to check product access")
			return
		}
		r.JSON(http.StatusOK, newProductAccessCheck(productID, userID, has))
		return
	}
	listAccessibleProductsPage(r, svc, userID)
}

// GrantAdminProductAccess creates a durable product access grant for a user
// (support comps / migrations / manual purchases). Idempotent at the service
// layer per (user, product, source).
func GrantAdminProductAccess(r *httprequest.Request) {
	var path adminUserProductAccessPath
	if err := r.ShouldBindURI(&path); err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	var req grantProductAccessRequest
	if !r.BindJSON(&req) {
		return
	}
	if req.ProductID.IsZero() {
		r.ErrorJSON(http.StatusBadRequest, "invalid product_id format")
		return
	}
	productID := req.ProductID.UUID()
	var endsAt *time.Time
	if req.EndsAt != nil && strings.TrimSpace(*req.EndsAt) != "" {
		parsed, perr := time.Parse(time.RFC3339, strings.TrimSpace(*req.EndsAt))
		if perr != nil {
			r.ErrorJSON(http.StatusBadRequest, "invalid ends_at timestamp; use RFC3339")
			return
		}
		e := parsed.UTC()
		endsAt = &e
	}
	admin := r.GetUser()
	if admin == nil || admin.ID == "" {
		r.ErrorJSON(http.StatusUnauthorized, "missing admin identity")
		return
	}
	svc := productAccessService(r)
	if svc == nil {
		r.ErrorJSON(http.StatusInternalServerError, "product access service unavailable")
		return
	}
	grant, _, err := svc.GrantProductAccess(r.Request.Context(), productaccess.GrantParams{
		UserID:     path.UserID,
		ProductID:  productID,
		SourceType: models.ProductAccessSourceAdmin,
		SourceID:   "admin:" + admin.ID + ":" + productID.String(),
		EndsAt:     endsAt,
	})
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, err.Error())
		return
	}
	resp := productAccessResponses(r, []models.ProductAccessGrant{*grant})
	r.JSON(http.StatusCreated, resp[0])
}

// RevokeAdminProductAccess revokes a single grant by id for a user.
func RevokeAdminProductAccess(r *httprequest.Request) {
	var path adminProductAccessGrantPath
	if err := r.ShouldBindURI(&path); err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	grantID, err := uuid.Parse(strings.TrimSpace(path.GrantID))
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid grant id format")
		return
	}
	svc := productAccessService(r)
	if svc == nil {
		r.ErrorJSON(http.StatusInternalServerError, "product access service unavailable")
		return
	}
	// Guard: the grant must belong to the path user (merchant scoping already
	// enforced by RLS).
	grant, err := svc.GetGrant(r.Request.Context(), grantID)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to load grant")
		return
	}
	if grant == nil || grant.CustomerID.String() != path.UserID {
		r.ErrorJSON(http.StatusNotFound, "grant not found for this user")
		return
	}
	found, err := svc.RevokeProductAccess(r.Request.Context(), grantID, models.ProductAccessRevokeAdmin)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, err.Error())
		return
	}
	if !found {
		r.ErrorJSON(http.StatusNotFound, "grant not found or already revoked")
		return
	}
	r.SuccessJSONMessage("product access revoked")
}

func listAccessibleProductsPage(r *httprequest.Request, svc *productaccess.Service, userID string) {
	limit := 25
	if raw := r.Query("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > openrails.ProductAccessMaxPageSize {
			r.ErrorJSON(http.StatusBadRequest, "limit must be between 1 and 100")
			return
		}
		limit = parsed
	}
	var cursor uuid.UUID
	if raw := r.Query("cursor"); raw != "" {
		parsed, err := uuid.Parse(raw)
		if err != nil || parsed == uuid.Nil || parsed.String() != raw {
			r.ErrorJSON(http.StatusBadRequest, "invalid cursor")
			return
		}
		cursor = parsed
	}
	grants, more, err := svc.ListAccessibleProductsPage(r.Request.Context(), userID, cursor, limit)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to list accessible products")
		return
	}
	page := openrails.ProductAccessList{Data: productAccessResponses(r, grants), HasMore: more}
	if more && len(grants) > 0 {
		page.NextCursor = grants[len(grants)-1].ID.String()
	}
	r.JSON(http.StatusOK, page)
}

func ServiceCheckUserProductAccess(r *httprequest.Request) {
	user, err := openrails.ParseCustomerID(r.Param("user_id"))
	if err != nil || user.IsZero() {
		r.ErrorJSON(http.StatusBadRequest, "invalid customer_id")
		return
	}
	var body struct {
		ProductIDs  []string `json:"product_ids"`
		ProductKeys []string `json:"product_keys"`
	}
	if !r.BindJSON(&body) {
		return
	}
	if (body.ProductIDs == nil) == (body.ProductKeys == nil) {
		r.ErrorJSON(http.StatusBadRequest, "exactly one of product_ids and product_keys is required")
		return
	}
	if len(body.ProductIDs)+len(body.ProductKeys) > openrails.ProductAccessMaxPageSize {
		r.ErrorJSON(http.StatusBadRequest, "at most 100 products are allowed")
		return
	}
	for _, key := range body.ProductKeys {
		if !validProductAccessKey(key) {
			r.ErrorJSON(http.StatusBadRequest, "product_key is invalid")
			return
		}
	}
	products := make([]uuid.UUID, 0, len(body.ProductIDs))
	for _, raw := range body.ProductIDs {
		product, err := openrails.ParseProductID(raw)
		if err != nil || product.IsZero() {
			r.ErrorJSON(http.StatusBadRequest, "invalid product_id")
			return
		}
		products = append(products, product.UUID())
	}
	svc := productAccessService(r)
	if svc == nil {
		r.ErrorJSON(http.StatusInternalServerError, "product access service unavailable")
		return
	}
	if body.ProductKeys != nil {
		decisions, err := svc.CheckProductKeys(r.Request.Context(), user.String(), body.ProductKeys)
		if err != nil {
			r.ErrorJSON(http.StatusInternalServerError, "failed to check product access")
			return
		}
		access := make(map[string]bool, len(decisions))
		for key, decision := range decisions {
			access[key] = decision.HasAccess
		}
		r.JSON(http.StatusOK, struct {
			Access map[string]bool `json:"access"`
		}{access})
		return
	}
	decisions, err := svc.CheckProducts(r.Request.Context(), user.String(), products)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to check product access")
		return
	}
	access := make(map[string]bool, len(decisions))
	for id, has := range decisions {
		access[openrails.ProductID(id).String()] = has
	}
	r.JSON(http.StatusOK, struct {
		Access map[string]bool `json:"access"`
	}{access})
}

func validProductAccessKey(key string) bool {
	return strings.TrimSpace(key) != "" && utf8.ValidString(key) && !strings.ContainsRune(key, 0)
}
