package handlers

import (
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	billingservice "github.com/open-rails/openrails/internal/service"
)

// customerParam reads the {customer_id} path value of a merchant route.
func customerParam(r *httprequest.Request) (billing.CustomerID, bool) {
	id, err := billing.ParseCustomerID(r.Param("customer_id"))
	if err != nil || id.IsZero() {
		r.APIError(api.Coded("invalid_customer_id", "").WithParam("customer_id"))
		return billing.CustomerID{}, false
	}
	return id, requireServiceCustomerScope(r, id)
}

// requireServiceCustomerScope refuses a merchant route the staff gate did
// not authorize. Staff act on any customer of their merchant.
func requireServiceCustomerScope(r *httprequest.Request, _ billing.CustomerID) bool {
	return requireMerchantRoutePrincipal(r)
}

// serviceCustomerScopeAllows reports what requireServiceCustomerScope would
// decide, without answering.
func serviceCustomerScopeAllows(r *httprequest.Request, _ billing.CustomerID) bool {
	_, ok := r.Staff()
	return ok
}

// StaffCan asks the route's staff gate one more permission on the merchant
// it already authorized; nil grants it. A handler whose answer depends on it
// (a permanent grant, which invoice actions to offer) takes one.
type StaffCan func(r *http.Request, permission string) error

// requireMerchantRoutePrincipal is a merchant handler's own check that the
// staff gate authorized the request.
func requireMerchantRoutePrincipal(r *httprequest.Request) bool {
	if _, ok := r.Staff(); ok {
		return true
	}
	r.ErrorCode(billing.CodeAuthenticationRequired, "")
	return false
}

// billingService is the request's billing service, or an answered 500.
func billingService(r *httprequest.Request) (*billingservice.Service, bool) {
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.InternalError("billing service unavailable", err)
		return nil, false
	}
	return svc, true
}

// writeMoneyError answers a credit, admission or usage refusal with its
// registered code.
func writeMoneyError(r *httprequest.Request, err error, internalMessage string) {
	switch {
	case errors.Is(err, billingservice.ErrIdempotencyKeyReused):
		r.APIError(api.Coded(billing.CodeIdempotencyKeyReused, err.Error()))
	case errors.Is(err, billing.ErrInsufficientCredits):
		r.APIError(api.Coded(billing.CodeInsufficientCredits, err.Error()))
	case errors.Is(err, billingservice.ErrCreditGrantNotFound):
		r.APIError(api.Coded("credit_grant_not_found", err.Error()))
	case errors.Is(err, billingservice.ErrCreditGrantHeld):
		r.APIError(api.Coded("credit_grant_held", err.Error()))
	case errors.Is(err, billingservice.ErrCreditGrantUnavailable):
		r.APIError(api.Coded("credit_grant_unavailable", err.Error()))
	case errors.Is(err, billingservice.ErrInvalidInvokerSpendLimit):
		r.APIError(api.Coded(billing.CodeInvalidParam, err.Error()))
	case errors.Is(err, billingservice.ErrUsageOutsideIngestWindow):
		r.APIError(api.Coded(billing.CodeInvalidParam, err.Error()).WithParam("occurred_at"))
	default:
		writeRefusal(r, err, internalMessage)
	}
}

// CreateCreditGrant grants a customer prepaid credit.
func CreateCreditGrant(r *httprequest.Request) {
	customer, ok := customerParam(r)
	if !ok {
		return
	}
	var params billing.CreateCreditGrantParams
	if !r.BindJSON(&params) {
		return
	}
	if params.Amount <= 0 {
		r.APIError(api.Coded(billing.CodeInvalidParam, "amount must be positive").WithParam("amount"))
		return
	}
	if strings.TrimSpace(params.SourceID) == "" || len(params.SourceID) > maxSourceIDBytes {
		r.APIError(api.Coded(billing.CodeInvalidParam, "source_id must contain 1 to 255 bytes").WithParam("source_id"))
		return
	}
	if strings.TrimSpace(params.Source) == "" {
		r.APIError(api.Coded(billing.CodeInvalidParam, "source is required").WithParam("source"))
		return
	}
	svc, ok := billingService(r)
	if !ok {
		return
	}
	grant, err := svc.CreateCreditGrant(r.Request.Context(), customer, params)
	if err != nil {
		writeMoneyError(r, err, "credit grant failed")
		return
	}
	status := http.StatusCreated
	if grant.Replayed {
		status = http.StatusOK
	}
	r.JSON(status, grant)
}

// ListCreditGrants lists a customer's credit grants, newest first.
func ListCreditGrants(r *httprequest.Request) {
	customer, ok := customerParam(r)
	if !ok {
		return
	}
	var params billing.CreditGrantListParams
	if !r.BindQuery(&params) {
		return
	}
	if params.PageRequest, ok = r.Page(); !ok {
		return
	}
	svc, ok := billingService(r)
	if !ok {
		return
	}
	page, err := svc.ListCreditGrants(r.Request.Context(), customer, params)
	if err != nil {
		writeMoneyError(r, err, "credit grant list failed")
		return
	}
	r.SuccessJSON(page)
}

func creditGrantParam(r *httprequest.Request) (billing.CreditGrantID, bool) {
	id, err := billing.ParseCreditGrantID(r.Param("id"))
	if err != nil || id.IsZero() {
		r.APIError(api.Coded(billing.CodeInvalidParam, "invalid credit grant id").WithParam("id"))
		return billing.CreditGrantID{}, false
	}
	return id, true
}

// GetCreditGrant reads one of a customer's credit grants.
func GetCreditGrant(r *httprequest.Request) {
	customer, ok := customerParam(r)
	if !ok {
		return
	}
	id, ok := creditGrantParam(r)
	if !ok {
		return
	}
	svc, ok := billingService(r)
	if !ok {
		return
	}
	grant, err := svc.GetCreditGrant(r.Request.Context(), customer, id)
	if err != nil {
		writeMoneyError(r, err, "credit grant read failed")
		return
	}
	r.SuccessJSON(grant)
}

// RevokeCreditGrant revokes a grant's unspent remainder.
func RevokeCreditGrant(r *httprequest.Request) {
	customer, ok := customerParam(r)
	if !ok {
		return
	}
	id, ok := creditGrantParam(r)
	if !ok {
		return
	}
	var params billing.RevokeCreditGrantParams
	if !r.BindJSON(&params) {
		return
	}
	params.Reason = strings.TrimSpace(params.Reason)
	if params.Reason == "" || utf8.RuneCountInString(params.Reason) > 500 {
		r.APIError(api.Coded(billing.CodeInvalidParam, "reason is required (at most 500 characters)").WithParam("reason"))
		return
	}
	svc, ok := billingService(r)
	if !ok {
		return
	}
	grant, err := svc.RevokeCreditGrant(r.Request.Context(), customer, id, params.Reason)
	if err != nil {
		writeMoneyError(r, err, "credit grant revocation failed")
		return
	}
	r.SuccessJSON(grant)
}

// ListCreditTransactions lists a customer's ledger in one currency.
func ListCreditTransactions(r *httprequest.Request) {
	customer, ok := customerParam(r)
	if !ok {
		return
	}
	listCreditTransactions(r, customer)
}

func listCreditTransactions(r *httprequest.Request, customer billing.CustomerID) {
	var params billing.CreditTransactionListParams
	if !r.BindQuery(&params) {
		return
	}
	var ok bool
	if params.PageRequest, ok = r.Page(); !ok {
		return
	}
	svc, ok := billingService(r)
	if !ok {
		return
	}
	page, err := svc.ListCreditTransactions(r.Request.Context(), customer, params)
	if err != nil {
		writeMoneyError(r, err, "credit transaction list failed")
		return
	}
	r.SuccessJSON(page)
}

// currencyQuery is a route's ?currency=.
type currencyQuery struct {
	Currency string `form:"currency"`
}

// GetBalance returns a customer's money in one currency.
func GetBalance(r *httprequest.Request) {
	customer, ok := customerParam(r)
	if !ok {
		return
	}
	getBalance(r, customer)
}

func getBalance(r *httprequest.Request, customer billing.CustomerID) {
	var q currencyQuery
	if !r.BindQuery(&q) {
		return
	}
	svc, ok := billingService(r)
	if !ok {
		return
	}
	balance, err := svc.GetBalance(r.Request.Context(), customer, q.Currency)
	if err != nil {
		writeMoneyError(r, err, "balance read failed")
		return
	}
	r.SuccessJSON(balance)
}

// GetCreditLimit returns how much a customer may owe in arrears.
func GetCreditLimit(r *httprequest.Request) {
	customer, ok := customerParam(r)
	if !ok {
		return
	}
	var q currencyQuery
	if !r.BindQuery(&q) {
		return
	}
	svc, ok := billingService(r)
	if !ok {
		return
	}
	limit, err := svc.GetCreditLimit(r.Request.Context(), customer, q.Currency)
	if err != nil {
		writeMoneyError(r, err, "credit limit read failed")
		return
	}
	r.SuccessJSON(limit)
}

// SetCreditLimit sets how much a customer may owe in arrears.
func SetCreditLimit(r *httprequest.Request) {
	customer, ok := customerParam(r)
	if !ok {
		return
	}
	var params billing.SetCreditLimitParams
	if !r.BindJSON(&params) {
		return
	}
	svc, ok := billingService(r)
	if !ok {
		return
	}
	limit, err := svc.SetCreditLimit(r.Request.Context(), customer, params)
	if err != nil {
		writeMoneyError(r, err, "credit limit update failed")
		return
	}
	r.SuccessJSON(limit)
}

// GetTrustLevel returns a customer's stored trust level.
func GetTrustLevel(r *httprequest.Request) {
	customer, ok := customerParam(r)
	if !ok {
		return
	}
	var q currencyQuery
	if !r.BindQuery(&q) {
		return
	}
	svc, ok := billingService(r)
	if !ok {
		return
	}
	level, err := svc.GetCustomerTrustLevel(r.Request.Context(), customer, q.Currency)
	if err != nil {
		writeMoneyError(r, err, "trust level read failed")
		return
	}
	r.SuccessJSON(level)
}

// SetTrustLevel stores a customer's trust level.
func SetTrustLevel(r *httprequest.Request) {
	customer, ok := customerParam(r)
	if !ok {
		return
	}
	var params billing.SetTrustLevelParams
	if !r.BindJSON(&params) {
		return
	}
	svc, ok := billingService(r)
	if !ok {
		return
	}
	level, err := svc.SetTrustLevel(r.Request.Context(), customer, params)
	if err != nil {
		writeMoneyError(r, err, "trust level update failed")
		return
	}
	r.SuccessJSON(level)
}
