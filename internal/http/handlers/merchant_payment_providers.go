package handlers

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"

	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/api"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Machine-readable codes of the provider-account lifecycle refusals (#655).
const (
	// CodeProviderAccountLastActive: the account is the only active one on its
	// rail; repeat with `allow_last: true` to archive it anyway.
	CodeProviderAccountLastActive = "provider_account_last_active"
	// CodeProviderAccountsAmbiguous: the rail-level DELETE found more than one
	// active account; archive by PSP id instead.
	CodeProviderAccountsAmbiguous = "provider_accounts_ambiguous"
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

// MerchantDeletePaymentProvider handles DELETE /v1/merchant/payment-providers/:provider:
// archive the rail's single active account. With several active accounts it
// answers 409 provider_accounts_ambiguous instead of guessing.
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

// MerchantArchivePaymentProviderAccount handles
// POST /v1/merchant/payment-providers/:provider/accounts/:psp_id/archive. The
// body is optional: `{"allow_last": true}` archives the rail's last active
// account. No provider call is made, so a dark account archives too.
func MerchantArchivePaymentProviderAccount(r *httprequest.Request) {
	svc, id, provider, ok := merchantProviderPathContext(r)
	if !ok {
		return
	}
	pspID, err := uuid.Parse(strings.TrimSpace(r.Param("psp_id")))
	if err != nil || pspID == uuid.Nil {
		r.ErrorJSON(http.StatusBadRequest, "psp_id must be the account's uuid")
		return
	}
	var req merchants.ArchivePaymentProviderAccountRequest
	if !decodeOptionalJSONBody(r, &req) {
		return
	}
	out, err := svc.ArchivePaymentProviderAccount(r.Request.Context(), id, provider, pspID, req)
	if err != nil {
		writeMerchantProviderError(r, err)
		return
	}
	r.JSON(http.StatusOK, map[string]any{"payment_provider": out})
}

// decodeOptionalJSONBody strictly decodes at most one JSON object; an empty
// body leaves body at its zero value.
func decodeOptionalJSONBody(r *httprequest.Request, body any) bool {
	decoder := json.NewDecoder(r.Request.Body)
	decoder.DisallowUnknownFields()
	err := decoder.Decode(body)
	if errors.Is(err, io.EOF) {
		return true
	}
	if err == nil && decoder.Decode(&json.RawMessage{}) != io.EOF {
		err = errors.New("request body must contain exactly one JSON object")
	}
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return false
	}
	return true
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
	var lastActive *merchants.LastActiveProviderAccountError
	var ambiguous *merchants.MultipleActiveProviderAccountsError
	switch {
	case errors.As(err, &lastActive):
		r.APIError(api.NewAPIError(http.StatusConflict, api.ErrorTypeInvalidRequest, CodeProviderAccountLastActive, err.Error()).
			WithMetadata(map[string]any{
				"rail":        lastActive.Rail,
				"environment": lastActive.Environment,
				"psp_id":      lastActive.Account.ID.String(),
				"account_id":  lastActive.Account.AccountID,
			}))
	case errors.As(err, &ambiguous):
		r.APIError(api.NewAPIError(http.StatusConflict, api.ErrorTypeInvalidRequest, CodeProviderAccountsAmbiguous, err.Error()).
			WithMetadata(map[string]any{
				"rail":        ambiguous.Rail,
				"environment": ambiguous.Environment,
				"accounts":    ambiguous.Accounts,
			}))
	case errors.Is(err, merchants.ErrPaymentProviderAccountNotFound):
		r.ErrorJSON(http.StatusNotFound, "payment provider account not found")
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
