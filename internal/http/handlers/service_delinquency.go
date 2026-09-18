package handlers

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/open-rails/openrails"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/delinquency"
	billingservice "github.com/open-rails/openrails/internal/service"
)

// serviceDelinquencyResponse is one payer's delinquency state on the wire;
// instants are time.Time so they encode at full RFC3339 precision.
type serviceDelinquencyResponse struct {
	CustomerID      openrails.CustomerID `json:"customer_id"`
	Currency        string               `json:"currency"`
	State           string               `json:"state"`
	OverdueSince    *time.Time           `json:"overdue_since,omitempty"`
	OverdueAmount   int64                `json:"overdue_amount,string"`
	OverdueInvoices int                  `json:"overdue_invoices"`
	EnteredAt       time.Time            `json:"entered_at"`
	EvaluatedAt     time.Time            `json:"evaluated_at"`
}

func serviceDelinquencyRows(rows []billingservice.DelinquencySnapshot) []serviceDelinquencyResponse {
	out := make([]serviceDelinquencyResponse, 0, len(rows))
	for _, r := range rows {
		row := serviceDelinquencyResponse{
			CustomerID:      openrails.CustomerID(r.CustomerID),
			Currency:        r.Currency,
			State:           r.State.String(),
			OverdueAmount:   r.OverdueAmount,
			OverdueInvoices: r.OverdueInvoices,
			EnteredAt:       r.EnteredAt.UTC(),
			EvaluatedAt:     r.EvaluatedAt.UTC(),
		}
		if r.OverdueSince != nil {
			since := r.OverdueSince.UTC()
			row.OverdueSince = &since
		}
		out = append(out, row)
	}
	return out
}

// ServiceListDelinquency returns the merchant's overdue roster (or#878) plus
// the effective policy it was judged against, so an operator reading "23 payers
// delinquent" can see the grace window and floor that produced the number.
//
// The roster is the EXCEPTION list: payers in good standing are never returned.
func ServiceListDelinquency(r *httprequest.Request) {
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	ctx := r.Request.Context()
	state := delinquency.State(strings.ToLower(strings.TrimSpace(r.Query("state"))))
	limit := 0
	if v := strings.TrimSpace(r.Query("limit")); v != "" {
		parsed, perr := strconv.Atoi(v)
		if perr != nil || parsed < 0 {
			r.ErrorJSON(http.StatusBadRequest, "invalid limit")
			return
		}
		limit = parsed
	}
	rows, err := svc.ListDelinquency(ctx, state, limit)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	policy, err := svc.GetDelinquencyPolicy(ctx)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "get delinquency policy failed")
		return
	}
	r.SuccessJSON(map[string]any{
		"delinquency": serviceDelinquencyRows(rows),
		"policy": map[string]any{
			"grace_days":   policy.GraceDays,
			"amount_floor": strconv.FormatInt(policy.AmountFloor, 10),
		},
	})
}

// ServiceGetCustomerDelinquency returns one payer's delinquency state in every
// currency it owes in. An empty list means the payer has never been overdue —
// absence IS `current`, and nothing is fabricated to fill the gap.
func ServiceGetCustomerDelinquency(r *httprequest.Request) {
	payer, err := parseServiceCustomerID(r.Param("customer_id"))
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	if payer == nil {
		r.ErrorJSON(http.StatusBadRequest, "customer_id required")
		return
	}
	if !requireServiceCustomerScope(r, *payer) {
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	rows, err := svc.GetDelinquency(r.Request.Context(), *payer)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	r.SuccessJSON(map[string]any{"delinquency": serviceDelinquencyRows(rows)})
}
