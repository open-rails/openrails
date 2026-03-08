package handlers

import (
	"net/http"
	"strings"

	"github.com/google/uuid"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	billingservice "github.com/open-rails/openrails/pkg/service"
)

func ServiceCreateProduct(r *httprequest.Request) {
	var req billingservice.CreateProductRequest
	if err := r.GinCtx.ShouldBindJSON(&req); err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid request")
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	out, err := svc.CreateProduct(r.Request.Context(), req)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	r.GinCtx.JSON(http.StatusOK, out)
}

func ServiceUpdateProduct(r *httprequest.Request) {
	id, err := uuid.Parse(strings.TrimSpace(r.GinCtx.Param("id")))
	if err != nil || id == uuid.Nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid product id")
		return
	}
	var req billingservice.UpdateProductRequest
	if err := r.GinCtx.ShouldBindJSON(&req); err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid request")
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	out, err := svc.UpdateProduct(r.Request.Context(), id, req)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	r.GinCtx.JSON(http.StatusOK, out)
}

func ServiceCreatePrice(r *httprequest.Request) {
	var req billingservice.CreatePriceRequest
	if err := r.GinCtx.ShouldBindJSON(&req); err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid request")
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	out, err := svc.CreatePrice(r.Request.Context(), req)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	r.GinCtx.JSON(http.StatusOK, out)
}

func ServiceUpdatePrice(r *httprequest.Request) {
	id, err := uuid.Parse(strings.TrimSpace(r.GinCtx.Param("id")))
	if err != nil || id == uuid.Nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid price id")
		return
	}
	var req billingservice.UpdatePriceRequest
	if err := r.GinCtx.ShouldBindJSON(&req); err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid request")
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	out, err := svc.UpdatePrice(r.Request.Context(), id, req)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	r.GinCtx.JSON(http.StatusOK, out)
}
