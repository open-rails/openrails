package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/authkit/auth"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	"github.com/open-rails/openrails/internal/billingauth"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/cardguard"
	"github.com/open-rails/openrails/internal/http/middleware"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/invoices"
	"github.com/open-rails/openrails/internal/modules/payment"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/paymentpage"
	billingservice "github.com/open-rails/openrails/internal/service"
)

// Invoices on /v1/me: the customer's own purchases. Paying, and every read of a
// payment's next action, is the customer in person.

// invoiceActor is the request's customer, nil after answering a refusal.
// present demands the customer in person, as paying does.
func invoiceActor(r *httprequest.Request, present bool) *billingservice.InvoiceActor {
	if present && !customerInitiatedChargeAllowed(r) {
		return nil
	}
	scope, ok := r.CustomerScope()
	if !ok {
		r.ErrorCode(billing.CodeAuthenticationRequired, "")
		return nil
	}
	ip := r.ClientIP()
	if present && ip != "" {
		addr, err := netip.ParseAddr(ip)
		if err != nil || addr.Zone() != "" || addr.IsUnspecified() {
			r.APIError(api.Coded(billing.CodeInvalidParam, "the forwarded client address is not an IP address"))
			return nil
		}
		ip = addr.Unmap().String()
	}
	actor := &billingservice.InvoiceActor{Customer: scope.Customer(), Principal: verifiedPrincipal(r), ClientIP: ip, Present: scope.Present()}
	if actor.Present {
		actor.Country, actor.Language = r.ClientCountry(), r.ClientLanguage()
	}
	return actor
}

// signedInPerson refuses, 403 customer_session_required, anyone but the
// merchant's own person signed in: a user of the issuer the merchant
// declares, acting itself in a session or with a device key, its credential
// bound to no scope. No application, invoker, API key or signed token.
func signedInPerson(r *httprequest.Request) bool {
	ctx := r.Request.Context()
	scope, scoped := r.CustomerScope()
	c, identified := billingauth.IdentityFromContext(ctx)
	v, verified := billingauth.VerifiedFrom(ctx)
	if !scoped || !identified || !verified {
		r.ErrorCode(billing.CodeAuthenticationRequired, "")
		return false
	}
	own, declared := r.State.IssuerMerchant(c.Issuer)
	bound, isBound := v.(auth.Bound)
	if !scope.Present() || c.Credential.Kind != billingauth.CredentialSession && c.Credential.Kind != billingauth.CredentialDeviceKey ||
		isBound && bound.BoundScope() != (billingauth.Scope{}) || !declared || own != scope.Merchant() {
		r.ErrorCode(billing.CodeCustomerSessionRequired, "")
		return false
	}
	return true
}

// invoiceKey is the request's Idempotency-Key, "" after answering a refusal.
func invoiceKey(r *httprequest.Request) string {
	key := strings.TrimSpace(r.Request.Header.Get("Idempotency-Key"))
	switch {
	case key == "":
		r.ErrorCode(billing.CodeIdempotencyKeyRequired, "")
	case len(key) > 255:
		r.APIError(api.Coded(billing.CodeInvalidParam, "Idempotency-Key is 1 to 255 bytes").WithParam("Idempotency-Key"))
	case cardguard.ContainsPAN(key):
		r.APIError(api.Coded(billing.CodeInvalidParam, "Idempotency-Key must not contain card data").WithParam("Idempotency-Key"))
	default:
		return key
	}
	return ""
}

func invoiceService(r *httprequest.Request) *billingservice.Service {
	svc, err := billingservice.New(r.Log, r.State)
	if err != nil {
		r.InternalError("billing service unavailable", err)
		return nil
	}
	return svc
}

func invoiceID(r *httprequest.Request) (billing.InvoiceID, bool) {
	id, err := billing.ParseInvoiceID(r.Param("id"))
	if err != nil || id.IsZero() {
		r.APIError(api.Coded(billing.CodeResourceNotFound, "invoice not found"))
		return billing.InvoiceID{}, false
	}
	return id, true
}

// answerInvoice writes an invoice request's answer: the invoice (201 created, 200
// otherwise), or, when its payment failed, 402 card_error carrying the invoice,
// as Stripe's carries its PaymentIntent. A replay says so.
func answerInvoice(r *httprequest.Request, answer billingservice.InvoiceAnswer) {
	// The blocking ledger updates after inline execution. Refresh its merchant
	// attack challenge here without counting the payment's decline again.
	if answer.Code == billing.CodeCardDeclined && !answer.Replayed {
		ctx := r.Request.Context()
		mid, _ := merchant.FromContext(ctx)
		r.State.CardAbuseGuard.RecordChargeFailure(ctx, mid.UUID(), nil, r.State.CardFailureLedger.AttackMode(ctx, mid.UUID()))
	}
	r.SetHeader("Cache-Control", "no-store")
	if answer.Replayed {
		r.SetHeader("Idempotent-Replayed", "true")
	}
	if answer.Status != http.StatusPaymentRequired {
		r.JSON(answer.Status, answer.Invoice)
		return
	}
	out := api.Coded(answer.Code, "")
	if failure := answer.Invoice.Payment.LastPaymentError; failure != nil {
		out.Message = failure.Message
		out.Metadata = map[string]any{"failure": failure}
	}
	out.Invoice = answer.Invoice
	r.APIError(out)
}

// writeInvoice writes an invoice read or changed without a payment.
func writeInvoice(r *httprequest.Request, invoice *billing.Invoice) {
	r.SetHeader("Cache-Control", "no-store")
	r.JSON(http.StatusOK, invoice)
}

// PreviewMyInvoice handles POST /v1/me/invoices/preview.
func PreviewMyInvoice(r *httprequest.Request) {
	var body billing.PreviewMyInvoiceParams
	if !r.BindJSON(&body) {
		return
	}
	actor := invoiceActor(r, false)
	svc := invoiceService(r)
	if actor == nil || svc == nil {
		return
	}
	preview, err := svc.PreviewInvoice(r.Request.Context(), *actor, body)
	if err != nil {
		writeInvoiceError(r, err)
		return
	}
	r.SuccessJSON(preview)
}

// CreateMyInvoice handles POST /v1/me/invoices: the one-call buy when a payment
// is named, the invoice and its embedded pay URL when a pay URL is, an open
// invoice to pay later otherwise. Asked again for the same lines, it answers
// the customer's open invoice of them. A pay URL offers the customer's saved
// cards, so only the merchant's own signed-in person asks for one.
func CreateMyInvoice(r *httprequest.Request) {
	var body billing.CreateMyInvoiceParams
	if !r.BindJSON(&body) || refuseCardNumber(r, body) {
		return
	}
	if body.PayURL != nil && !signedInPerson(r) {
		return
	}
	actor := invoiceActor(r, body.Payment != nil)
	if actor == nil {
		return
	}
	// A one-call buy's key names its payment attempt, and an invoice in a
	// merchant's unit, paid as it is created, keeps it; otherwise the invoice
	// is the customer's open invoice of the same lines.
	var key string
	if body.Payment != nil || strings.TrimSpace(r.Request.Header.Get("Idempotency-Key")) != "" {
		if key = invoiceKey(r); key == "" {
			return
		}
	}
	svc := invoiceService(r)
	if svc == nil {
		return
	}
	ctx := intents.WithInvoiceCaptchaSubjects(r.Request.Context(), middleware.SubjectKeysFromContext(r.Request.Context()))
	answer, err := svc.CreateInvoice(ctx, *actor, body, key)
	if err != nil {
		writeInvoiceError(r, err)
		return
	}
	answerInvoice(r, answer)
}

// PayMyInvoice handles POST /v1/me/invoices/{id}/pay.
func PayMyInvoice(r *httprequest.Request) {
	var body billing.PayInvoiceParams
	if !r.BindJSON(&body) || refuseCardNumber(r, body) {
		return
	}
	if !payURLPayment(r, body) {
		return
	}
	actor := invoiceActor(r, true)
	if actor == nil {
		return
	}
	id, ok := invoiceID(r)
	if !ok {
		return
	}
	key := invoiceKey(r)
	svc := invoiceService(r)
	if key == "" || svc == nil {
		return
	}
	ctx := intents.WithInvoiceCaptchaSubjects(r.Request.Context(), middleware.SubjectKeysFromContext(r.Request.Context()))
	answer, err := svc.PayInvoice(ctx, *actor, id, body, key)
	if err != nil {
		writeInvoiceError(r, err)
		return
	}
	answerInvoice(r, answer)
}

// ConfirmMyInvoice handles POST /v1/me/invoices/{id}/confirm: a nudge after the
// customer completed next_action; OpenRails reads the provider.
func ConfirmMyInvoice(r *httprequest.Request) {
	actor := invoiceActor(r, true)
	if actor == nil {
		return
	}
	id, ok := invoiceID(r)
	svc := invoiceService(r)
	if !ok || svc == nil {
		return
	}
	var body billing.ConfirmInvoiceParams
	if !r.BindOptionalJSON(&body) {
		return
	}
	answer, err := svc.ConfirmInvoice(r.Request.Context(), *actor, id, body)
	if err != nil {
		writeInvoiceError(r, err)
		return
	}
	answerInvoice(r, answer)
}

// CancelMyInvoice handles POST /v1/me/invoices/{id}/cancel.
func CancelMyInvoice(r *httprequest.Request) {
	actor := invoiceActor(r, false)
	if actor == nil {
		return
	}
	id, ok := invoiceID(r)
	svc := invoiceService(r)
	if !ok || svc == nil {
		return
	}
	invoice, err := svc.CancelInvoice(r.Request.Context(), *actor, id)
	if err != nil {
		writeInvoiceError(r, err)
		return
	}
	writeInvoice(r, invoice)
}

// ReportMyPayURLFailure handles POST /v1/me/invoices/{id}/pay-url-failure: the
// customer's page could not frame the invoice's pay URL.
func ReportMyPayURLFailure(r *httprequest.Request) {
	actor := invoiceActor(r, false)
	if actor == nil {
		return
	}
	id, ok := invoiceID(r)
	svc := invoiceService(r)
	if !ok || svc == nil {
		return
	}
	if err := svc.ReportPayURLFailure(r.Request.Context(), *actor, id); err != nil {
		writeInvoiceError(r, err)
		return
	}
	r.NoContent()
}

// GetMyInvoice handles GET /v1/me/invoices/{id}: the resumable handle.
func GetMyInvoice(r *httprequest.Request) {
	actor := invoiceActor(r, false)
	if actor == nil {
		return
	}
	id, ok := invoiceID(r)
	svc := invoiceService(r)
	if !ok || svc == nil {
		return
	}
	invoice, err := svc.GetInvoice(r.Request.Context(), *actor, id)
	if err != nil {
		writeInvoiceError(r, err)
		return
	}
	writeInvoice(r, invoice)
}

// ListMyInvoices handles GET /v1/me/invoices.
func ListMyInvoices(r *httprequest.Request) {
	actor := invoiceActor(r, false)
	if actor == nil {
		return
	}
	params, ok := invoiceListQuery(r, false)
	if !ok {
		return
	}
	svc := invoiceService(r)
	if svc == nil {
		return
	}
	out, err := svc.ListInvoices(r.Request.Context(), *actor, params)
	if err != nil {
		writeInvoiceError(r, err)
		return
	}
	r.SetHeader("Cache-Control", "no-store")
	r.SuccessJSON(out)
}

// ListInvoices handles GET /v1/admin/invoices.
func ListInvoices(r *httprequest.Request) {
	params, ok := invoiceListQuery(r, true)
	if !ok {
		return
	}
	svc := invoiceService(r)
	if svc == nil {
		return
	}
	out, err := svc.ListMerchantInvoices(r.Request.Context(), params)
	if err != nil {
		writeInvoiceError(r, err)
		return
	}
	customers := make([]billing.CustomerID, len(out.Items))
	for i, invoice := range out.Items {
		customers[i] = invoice.CustomerID
	}
	contacts := customerContacts(r, customers)
	for i := range out.Items {
		out.Items[i].CustomerContact = contacts[out.Items[i].CustomerID]
	}
	r.SetHeader("Cache-Control", "no-store")
	r.SuccessJSON(out)
}

// GetInvoice handles GET /v1/admin/invoices/{id}.
func GetInvoice(r *httprequest.Request) {
	id, ok := invoiceID(r)
	svc := invoiceService(r)
	if !ok || svc == nil {
		return
	}
	invoice, err := svc.GetMerchantInvoice(r.Request.Context(), id)
	if err != nil {
		writeInvoiceError(r, err)
		return
	}
	writeInvoice(r, invoice)
}

// writeInvoiceError answers an invoice refusal by its code.
func writeInvoiceError(r *httprequest.Request, err error) {
	var line *invoices.LineError
	if errors.As(err, &line) {
		out := api.Coded(line.Code(), line.Refusal.Message).WithParam(lineParam(line.Index))
		out.Metadata = map[string]any{"code": line.Refusal.Code}
		if line.Refusal.OwnedBy != nil {
			out.Metadata["owned_by"] = *line.Refusal.OwnedBy
		}
		if line.Refusal.Hint != nil {
			out.Metadata["hint"] = *line.Refusal.Hint
		}
		if len(line.Refusal.Missing) > 0 {
			out.Metadata["missing"] = line.Refusal.Missing
		}
		r.APIError(out)
		return
	}
	var blocked *payment.CardAttemptsBlockedError
	var refused *paymentmethods.PaymentMethodError
	switch {
	case errors.As(err, &blocked):
		writeCardAttemptsBlocked(r, blocked.RetryAfter)
	case errors.As(err, &refused):
		writePaymentMethodError(r, refused)
	case errors.Is(err, pgx.ErrNoRows):
		r.ErrorCode(billing.CodeResourceNotFound, "")
	case errors.Is(err, payment.ErrPaymentMethodStale):
		writePaymentMethodStale(r)
	case errors.Is(err, billing.ErrIdempotencyKeyReused):
		r.ErrorCode(billing.CodeIdempotencyKeyReused, "")
	case errors.Is(err, payment.ErrInvoiceAttemptValidation):
		r.APIError(api.Coded(billing.CodeInvalidParam, err.Error()))
	default:
		writeRefusal(r, err, "invoice request failed")
	}
}

func lineParam(i int) string { return "lines[" + strconv.Itoa(i) + "]" }

// PreviewMerchantInvoice handles POST /v1/app/invoices/preview: lines priced for
// one customer.
func PreviewMerchantInvoice(r *httprequest.Request) {
	var body billing.PreviewInvoiceParams
	if !r.BindJSON(&body) {
		return
	}
	svc := invoiceService(r)
	if svc == nil {
		return
	}
	preview, err := svc.PreviewMerchantInvoice(r.Request.Context(), body)
	if err != nil {
		writeInvoiceError(r, err)
		return
	}
	r.SuccessJSON(preview)
}

// CreateMerchantInvoice handles POST /v1/app/invoices: an unpaid invoice for one
// customer, which they pay on the merchant's site.
func CreateMerchantInvoice(r *httprequest.Request) {
	var body billing.CreateInvoiceParams
	if !r.BindJSON(&body) {
		return
	}
	svc := invoiceService(r)
	if svc == nil {
		return
	}
	invoice, replayed, err := svc.CreateMerchantInvoice(r.Request.Context(), body, r.Header("Idempotency-Key"))
	if err != nil {
		writeInvoiceError(r, err)
		return
	}
	status := http.StatusCreated
	if replayed {
		status = http.StatusOK
	}
	r.SetHeader("Cache-Control", "no-store")
	r.JSON(status, invoice)
}

// CancelMerchantInvoice handles POST /v1/app/invoices/{id}/cancel: withdraws an
// open invoice the merchant created.
func CancelMerchantInvoice(r *httprequest.Request) {
	id, ok := invoiceID(r)
	svc := invoiceService(r)
	if !ok || svc == nil {
		return
	}
	invoice, err := svc.CancelMerchantInvoice(r.Request.Context(), id)
	if err != nil {
		writeInvoiceError(r, err)
		return
	}
	writeInvoice(r, invoice)
}

// payURLPayment refuses on the checkout origin what its secret does
// not open: a saved card the pay URL does not offer (the merchant's opt-in,
// and cards the customer allowed to be shown again), or another attempt once
// the invoice took its most declines.
func payURLPayment(r *httprequest.Request, body billing.PayInvoiceParams) bool {
	c, ok := paymentpage.FromContext(r.Request.Context())
	if !ok {
		return true
	}
	ctx := r.Request.Context()
	if pm := body.Payment.PaymentMethodID; pm != nil {
		offered := false
		if c.SavedPaymentMethods {
			methods, err := r.State.PaymentMethodService.ListByIDs(ctx, c.CustomerID.UUID(), []uuid.UUID{pm.UUID()})
			if err != nil {
				r.InternalError("failed to read the payment method", err)
				return false
			}
			views, err := paymentMethodsView(r, identity.CustomerID(c.CustomerID.UUID()), methods, false)
			if err != nil {
				r.InternalError("failed to read the payment method", err)
				return false
			}
			offered = len(paymentpage.SavedCards(views)) == 1
		}
		if !offered {
			r.APIError(api.Coded(billing.CodeResourceNotFound, "payment method not found").WithParam("payment.payment_method_id"))
			return false
		}
	}
	exhausted, err := paymentpage.NewStore(r.State.DB).Exhausted(ctx, c)
	if err != nil {
		r.InternalError("failed to count the pay URL's attempts", err)
		return false
	}
	if exhausted {
		r.ErrorCode(billing.CodePayURLAttemptsExhausted, "")
		return false
	}
	return true
}

// MyInvoiceListQuery is a customer's own invoices' query.
type MyInvoiceListQuery struct {
	billing.PageRequest
	IDs            []billing.InvoiceID    `form:"ids,comma"`
	SubscriptionID billing.SubscriptionID `form:"subscription_id"`
	Status         billing.InvoiceStatus  `form:"status"`
	Origin         billing.InvoiceOrigin  `form:"origin"`
	Overdue        bool                   `form:"overdue"`
}

// invoiceListQuery reads an invoice list's filters: the merchant's, or a
// customer's own.
func invoiceListQuery(r *httprequest.Request, merchant bool) (billing.InvoiceListParams, bool) {
	var params billing.InvoiceListParams
	if merchant {
		if !bindList(r, &params, &params.PageRequest) {
			return params, false
		}
	} else {
		var query MyInvoiceListQuery
		if !bindList(r, &query, &query.PageRequest) {
			return params, false
		}
		params = billing.InvoiceListParams{PageRequest: query.PageRequest, IDs: query.IDs, SubscriptionID: query.SubscriptionID,
			Status: query.Status, Origin: query.Origin, Overdue: query.Overdue}
	}
	switch params.Status {
	case "", billing.InvoiceOpen, billing.InvoiceProcessing, billing.InvoiceComplete, billing.InvoiceCanceled, billing.InvoiceExpired, billing.InvoiceVoid, billing.InvoiceUncollectible:
	default:
		r.APIError(api.Coded(billing.CodeInvalidQuery, "status is invalid").WithParam("status"))
		return params, false
	}
	switch params.Origin {
	case "", billing.InvoiceOriginCustomer, billing.InvoiceOriginMerchant, billing.InvoiceOriginSubscription, billing.InvoiceOriginUsage:
	default:
		r.APIError(api.Coded(billing.CodeInvalidQuery, "origin is invalid").WithParam("origin"))
		return params, false
	}
	var ok bool
	if params.IDs, ok = listIDs(r, params.IDs); !ok {
		return params, false
	}
	return params, true
}

// WaiveInvoiceBalance handles POST /v1/admin/invoices/{id}/waive-balance: voids an unpaid usage
// invoice.
func WaiveInvoiceBalance(r *httprequest.Request) {
	usageInvoiceAction(r, (*billingservice.Service).VoidMerchantInvoice)
}

// MarkInvoiceUncollectible handles POST /v1/admin/invoices/{id}/mark-uncollectible:
// writes an open usage invoice off.
func MarkInvoiceUncollectible(r *httprequest.Request) {
	usageInvoiceAction(r, (*billingservice.Service).MarkMerchantInvoiceUncollectible)
}

func usageInvoiceAction(r *httprequest.Request, act func(*billingservice.Service, context.Context, billing.InvoiceID) (*billing.Invoice, error)) {
	id, ok := invoiceID(r)
	svc := invoiceService(r)
	if !ok || svc == nil {
		return
	}
	invoice, err := act(svc, r.Request.Context(), id)
	if err != nil {
		writeInvoiceError(r, err)
		return
	}
	writeInvoice(r, invoice)
}
