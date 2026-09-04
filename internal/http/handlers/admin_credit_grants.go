package handlers

import (
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/controlplane"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/pkg/api"
	"github.com/open-rails/openrails/pkg/billingauth"
	billingservice "github.com/open-rails/openrails/pkg/service"
)

func creditPagination(r *httprequest.Request) (limit, offset int, ok bool) {
	limit, _ = strconv.Atoi(r.Request.URL.Query().Get("limit"))
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	offset, err := strconv.Atoi(r.Request.URL.Query().Get("offset"))
	if r.Request.URL.Query().Get("offset") == "" {
		err = nil
	}
	if err != nil || offset < 0 || offset > math.MaxInt32 {
		r.ErrorJSON(http.StatusBadRequest, "invalid offset")
		return 0, 0, false
	}
	return limit, offset, true
}

// ListAdminCreditGrants uses the same live gate as the write routes to report
// action availability; user roles and credential permissions are not guessed by UI.
func ListAdminCreditGrants(gate billingauth.Gate) func(*httprequest.Request) {
	return func(r *httprequest.Request) {
		payer, err := parseServiceCustomerID(r.Param("customer_id"))
		if err != nil || payer == nil {
			r.ErrorJSON(http.StatusBadRequest, "invalid customer_id")
			return
		}
		currency, ok := serviceRequiredCurrency(r, r.Request.URL.Query().Get("currency"))
		if !ok {
			return
		}
		limit, offset, ok := creditPagination(r)
		if !ok {
			return
		}
		svc, err := billingservice.New(r.State)
		if err != nil {
			r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
			return
		}
		page, err := svc.ListCreditGrants(r.Request.Context(), *payer, currency, limit, offset)
		if err != nil {
			r.ErrorJSON(http.StatusBadRequest, err.Error())
			return
		}
		canGrant, canRevoke := false, false
		if gate != nil {
			_, err = gate.Authorize(r.Request.Context(), r.Request, controlplane.PermMerchantCreditsGrant)
			canGrant = err == nil
			_, err = gate.Authorize(r.Request.Context(), r.Request, controlplane.PermMerchantCreditsRevoke)
			canRevoke = err == nil
		}
		r.SuccessJSON(struct {
			*billingservice.CreditGrantPage
			CanGrant  bool `json:"can_grant"`
			CanRevoke bool `json:"can_revoke"`
		}{page, canGrant, canRevoke})
	}
}

func RevokeAdminCreditGrant(r *httprequest.Request) {
	payer, err := parseServiceCustomerID(r.Param("customer_id"))
	if err != nil || payer == nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid customer_id")
		return
	}
	grantID, err := uuid.Parse(r.Param("grant_id"))
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid grant_id")
		return
	}
	var body struct {
		Reason string `json:"reason"`
	}
	if !r.BindJSON(&body) {
		return
	}
	body.Reason = strings.TrimSpace(body.Reason)
	if body.Reason == "" || utf8.RuneCountInString(body.Reason) > 500 {
		r.ErrorJSON(http.StatusBadRequest, "reason is required (maximum 500 characters)")
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	result, err := svc.RevokeCreditGrant(r.Request.Context(), *payer, grantID, body.Reason)
	if err != nil {
		switch {
		case errors.Is(err, billingservice.ErrCreditGrantNotFound):
			r.APIError(api.NewAPIError(http.StatusNotFound, api.ErrorTypeInvalidRequest, "credit_grant_not_found", "Credit grant not found."))
		case errors.Is(err, billingservice.ErrCreditGrantHeld):
			r.APIError(api.NewAPIError(http.StatusConflict, api.ErrorTypeInvalidRequest, "credit_grant_held", "The remaining credit is needed by active holds. Retry after those holds are released or settled."))
		case errors.Is(err, billingservice.ErrCreditGrantUnavailable):
			r.APIError(api.NewAPIError(http.StatusConflict, api.ErrorTypeInvalidRequest, "credit_grant_unavailable", "This credit grant has expired, ended, or has no remaining credit."))
		default:
			r.ErrorJSON(http.StatusInternalServerError, "credit revocation failed")
		}
		return
	}
	r.SuccessJSON(result)
}

func ListAdminCreditTransactions(r *httprequest.Request) {
	payer, err := parseServiceCustomerID(r.Param("customer_id"))
	if err != nil || payer == nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid customer_id")
		return
	}
	currency, ok := serviceRequiredCurrency(r, r.Request.URL.Query().Get("currency"))
	if !ok {
		return
	}
	limit, offset, ok := creditPagination(r)
	if !ok {
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	items, total, err := svc.GetCustomerCreditTransactions(r.Request.Context(), *payer, currency, limit, offset)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	out := make([]serviceTxnResponse, 0, len(items))
	for _, t := range items {
		out = append(out, serviceTxnResponse{ID: t.ID, CustomerID: t.CustomerID, Invoker: t.Invoker, Amount: t.Amount, Currency: t.Currency, TransactionType: t.TransactionType, Status: t.Status, Source: t.Source, CreatedAt: t.CreatedAt})
	}
	decimals, err := svc.CreditUnitDecimals(r.Request.Context(), currency)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "credit currency unavailable")
		return
	}
	r.SuccessJSON(map[string]any{"transactions": out, "total": total, "limit": limit, "offset": offset, "unit_decimals": decimals})
}
