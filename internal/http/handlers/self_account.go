package handlers

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/pkg/identity"
	billingservice "github.com/open-rails/openrails/pkg/service"
)

// Self-service money surface: the authenticated merchant_subject reads its own
// balance and transaction history.
//
// The payer is resolved exactly like the rest of /v1/me
// (identity.CustomerIDFromString over the acting subject — see
// GetMyUsage/GetMyInvoices), and every query runs RLS-scoped to the pinned
// merchant.

// selfAccountPayer resolves the acting payer from the delegated principal, or
// writes the error response and returns false.
func selfAccountPayer(r *httprequest.Request) (identity.CustomerID, bool) {
	user := r.GetUser()
	if user == nil || strings.TrimSpace(user.ID) == "" {
		r.ErrorJSON(http.StatusUnauthorized, "User authentication required")
		return identity.CustomerID(uuid.Nil), false
	}
	payer := identity.CustomerIDFromString(user.ID)
	if payer.IsZero() {
		r.ErrorJSON(http.StatusBadRequest, "payer could not be resolved from subject")
		return identity.CustomerID(uuid.Nil), false
	}
	return payer, true
}

type selfBalanceResponse struct {
	Currency      string `json:"currency"`
	BalanceAmount int64  `json:"balance_amount"`
}

func GetMyBalance(r *httprequest.Request) {
	payer, ok := selfAccountPayer(r)
	if !ok {
		return
	}
	currency, ok := serviceRequiredCurrency(r, r.Request.URL.Query().Get("currency"))
	if !ok {
		return
	}

	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	snap, err := svc.GetCreditAccount(r.Request.Context(), payer, currency)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	r.SuccessJSON(selfBalanceResponse{Currency: snap.Currency, BalanceAmount: snap.BalanceAmount})
}

// GetMyAccountTransactions (GET /v1/me/transactions?currency=&limit=&offset=)
// lists the authenticated subject's transactions for one currency
// (issue #339), newest first. Same wire shape as the service-side route, scoped
// to the delegated subject.
func GetMyAccountTransactions(r *httprequest.Request) {
	payer, ok := selfAccountPayer(r)
	if !ok {
		return
	}
	currency, ok := serviceRequiredCurrency(r, r.Request.URL.Query().Get("currency"))
	if !ok {
		return
	}

	limit, _ := strconv.Atoi(r.Request.URL.Query().Get("limit"))
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	offset, _ := strconv.Atoi(r.Request.URL.Query().Get("offset"))
	if offset < 0 {
		offset = 0
	}

	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	items, total, err := svc.GetCustomerCreditTransactions(r.Request.Context(), payer, currency, limit, offset)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	out := make([]serviceTxnResponse, 0, len(items))
	for _, t := range items {
		out = append(out, serviceTxnResponse{
			ID: t.ID, CustomerID: t.CustomerID, Invoker: t.Invoker, Amount: t.Amount,
			Currency: t.Currency, TransactionType: customerTransactionType(t.TransactionType), Status: t.Status, Source: t.Source,
			CreatedAt: t.CreatedAt,
		})
	}
	r.SuccessJSON(map[string]any{"transactions": out, "total": total})
}

func customerTransactionType(txType string) string {
	switch strings.ToLower(strings.TrimSpace(txType)) {
	case "withdrawal", "credit_spend":
		return "spend"
	case "credit_expire", "expiry":
		return "expiry"
	case "owed_accrual":
		return "arrears_accrual"
	case "owed_payment":
		return "arrears_payment"
	default:
		return txType
	}
}
