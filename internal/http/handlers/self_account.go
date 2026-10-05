package handlers

import (
	"strings"

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

// selfAccountPayer resolves the acting payer from the delegated principal, or
// writes the error response and returns false.
func selfAccountPayer(r *httprequest.Request) (identity.CustomerID, bool) {
	user := r.GetUser()
	if user == nil || strings.TrimSpace(user.ID) == "" {
		r.ErrorCode(billing.CodeAuthenticationRequired, "User authentication required")
		return identity.CustomerID(uuid.Nil), false
	}
	payer := identity.CustomerIDFromString(user.ID)
	if payer.IsZero() {
		r.ErrorCode(billing.CodeInvalidParam, "payer could not be resolved from subject")
		return identity.CustomerID(uuid.Nil), false
	}
	return payer, true
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
