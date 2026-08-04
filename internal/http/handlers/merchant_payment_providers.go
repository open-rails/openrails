package handlers

import (
	"errors"
	"net/http"
	"strings"

	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/merchant"
)

// MerchantListPaymentProviders handles GET /v1/merchant/payment-providers.
// Routing lands in #555; this handler is intentionally route-agnostic.
func MerchantListPaymentProviders(r *httprequest.Request) {
	svc, id, ok := merchantProviderContext(r)
	if !ok {
		return
	}
	items, err := svc.ListPaymentProviderConfigs(
		r.Request.Context(),
		id,
		r.Query("provider"),
		r.Query("environment"),
		r.Query("status"),
	)
	if err != nil {
		writeMerchantProviderError(r, err)
		return
	}
	r.JSON(http.StatusOK, map[string]any{
		"data":                 items,
		"provider_definitions": merchants.PaymentProviderDefinitions(),
	})
}

// MerchantGetPaymentProvider handles GET /v1/merchant/payment-providers/:provider.
func MerchantGetPaymentProvider(r *httprequest.Request) {
	svc, id, provider, ok := merchantProviderPathContext(r)
	if !ok {
		return
	}
	out, err := svc.GetPaymentProviderConfig(r.Request.Context(), id, provider, r.Query("environment"))
	if err != nil {
		writeMerchantProviderError(r, err)
		return
	}
	r.JSON(http.StatusOK, map[string]any{"payment_provider": out})
}

// MerchantPutPaymentProvider handles PUT /v1/merchant/payment-providers/:provider.
func MerchantPutPaymentProvider(r *httprequest.Request) {
	svc, id, provider, ok := merchantProviderPathContext(r)
	if !ok {
		return
	}
	var req merchants.UpsertPaymentProviderConfigRequest
	if !r.BindJSON(&req) {
		return
	}
	out, err := svc.UpsertPaymentProviderConfig(r.Request.Context(), id, provider, req)
	if err != nil {
		writeMerchantProviderError(r, err)
		return
	}
	r.JSON(http.StatusOK, map[string]any{"payment_provider": out})
}

// MerchantDeletePaymentProvider handles DELETE /v1/merchant/payment-providers/:provider.
func MerchantDeletePaymentProvider(r *httprequest.Request) {
	svc, id, provider, ok := merchantProviderPathContext(r)
	if !ok {
		return
	}
	out, err := svc.DeletePaymentProviderConfig(r.Request.Context(), id, provider, r.Query("environment"))
	if err != nil {
		writeMerchantProviderError(r, err)
		return
	}
	r.JSON(http.StatusOK, map[string]any{"payment_provider": out})
}

func merchantProviderPathContext(r *httprequest.Request) (*merchants.Service, merchant.ID, string, bool) {
	svc, id, ok := merchantProviderContext(r)
	if !ok {
		return nil, merchant.ID{}, "", false
	}
	provider := strings.TrimSpace(r.Param("provider"))
	if provider == "" {
		r.ErrorJSON(http.StatusBadRequest, "provider required")
		return nil, merchant.ID{}, "", false
	}
	return svc, id, provider, true
}

func merchantProviderContext(r *httprequest.Request) (*merchants.Service, merchant.ID, bool) {
	if r.State == nil || r.State.Merchants == nil {
		r.ErrorJSON(http.StatusServiceUnavailable, "merchant provider config unavailable")
		return nil, merchant.ID{}, false
	}
	id, ok := merchant.FromContext(r.Request.Context())
	if !ok {
		r.ErrorJSON(http.StatusInternalServerError, "merchant context missing")
		return nil, merchant.ID{}, false
	}
	return r.State.Merchants, id, true
}

func writeMerchantProviderError(r *httprequest.Request, err error) {
	switch {
	case errors.Is(err, merchants.ErrSecretNotFound):
		r.ErrorJSON(http.StatusNotFound, "payment provider not configured")
	case errors.Is(err, merchants.ErrSecretBackendUnavailable):
		r.ErrorJSON(http.StatusServiceUnavailable, "secret backend unavailable")
	case strings.Contains(err.Error(), "required"),
		strings.Contains(err.Error(), "unsupported"),
		strings.Contains(err.Error(), "environment"),
		strings.Contains(err.Error(), "invalid"),
		strings.Contains(err.Error(), "unknown"):
		r.ErrorJSON(http.StatusBadRequest, err.Error())
	default:
		r.ErrorJSON(http.StatusInternalServerError, err.Error())
	}
}
