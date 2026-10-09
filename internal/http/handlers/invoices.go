package handlers

import (
	"errors"
	"net/http"
	"net/mail"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/providerrecovery"
	billingservice "github.com/open-rails/openrails/internal/service"
	log "github.com/sirupsen/logrus"
)

// ListInvoices (GET /merchant/invoices) is one page of the merchant's
// invoices, newest period first.
func ListInvoices(gate StaffCan) func(*httprequest.Request) {
	return func(r *httprequest.Request) {
		page, ok := r.Page()
		if !ok {
			return
		}
		params := billing.InvoiceListParams{PageRequest: page}
		if raw := strings.TrimSpace(r.Query("customer_id")); raw != "" {
			id, err := billing.ParseCustomerID(raw)
			if err != nil || id.IsZero() {
				r.APIError(api.Coded(billing.CodeInvalidQuery, "customer_id is invalid").WithParam("customer_id"))
				return
			}
			params.CustomerID = id
		}
		if raw := strings.TrimSpace(r.Query("currency")); raw != "" {
			if err := money.RequireBillingCurrency(raw); err != nil {
				r.APIError(api.Coded(billing.CodeInvalidQuery, "currency is invalid").WithParam("currency"))
				return
			}
			params.Currency = raw
		}
		if raw := billing.InvoiceStatus(strings.TrimSpace(r.Query("status"))); raw != "" {
			if !slices.Contains(billing.InvoiceStatuses(), raw) {
				r.APIError(api.Coded(billing.CodeInvalidQuery, "status is invalid").WithParam("status"))
				return
			}
			params.Status = raw
		}
		for name, target := range map[string]**time.Time{"period_starts_after": &params.PeriodStartsAfter, "period_starts_before": &params.PeriodStartsBefore} {
			if raw := r.Query(name); raw != "" {
				parsed, err := time.Parse(time.RFC3339, raw)
				if err != nil {
					r.APIError(api.Coded(billing.CodeInvalidQuery, name+" must be RFC 3339").WithParam(name))
					return
				}
				*target = &parsed
			}
		}
		svc, ok := newAdminBillingService(r)
		if !ok {
			return
		}
		out, err := svc.ListInvoices(r.Request.Context(), params)
		if err != nil {
			writeInvoiceError(r, err)
			return
		}
		update, collect := invoicePermission(r, gate, billing.MerchantInvoicesUpdate), invoicePermission(r, gate, billing.MerchantInvoicesCollect)
		for i := range out.Items {
			permittedInvoiceActions(&out.Items[i], update, collect)
		}
		r.SuccessJSON(out)
	}
}

// GetInvoice (GET /merchant/invoices/{id}) reads one of the merchant's
// invoices.
func GetInvoice(gate StaffCan) func(*httprequest.Request) {
	return func(r *httprequest.Request) {
		_, invoice, ok := loadMerchantInvoice(r)
		if !ok {
			return
		}
		permittedInvoiceActions(invoice, invoicePermission(r, gate, billing.MerchantInvoicesUpdate), invoicePermission(r, gate, billing.MerchantInvoicesCollect))
		r.SuccessJSON(invoice)
	}
}

// ListInvoicePayments (GET /merchant/invoices/{id}/payments) is one page of an
// invoice's payments, newest first.
func ListInvoicePayments(r *httprequest.Request) {
	svc, invoice, ok := loadMerchantInvoice(r)
	if !ok {
		return
	}
	page, ok := r.Page()
	if !ok {
		return
	}
	out, err := svc.ListInvoicePayments(r.Request.Context(), identity.CustomerID(invoice.CustomerID), invoice.ID.UUID(), page)
	if err != nil {
		writeInvoiceError(r, err)
		return
	}
	r.SuccessJSON(out)
}

// VoidInvoice, MarkInvoiceUncollectible and CreateInvoicePayment change an
// invoice and answer it.
func VoidInvoice(r *httprequest.Request) { applyInvoiceAction(r, billing.InvoiceActionVoid) }
func MarkInvoiceUncollectible(r *httprequest.Request) {
	applyInvoiceAction(r, billing.InvoiceActionUncollectible)
}
func CreateInvoicePayment(r *httprequest.Request) {
	applyInvoiceAction(r, billing.InvoiceActionRecordPayment)
}

func applyInvoiceAction(r *httprequest.Request, action billing.InvoiceAction) {
	var body billing.CreateInvoicePaymentParams
	if action == billing.InvoiceActionRecordPayment {
		if !r.BindJSON(&body) {
			return
		}
		body.Reference = strings.TrimSpace(body.Reference)
		if body.Amount <= 0 || body.Reference == "" || len(body.Reference) > 255 {
			r.ErrorCode(billing.CodeInvoicePaymentInvalid, "a positive amount and a reference of 1-255 bytes are required")
			return
		}
	}
	svc, invoice, ok := loadMerchantInvoice(r)
	if !ok {
		return
	}
	out, err := svc.ApplyInvoiceAction(r.Request.Context(), identity.CustomerID(invoice.CustomerID), invoice.ID.UUID(), money.InvoiceAdminMutation{Action: action, Amount: body.Amount, Reference: body.Reference})
	if err != nil {
		writeInvoiceError(r, err)
		return
	}
	r.SuccessJSON(out)
}

// RetryInvoiceCollection (POST /merchant/invoices/{id}/retry-collection)
// charges an open invoice to one of its customer's cards: 200 when the charge
// settled or failed, 202 while it is unresolved.
func RetryInvoiceCollection(r *httprequest.Request) {
	var body billing.RetryInvoiceCollectionParams
	if !r.BindJSON(&body) {
		return
	}
	key, ok := paymentActionKey(r)
	if !ok {
		return
	}
	if body.PaymentMethodID.IsZero() {
		writePaymentMethodRequired(r)
		return
	}
	body.IdempotencyKey = key
	svc, invoice, ok := loadMerchantInvoice(r)
	if !ok {
		return
	}
	// The durable claim owns eligibility: a replay is answered even after the
	// invoice is paid.
	out, err := svc.RetryInvoiceCollection(r.Request.Context(), identity.CustomerID(invoice.CustomerID), invoice.ID.UUID(), body)
	if err != nil {
		writeInvoiceError(r, err)
		return
	}
	status := http.StatusOK
	if out.Payment.Status == billing.InvoicePaymentAttempted {
		status = http.StatusAccepted
	}
	r.JSON(status, out)
}

// ListMyInvoices (GET /me/invoices) is one page of the caller's invoices,
// newest period first.
func ListMyInvoices(r *httprequest.Request) {
	payer, ok := selfAccountPayer(r)
	if !ok {
		return
	}
	page, ok := r.Page()
	if !ok {
		return
	}
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	out, err := svc.ListInvoices(r.Request.Context(), billing.InvoiceListParams{CustomerID: billing.CustomerID(payer), PageRequest: page})
	if err != nil {
		writeInvoiceError(r, err)
		return
	}
	for i := range out.Items {
		out.Items[i].AvailableActions = []billing.InvoiceAction{}
	}
	r.SuccessJSON(out)
}

// GetMyInvoice (GET /me/invoices/{id}) reads one of the caller's invoices.
func GetMyInvoice(r *httprequest.Request) {
	payer, ok := selfAccountPayer(r)
	if !ok {
		return
	}
	id, ok := invoiceIDParam(r)
	if !ok {
		return
	}
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	out, err := svc.GetInvoice(r.Request.Context(), payer, id)
	if err != nil {
		writeInvoiceError(r, err)
		return
	}
	out.AvailableActions = []billing.InvoiceAction{}
	r.SuccessJSON(out)
}

// PayMyInvoice (POST /me/invoices/{id}/pay-now) is the customer paying an
// invoice now: 200 when the charge resolved, 202 while it is unresolved.
func PayMyInvoice(r *httprequest.Request) {
	payer, ok := customerActionPayer(r)
	if !ok {
		return
	}
	key, ok := paymentActionKey(r)
	if !ok {
		return
	}
	id, ok := invoiceIDParam(r)
	if !ok {
		return
	}
	var body billing.PayInvoiceParams
	if !r.BindJSON(&body) {
		return
	}
	if body.PaymentMethodID.IsZero() {
		writePaymentMethodRequired(r)
		return
	}
	body.IdempotencyKey = key
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	out, err := svc.PayInvoice(r.Request.Context(), payer, id, body)
	if err != nil {
		customerPaymentError(r, err)
		return
	}
	out.Invoice.AvailableActions = []billing.InvoiceAction{}
	status := http.StatusOK
	if out.Operation.Unresolved() {
		status = http.StatusAccepted
	}
	r.JSON(status, out)
}

// GetInvoiceProfile (GET /merchant/customers/{customer_id}/invoice-profile)
// reads a customer's invoice profile; 404 when none is set.
func GetInvoiceProfile(r *httprequest.Request) {
	payer, ok := invoiceProfileCustomer(r)
	if !ok {
		return
	}
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	profile, err := svc.GetInvoiceProfile(r.Request.Context(), payer)
	if err != nil {
		writeInvoiceError(r, err)
		return
	}
	if profile == nil {
		r.ErrorCode(billing.CodeResourceNotFound, "the customer has no invoice profile")
		return
	}
	r.SuccessJSON(profile)
}

// SetInvoiceProfile (PUT /merchant/customers/{customer_id}/invoice-profile)
// replaces a customer's invoice profile; with If-None-Match: * it only
// creates one (201), answering an existing profile unchanged (200).
func SetInvoiceProfile(r *httprequest.Request) {
	payer, ok := invoiceProfileCustomer(r)
	if !ok {
		return
	}
	var body billing.InvoiceProfile
	if !r.BindJSON(&body) {
		return
	}
	if body.NetTermsDays < 0 || int64(body.NetTermsDays) > money.MaxInvoiceNetTermsDays {
		r.APIError(api.Coded(billing.CodeInvalidParam, "net_terms_days is out of range").WithParam("net_terms_days"))
		return
	}
	if body.CollectionMethod != billing.CollectChargeAutomatically && body.CollectionMethod != billing.CollectSendInvoice {
		r.APIError(api.Coded(billing.CodeInvalidParam, "collection_method is invalid").WithParam("collection_method"))
		return
	}
	for i := range body.BillingContacts {
		body.BillingContacts[i].Email = strings.TrimSpace(body.BillingContacts[i].Email)
		address, err := mail.ParseAddress(body.BillingContacts[i].Email)
		if err != nil || address.Address != body.BillingContacts[i].Email {
			r.APIError(api.Coded(billing.CodeInvalidParam, "billing contacts need valid email addresses").WithParam("billing_contacts"))
			return
		}
	}
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	profile, created, err := svc.SetInvoiceProfile(r.Request.Context(), payer, billing.SetInvoiceProfileParams{InvoiceProfile: body, IfAbsent: r.Header("If-None-Match") == "*"})
	if err != nil {
		writeInvoiceError(r, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	r.JSON(status, profile)
}

func invoiceIDParam(r *httprequest.Request) (uuid.UUID, bool) {
	id, err := billing.ParseInvoiceID(r.Param("id"))
	if err != nil || id.IsZero() {
		r.APIError(api.Coded(billing.CodeInvalidParam, "invalid invoice id").WithParam("id"))
		return uuid.Nil, false
	}
	return id.UUID(), true
}

func loadMerchantInvoice(r *httprequest.Request) (*billingservice.Service, *billing.Invoice, bool) {
	id, ok := invoiceIDParam(r)
	if !ok {
		return nil, nil, false
	}
	svc, ok := newAdminBillingService(r)
	if !ok {
		return nil, nil, false
	}
	invoice, err := svc.GetInvoice(r.Request.Context(), identity.CustomerID{}, id)
	if err != nil {
		writeInvoiceError(r, err)
		return nil, nil, false
	}
	return svc, invoice, true
}

func invoicePermission(r *httprequest.Request, gate StaffCan, permission string) bool {
	return gate != nil && gate(r.Request, permission) == nil
}

// permittedInvoiceActions keeps the actions the caller holds the permission
// for.
func permittedInvoiceActions(invoice *billing.Invoice, update, collect bool) {
	invoice.AvailableActions = slices.DeleteFunc(invoice.AvailableActions, func(action billing.InvoiceAction) bool {
		if action == billing.InvoiceActionRetryCollection {
			return !collect
		}
		return !update
	})
}

func invoiceProfileCustomer(r *httprequest.Request) (identity.CustomerID, bool) {
	if r.State == nil || r.State.DB == nil {
		r.ErrorCode(billing.CodeServiceUnavailable, "invoice profile unavailable")
		return identity.CustomerID{}, false
	}
	id, err := billing.ParseCustomerID(r.Param("customer_id"))
	if err != nil || id.IsZero() {
		r.APIError(api.Coded(billing.CodeInvalidParam, "invalid customer_id").WithParam("customer_id"))
		return identity.CustomerID{}, false
	}
	mid, err := merchant.Require(r.Request.Context())
	if err != nil {
		writeInvoiceError(r, err)
		return identity.CustomerID{}, false
	}
	exists, err := r.State.DB.Gen(r.Request.Context()).InvoiceProfileCustomerExists(r.Request.Context(), gen.InvoiceProfileCustomerExistsParams{MerchantID: mid.UUID(), CustomerID: id.UUID()})
	if err != nil {
		writeInvoiceError(r, err)
		return identity.CustomerID{}, false
	}
	if !exists {
		r.ErrorCode(billing.CodeResourceNotFound, "customer not found")
		return identity.CustomerID{}, false
	}
	return identity.CustomerID(id.UUID()), true
}

var invoiceRefusals = []struct {
	err  error
	code string
}{
	{money.ErrInvoiceActionNotAllowed, billing.CodeInvoiceActionNotAllowed},
	{money.ErrInvoiceNotRetryable, billing.CodeInvoiceNotRetryable},
	{money.ErrInvoiceRetryInProgress, billing.CodeInvoiceRetryInProgress},
	{money.ErrInvoiceRetryOutcomeUnknown, billing.CodeInvoiceRetryOutcomeUnknown},
	{money.ErrInvoiceRetryIdempotencyConflict, billing.CodeInvoiceRetryIdempotencyConflict},
	{money.ErrInvoicePaymentReferenceUsed, billing.CodeInvoicePaymentReferenceUsed},
	{money.ErrInvoicePaymentExceedsDue, billing.CodeInvoicePaymentExceedsDue},
	{money.ErrInvoicePaymentInvalid, billing.CodeInvoicePaymentInvalid},
	{money.ErrCollectionPaymentMethodInvalid, billing.CodeCollectionPaymentMethodInvalid},
	{money.ErrCollectionPaymentMethodRequired, billing.CodeCollectionPaymentMethodRequired},
}

func writeInvoiceError(r *httprequest.Request, err error) {
	if errors.Is(err, providerrecovery.ErrPending) || errors.Is(err, money.ErrInvoiceRecoveryHeld) {
		r.ErrorCode(billing.CodeServiceUnavailable, "billing is paused while provider recovery completes")
		return
	}
	if db.IsNotFound(err) {
		r.ErrorCode(billing.CodeResourceNotFound, "invoice not found")
		return
	}
	for _, refusal := range invoiceRefusals {
		if errors.Is(err, refusal.err) {
			r.ErrorCode(refusal.code, err.Error())
			return
		}
	}
	var coded interface{ ErrorCode() string }
	if errors.As(err, &coded) {
		writeRefusal(r, err, "invoice operation failed")
		return
	}
	log.WithError(err).Warn("invoice operation failed")
	r.InternalError("invoice operation failed", err)
}
