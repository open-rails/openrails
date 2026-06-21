package handlers

import (
	"fmt"
	"net/http"

	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/pkg/catalog"
)

type merchantCatalogPublishRequest struct {
	Catalog   catalog.Manifest `json:"catalog"`
	Insert    bool             `json:"insert"`
	Overwrite bool             `json:"overwrite"`
	Prune     bool             `json:"prune"`
	PlanOnly  *bool            `json:"plan_only"`
}

type merchantCatalogPublishResponse struct {
	Plan   *catalog.ApplyPlan   `json:"plan"`
	Result *catalog.ApplyResult `json:"result,omitempty"`
}

// MerchantPublishCatalog handles POST /v1/merchant/catalog/publish.
// It is the HTTP form of push-merchant-catalog for the current merchant only.
func MerchantPublishCatalog(r *httprequest.Request) {
	var req merchantCatalogPublishRequest
	if !r.BindJSON(&req) {
		return
	}
	if err := req.Catalog.Validate(); err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	applier := catalog.NewServiceApplier(svc)
	plan, err := catalog.Plan(r.Request.Context(), applier, &req.Catalog)
	if err != nil {
		writeCatalogError(r, fmt.Errorf("plan catalog: %w", err))
		return
	}
	resp := merchantCatalogPublishResponse{Plan: plan}
	if catalogPublishPlanOnly(req) {
		r.JSON(http.StatusOK, resp)
		return
	}
	result, err := catalog.ApplyWithOptions(r.Request.Context(), applier, plan, catalog.ApplyOptions{
		Insert:    req.Insert,
		Overwrite: req.Overwrite,
		Prune:     req.Prune,
	})
	if err != nil {
		writeCatalogError(r, fmt.Errorf("apply catalog: %w", err))
		return
	}
	resp.Result = result
	r.JSON(http.StatusOK, resp)
}

func catalogPublishPlanOnly(req merchantCatalogPublishRequest) bool {
	if req.PlanOnly != nil && *req.PlanOnly {
		return true
	}
	return !req.Insert && !req.Overwrite && !req.Prune
}
