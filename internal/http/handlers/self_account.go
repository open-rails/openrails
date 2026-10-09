package handlers

import (
	"github.com/open-rails/openrails/billing"

	"github.com/google/uuid"

	identity "github.com/open-rails/openrails/internal/billingidentity"
	httprequest "github.com/open-rails/openrails/internal/http/request"
)

// Self-service money surface: the authenticated merchant_subject reads its own
// balance and transaction history and chooses its invoice collection method.
//
// The payer is resolved exactly like the rest of /v1/me
// (identity.CustomerIDFromString over the acting subject — see
// GetMyUsage/GetMyInvoices), and every query is scoped to the request
// merchant.

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

// GetMyBalance returns the customer's own money in one currency.
func GetMyBalance(r *httprequest.Request) {
	payer, ok := selfAccountPayer(r)
	if !ok {
		return
	}
	getBalance(r, payer)
}

// GetMyCreditTransactions lists the customer's own ledger in one currency.
func GetMyCreditTransactions(r *httprequest.Request) {
	payer, ok := selfAccountPayer(r)
	if !ok {
		return
	}
	listCreditTransactions(r, payer)
}
