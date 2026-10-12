package handlers

import (
	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	httprequest "github.com/open-rails/openrails/internal/http/request"
)

// Self-service money surface: the customer reads its own balance and ledger.
// The customer is the route gate's scope, and every query is scoped to the
// request merchant.

// selfAccountPayer is the customer route's verified customer, or writes the
// 401 and returns false.
func selfAccountPayer(r *httprequest.Request) (identity.CustomerID, bool) {
	scope, ok := r.CustomerScope()
	if !ok {
		r.ErrorCode(billing.CodeAuthenticationRequired, "")
		return identity.CustomerID(uuid.Nil), false
	}
	return identity.CustomerID(scope.Customer()), true
}

// GetMe answers the customer's own summary: money per currency, the card
// that pays each currency, and the low-balance line of each balance.
func GetMe(r *httprequest.Request) {
	payer, ok := selfAccountPayer(r)
	if !ok {
		return
	}
	svc, ok := billingService(r)
	if !ok {
		return
	}
	out, err := svc.GetCustomerAccount(r.Request.Context(), payer)
	if err != nil {
		r.InternalError("account read failed", err)
		return
	}
	r.SuccessJSON(out)
}

// ListMyBalanceTransactions lists the customer's own ledger in one currency.
func ListMyBalanceTransactions(r *httprequest.Request) {
	payer, ok := selfAccountPayer(r)
	if !ok {
		return
	}
	listBalanceTransactions(r, payer, nil)
}

// SetMyLowBalanceLine sets the customer's own low-balance line in one
// currency and answers the line its balance is judged at.
func SetMyLowBalanceLine(r *httprequest.Request) {
	payer, ok := selfAccountPayer(r)
	if !ok {
		return
	}
	currency, ok := serviceRequiredCurrency(r, r.Param("currency"))
	if !ok {
		return
	}
	var params billing.SetLowBalanceLineParams
	if !r.BindJSON(&params) {
		return
	}
	svc, ok := billingService(r)
	if !ok {
		return
	}
	out, err := svc.SetLowBalanceLine(r.Request.Context(), payer, currency, params)
	if err != nil {
		writeRefusal(r, err, "low-balance line update failed")
		return
	}
	r.SuccessJSON(out)
}
