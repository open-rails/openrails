package handlers

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/messages"
	"github.com/open-rails/openrails/internal/pagination"
)

// ListCustomerMessages handles GET /v1/admin/customers/{customer_id}/messages:
// the customer's message log, newest first.
func ListCustomerMessages(r *httprequest.Request) {
	customer, ok := commerceCustomer(r, customerIDParam(r.Param("customer_id")))
	if !ok {
		return
	}
	var query billing.CustomerMessageListParams
	if !bindList(r, &query, &query.PageRequest) {
		return
	}
	ctx := r.Request.Context()
	mid, err := merchant.Require(ctx)
	if err != nil {
		r.InternalError("customer messages unavailable", err)
		return
	}
	params := gen.ListCustomerMessagesParams{MerchantID: mid.UUID(), CustomerID: customer.UUID()}
	limit, ok := keyset(r, query.PageRequest, &params.AfterAt, &params.AfterID)
	if !ok {
		return
	}
	params.RowLimit = pagination.Fetch(limit)
	rows, err := r.State.DB.Gen(ctx).ListCustomerMessages(ctx, params)
	if err != nil {
		r.InternalError("customer messages could not be listed", err)
		return
	}
	page := pagination.Cut(rows, limit, func(m gen.BillingCustomerMessage) any { return pagination.TimeID{At: m.CreatedAt, ID: m.ID} })
	r.SuccessJSON(pagination.Map(page, customerMessageToAPI))
}

func customerMessageToAPI(m gen.BillingCustomerMessage) billing.CustomerMessage {
	out := billing.CustomerMessage{
		ID: billing.CustomerMessageID(m.ID), CustomerID: billing.CustomerID(m.CustomerID), EventID: billing.NotificationID(m.EventID),
		EventType: m.EventType, Channel: billing.MessageChannel(m.Channel), Status: billing.MessageStatus(m.Status), Attempts: int(m.Attempts),
		Recipient: m.Recipient, CreatedAt: m.CreatedAt.UTC(), UpdatedAt: m.UpdatedAt.UTC(),
	}
	if m.LastErrorCode != nil {
		code := billing.MessageError(*m.LastErrorCode)
		out.LastError = &code
	}
	if m.SentAt != nil {
		at := m.SentAt.UTC()
		out.SentAt = &at
	}
	return out
}

func messageService(r *httprequest.Request) (*messages.Service, bool) {
	if r.State == nil || r.State.Messages == nil {
		r.ErrorCode(billing.CodeServiceUnavailable, "customer messages are not configured")
		return nil, false
	}
	return r.State.Messages, true
}

// PreviewMessage handles POST /v1/merchant/messages/preview.
func PreviewMessage(r *httprequest.Request) {
	svc, ok := messageService(r)
	if !ok {
		return
	}
	var in billing.PreviewMessageParams
	if !r.BindJSON(&in) {
		return
	}
	out, err := svc.Preview(r.Request.Context(), in.EventType, in.Language, in.Template)
	if err != nil {
		writeRefusal(r, err, "message preview failed")
		return
	}
	r.SuccessJSON(out)
}

// CreateTestMessage handles POST /v1/merchant/messages/test.
func CreateTestMessage(r *httprequest.Request) {
	svc, ok := messageService(r)
	if !ok {
		return
	}
	var in billing.CreateTestMessageParams
	if !r.BindJSON(&in) {
		return
	}
	out, err := svc.Test(r.Request.Context(), in.EventType, in.Language, in.Template)
	if err != nil {
		writeRefusal(r, err, "test message failed")
		return
	}
	r.SuccessJSON(out)
}
