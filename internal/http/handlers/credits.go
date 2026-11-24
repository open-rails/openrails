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
		r.APIError(api.Coded(billing.CodeInvalidCustomerID, "").WithParam("customer_id"))
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
	svc, err := billingservice.New(r.Log, r.State)
	if err != nil {
		r.InternalError("billing service unavailable", err)
		return nil, false
	}
	return svc, true
}

// writeMoneyError answers a credit, admission or usage refusal with its
// registered code.
func writeMoneyError(r *httprequest.Request, err error, internalMessage string) {
	setRetryAfter(r, err)
	if out := moneyRefusal(err); out != nil {
		r.APIError(out)
		return
	}
	writeRefusal(r, err, internalMessage)
}

// moneyRefusal is the answer to a money refusal, nil for an unclassified
// error.
func moneyRefusal(err error) *api.APIError {
	switch {
	case errors.Is(err, billingservice.ErrIdempotencyKeyReused):
		return api.Coded(billing.CodeIdempotencyKeyReused, err.Error())
	case errors.Is(err, billing.ErrInsufficientCredits):
		return api.Coded(billing.CodeInsufficientCredits, err.Error())
	case errors.Is(err, billingservice.ErrCreditGrantNotFound):
		return api.Coded(billing.CodeCreditGrantNotFound, err.Error())
	case errors.Is(err, billingservice.ErrCreditGrantHeld):
		return api.Coded(billing.CodeCreditGrantHeld, err.Error())
	case errors.Is(err, billingservice.ErrCreditGrantUnavailable):
		return api.Coded(billing.CodeCreditGrantUnavailable, err.Error())
	case errors.Is(err, billingservice.ErrUsageOutsideIngestWindow):
		return api.Coded(billing.CodeInvalidParam, err.Error()).WithParam("occurred_at")
	}
	return refusalOf(err)
}

// CreateCreditGrants grants prepaid credit across customers, all or none.
// It answers 201, or 200 when every grant replayed one already made.
func CreateCreditGrants(r *httprequest.Request) {
	var params billing.CreateCreditGrantBatchParams
	if !r.BindJSON(&params) {
		return
	}
	if !batchItems(r, len(params.Items), billing.MaxBatchItems) {
		return
	}
	for _, item := range params.Items {
		if !item.CustomerID.IsZero() && !requireServiceCustomerScope(r, item.CustomerID) {
			return
		}
	}
	svc, ok := billingService(r)
	if !ok {
		return
	}
	grants, err := svc.CreateCreditGrants(r.Request.Context(), params.Items)
	if err != nil {
		writeMoneyError(r, err, "credit grant failed")
		return
	}
	status := http.StatusOK
	for _, grant := range grants {
		if !grant.Replayed {
			status = http.StatusCreated
		}
	}
	r.JSON(status, billing.CreateCreditGrantBatchResult{Items: grants})
}

// ListCreditGrants lists a customer's credit grants, newest first.
func ListCreditGrants(r *httprequest.Request) {
	var params billing.CreditGrantListParams
	if !bindList(r, &params, &params.PageRequest) {
		return
	}
	var ok bool
	if params.IDs, ok = listIDs(r, params.IDs); !ok {
		return
	}
	if params.IDs == nil {
		id := params.CustomerID
		if id.IsZero() {
			r.APIError(api.Coded(billing.CodeInvalidCustomerID, "customer_id is required").WithParam("customer_id"))
			return
		}
		if !requireServiceCustomerScope(r, id) {
			return
		}
		params.CustomerID = id
	}
	svc, ok := billingService(r)
	if !ok {
		return
	}
	page, err := svc.ListCreditGrants(r.Request.Context(), params)
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
	return id, requireMerchantRoutePrincipal(r)
}

// GetCreditGrant reads one credit grant.
func GetCreditGrant(r *httprequest.Request) {
	id, ok := creditGrantParam(r)
	if !ok {
		return
	}
	svc, ok := billingService(r)
	if !ok {
		return
	}
	grant, err := svc.GetCreditGrant(r.Request.Context(), id)
	if err != nil {
		writeMoneyError(r, err, "credit grant read failed")
		return
	}
	r.SuccessJSON(grant)
}

// RevokeCreditGrant revokes a grant's unspent remainder.
func RevokeCreditGrant(r *httprequest.Request) {
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
	var actor string
	if admin, ok := r.Staff(); ok {
		actor = admin.Subject
	}
	grant, err := svc.RevokeCreditGrant(r.Request.Context(), id, params.Reason, actor)
	if err != nil {
		writeMoneyError(r, err, "credit grant revocation failed")
		return
	}
	r.SuccessJSON(grant)
}

// ListBalanceTransactions lists a customer's ledger in one currency.
func ListBalanceTransactions(r *httprequest.Request) {
	customer, ok := customerParam(r)
	if !ok {
		return
	}
	var params billing.BalanceTransactionListParams
	if !bindList(r, &params, &params.PageRequest) {
		return
	}
	if params.IDs, ok = listIDs(r, params.IDs); !ok {
		return
	}
	listBalanceTransactions(r, customer, params)
}

// MyBalanceTransactionListQuery is a customer's own ledger's query.
type MyBalanceTransactionListQuery struct {
	billing.PageRequest
	Currency string `form:"currency"`
}

func listBalanceTransactions(r *httprequest.Request, customer billing.CustomerID, params billing.BalanceTransactionListParams) {
	svc, ok := billingService(r)
	if !ok {
		return
	}
	page, err := svc.ListBalanceTransactions(r.Request.Context(), customer, params)
	if err != nil {
		writeMoneyError(r, err, "balance transaction list failed")
		return
	}
	r.SuccessJSON(page)
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
	var q billing.GetBalanceParams
	if !r.BindQuery(&q) {
		return
	}
	svc, ok := billingService(r)
	if !ok {
		return
	}
	balance, err := svc.GetBalance(r.Request.Context(), customer, string(q.Currency))
	if err != nil {
		writeMoneyError(r, err, "balance read failed")
		return
	}
	r.SuccessJSON(balance)
}
