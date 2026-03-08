package handlers

import (
	"net/http"
	"strings"

	httprequest "github.com/open-rails/openrails/internal/http/request"
	billingservice "github.com/open-rails/openrails/pkg/service"
)

type serviceCreateCreditTypeRequest struct {
	Name          string `json:"name"`
	DisplayName   string `json:"display_name"`
	Unit          string `json:"unit"`
	DecimalPlaces int    `json:"decimal_places"`
}

// POST /v1/credit-types
func ServiceCreateCreditType(r *httprequest.Request) {
	req := new(serviceCreateCreditTypeRequest)
	if !r.BindJSON(req) {
		return
	}

	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}

	out, err := svc.CreateCreditType(r.Request.Context(), billingservice.CreateCreditTypeRequest{
		Name:          req.Name,
		DisplayName:   req.DisplayName,
		Unit:          req.Unit,
		DecimalPlaces: req.DecimalPlaces,
	})
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	r.GinCtx.JSON(http.StatusOK, out)
}

// GET /v1/credit-types?active_only=true
func ServiceListCreditTypes(r *httprequest.Request) {
	activeOnly := true
	if v := strings.TrimSpace(r.GinCtx.Query("active_only")); v != "" {
		switch strings.ToLower(v) {
		case "1", "true", "yes":
			activeOnly = true
		case "0", "false", "no":
			activeOnly = false
		default:
			r.ErrorJSON(http.StatusBadRequest, "active_only must be true or false")
			return
		}
	}

	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	items, err := svc.ListCreditTypes(r.Request.Context(), activeOnly)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to list credit types")
		return
	}
	r.GinCtx.JSON(http.StatusOK, items)
}

type serviceUpdateCreditTypeRequest struct {
	DisplayName *string `json:"display_name"`
	IsActive    *bool   `json:"is_active"`
}

// PATCH /v1/credit-types/:name
func ServiceUpdateCreditType(r *httprequest.Request) {
	name := strings.TrimSpace(r.GinCtx.Param("name"))
	if name == "" {
		r.ErrorJSON(http.StatusBadRequest, "name is required")
		return
	}
	req := new(serviceUpdateCreditTypeRequest)
	if !r.BindJSON(req) {
		return
	}

	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	out, err := svc.UpdateCreditType(r.Request.Context(), name, billingservice.UpdateCreditTypeRequest{
		DisplayName: req.DisplayName,
		IsActive:    req.IsActive,
	})
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	r.GinCtx.JSON(http.StatusOK, out)
}

// POST /v1/credit-types/:name/deactivate
func ServiceDeactivateCreditType(r *httprequest.Request) {
	name := strings.TrimSpace(r.GinCtx.Param("name"))
	if name == "" {
		r.ErrorJSON(http.StatusBadRequest, "name is required")
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	if err := svc.DeactivateCreditType(r.Request.Context(), name); err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	r.GinCtx.JSON(http.StatusOK, map[string]any{"ok": true})
}

// POST /v1/credit-types/:name/activate
func ServiceActivateCreditType(r *httprequest.Request) {
	name := strings.TrimSpace(r.GinCtx.Param("name"))
	if name == "" {
		r.ErrorJSON(http.StatusBadRequest, "name is required")
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	if err := svc.ActivateCreditType(r.Request.Context(), name); err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	r.GinCtx.JSON(http.StatusOK, map[string]any{"ok": true})
}
