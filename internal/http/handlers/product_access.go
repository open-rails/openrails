package handlers

import (
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/models"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/productaccess"
	"github.com/open-rails/openrails/pkg/identity"
)

// ProductAccessGrantResponse is the application-facing view of a durable product
// access grant (issue #250), enriched with product metadata so host apps can
// build a purchased-library view without querying the catalog or payment history.
type ProductAccessGrantResponse struct {
	ID           string     `json:"id"`
	CustomerID   string     `json:"customer_id"`
	ProductID    string     `json:"product_id"`
	ProductSlug  string     `json:"product_slug,omitempty"`
	ProductName  string     `json:"product_name,omitempty"`
	SourceType   string     `json:"source_type"`
	SourceID     string     `json:"source_id,omitempty"`
	PaymentID    *string    `json:"payment_id,omitempty"`
	Status       string     `json:"status"`
	StartsAt     time.Time  `json:"starts_at"`
	EndsAt       *time.Time `json:"ends_at,omitempty"`
	RevokedAt    *time.Time `json:"revoked_at,omitempty"`
	RevokeReason *string    `json:"revoke_reason,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

type productAccessPath struct {
	ProductID string `uri:"product_id" binding:"required"`
}

type adminUserProductAccessPath struct {
	UserID string `uri:"customer_id" binding:"required"`
}

type adminProductAccessGrantPath struct {
	UserID  string `uri:"customer_id" binding:"required"`
	GrantID string `uri:"id" binding:"required"`
}

type grantProductAccessRequest struct {
	ProductID string  `json:"product_id" binding:"required"`
	EndsAt    *string `json:"ends_at,omitempty"` // RFC3339; omit for indefinite
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
			CustomerID: g.CustomerID.String(),
			ProductID:  g.ProductID.String(),
			SourceType: string(g.SourceType),
			SourceID:   g.SourceID,
			Status:     string(g.Status),
			StartsAt:   g.StartsAt,
			EndsAt:     g.EndsAt,
			RevokedAt:  g.RevokedAt,
			CreatedAt:  g.CreatedAt,
			UpdatedAt:  g.UpdatedAt,
		}
		if g.PaymentID != nil {
			pid := g.PaymentID.String()
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
				resp.ProductSlug = prod.Slug
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
	grants, err := svc.ListAccessibleProducts(r.Request.Context(), user.ID)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to list accessible products")
		return
	}
	r.JSON(http.StatusOK, productAccessResponses(r, grants))
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
	productID, err := uuid.Parse(strings.TrimSpace(path.ProductID))
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid product_id format")
		return
	}
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

// productAccessCheckResponse is the typed payload for has-access checks.
type productAccessCheckResponse struct {
	CustomerID string `json:"customer_id"`
	ProductID  string `json:"product_id"`
	HasAccess  bool   `json:"has_access"`
}

func newProductAccessCheck(productID uuid.UUID, userID string, has bool) productAccessCheckResponse {
	return productAccessCheckResponse{CustomerID: identity.CustomerIDFromString(userID).UUID().String(), ProductID: productID.String(), HasAccess: has}
}

// --- API-key service (GET /v1/merchant/customers/:user_id/product-access) ---

// ServiceGetUserProductAccess lists a user's accessible products for a
// server-to-server (API-key) caller. Optional ?product_id=... narrows to a single
// has-access check.
func ServiceGetUserProductAccess(r *httprequest.Request) {
	userID := strings.TrimSpace(r.Param("user_id"))
	if userID == "" {
		r.ErrorJSON(http.StatusBadRequest, "user_id is required")
		return
	}
	svc := productAccessService(r)
	if svc == nil {
		r.ErrorJSON(http.StatusInternalServerError, "product access service unavailable")
		return
	}
	if productIDStr := strings.TrimSpace(r.Query("product_id")); productIDStr != "" {
		productID, err := uuid.Parse(productIDStr)
		if err != nil {
			r.ErrorJSON(http.StatusBadRequest, "invalid product_id format")
			return
		}
		has, err := svc.HasProductAccess(r.Request.Context(), userID, productID)
		if err != nil {
			r.ErrorJSON(http.StatusInternalServerError, "failed to check product access")
			return
		}
		r.JSON(http.StatusOK, newProductAccessCheck(productID, userID, has))
		return
	}
	grants, err := svc.ListAccessibleProducts(r.Request.Context(), userID)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to list accessible products")
		return
	}
	r.JSON(http.StatusOK, productAccessResponses(r, grants))
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
	productID, err := uuid.Parse(strings.TrimSpace(req.ProductID))
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid product_id format")
		return
	}
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
