package handlers

import (
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/api"
	"github.com/open-rails/openrails/internal/db/gen"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/pagination"
)

// ListDisputes lists the merchant's disputes, newest first.
//
//	GET /admin/disputes?kind&status&reason&network&psp_id&customer_id&payment_id&since&until&limit&cursor
func ListDisputes(r *httprequest.Request) {
	mid, ok := readScope(r)
	if !ok {
		return
	}
	var query billing.DisputeListParams
	if !bindList(r, &query, &query.PageRequest) {
		return
	}
	params := gen.ListDisputesParams{
		MerchantID: mid, Kinds: texts(query.Kind), Statuses: texts(query.Status), Reasons: texts(query.Reason), Networks: texts(query.Network),
		PspID: idOf(query.PSPID), CustomerID: idOf(query.CustomerID), PaymentID: idOf(query.PaymentID),
		Since: instant(query.Since), Until: instant(query.Until),
	}
	limit, ok := keyset(r, query.PageRequest, &params.AfterAt, &params.AfterID)
	if !ok {
		return
	}
	params.RowLimit = pagination.Fetch(limit)
	rows, err := r.State.DB.Gen(r.Request.Context()).ListDisputes(r.Request.Context(), params)
	if err != nil {
		r.InternalError("disputes could not be listed", err)
		return
	}
	page := pagination.Cut(rows, limit, func(d gen.BillingDispute) any { return pagination.TimeID{At: d.DisputedAt, ID: d.ID} })
	r.SuccessJSON(pagination.Map(page, disputeToAPI))
}

// GetDispute reads one dispute.
//
//	GET /admin/disputes/{id}
func GetDispute(r *httprequest.Request) {
	mid, ok := readScope(r)
	if !ok {
		return
	}
	id, err := billing.ParseDisputeID(r.Param("id"))
	if err != nil || id.IsZero() {
		r.APIError(api.Coded(billing.CodeInvalidParam, "invalid dispute id").WithParam("id"))
		return
	}
	row, err := r.State.DB.Gen(r.Request.Context()).GetDispute(r.Request.Context(), gen.GetDisputeParams{MerchantID: mid, ID: id.UUID()})
	if errors.Is(err, pgx.ErrNoRows) {
		r.ErrorCode(billing.CodeResourceNotFound, "dispute not found")
		return
	}
	if err != nil {
		r.InternalError("dispute could not be read", err)
		return
	}
	r.JSON(http.StatusOK, disputeToAPI(row))
}

// ListDisputePrograms is each card PSP's standing in the dispute monitoring
// programs for a month.
//
//	GET /metrics/dispute-programs?at&psp_id
func ListDisputePrograms(r *httprequest.Request) {
	svc := r.State.Disputes
	if svc == nil {
		r.ErrorCode(billing.CodeServiceUnavailable, "dispute monitoring is not configured")
		return
	}
	var query billing.DisputeProgramListParams
	if !r.BindQuery(&query) {
		return
	}
	standings, err := svc.ListPrograms(r.Request.Context(), query)
	if err != nil {
		r.InternalError("dispute programs could not be evaluated", err)
		return
	}
	if standings == nil {
		standings = []billing.DisputeStanding{}
	}
	r.SuccessJSON(billing.ListPage[billing.DisputeStanding]{Items: standings})
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

func texts[T ~string](values []T) []string {
	var out []string
	for _, v := range values {
		if v != "" {
			out = append(out, string(v))
		}
	}
	return out
}

func disputeToAPI(d gen.BillingDispute) billing.Dispute {
	out := billing.Dispute{
		ID: billing.DisputeID(d.ID), ProviderRef: d.ProviderRef, Rail: billing.Rail(d.Rail), PSPID: billing.PSPID(d.PspID),
		Kind: billing.DisputeKind(d.Kind), Status: billing.DisputeStatus(d.Status), Network: billing.CardNetwork(d.Network),
		Reason: billing.DisputeReason(d.Reason), Amount: d.Amount, DisputedAt: d.DisputedAt.UTC(),
		ChargebackAt: utcPtr(d.ChargebackAt), EvidenceDueAt: utcPtr(d.EvidenceDueAt), ClosedAt: utcPtr(d.ClosedAt),
	}
	if d.ReasonCode != nil {
		out.ReasonCode = *d.ReasonCode
	}
	if d.ProviderReason != nil {
		out.ProviderReason = *d.ProviderReason
	}
	if d.Currency != nil {
		currency := catalog.Currency(*d.Currency)
		out.Currency = &currency
	}
	if d.CustomerID != nil {
		customer := billing.CustomerID(*d.CustomerID)
		out.CustomerID = &customer
	}
	if d.PaymentID != nil {
		payment := billing.PaymentID(*d.PaymentID)
		out.PaymentID = &payment
	}
	if d.ReversalPaymentID != nil {
		payment := billing.PaymentID(*d.ReversalPaymentID)
		out.ReversalPaymentID = &payment
	}
	if d.RecoveryPaymentID != nil {
		payment := billing.PaymentID(*d.RecoveryPaymentID)
		out.RecoveryPaymentID = &payment
	}
	return out
}
