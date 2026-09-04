package handlers

import (
	"errors"
	"net/http"
	"net/mail"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/permissions"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
	billingservice "github.com/open-rails/openrails/pkg/service"
	log "github.com/sirupsen/logrus"
)

func invoicePermission(r *httprequest.Request, gate billingauth.Gate, permission string) bool {
	if gate == nil {
		return false
	}
	_, err := gate.Authorize(r.Request.Context(), r.Request, permission)
	return err == nil
}
func permittedInvoiceActions(invoice *billingservice.MerchantInvoiceDTO, update, collect bool) {
	invoice.AvailableActions = slices.DeleteFunc(invoice.AvailableActions, func(action billingservice.InvoiceAdminAction) bool {
		if action == billingservice.InvoiceAdminRetryCollection {
			return !collect
		}
		return !update
	})
}
func invoicePage(r *httprequest.Request) (int, int, bool) {
	limit, offset := 50, 0
	for name, target := range map[string]*int{"limit": &limit, "offset": &offset} {
		if raw := r.Query(name); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 0 || n > 2147483647 {
				r.ErrorJSON(http.StatusBadRequest, "invalid "+name)
				return 0, 0, false
			}
			*target = n
		}
	}
	if limit < 1 || limit > 100 {
		r.ErrorJSON(http.StatusBadRequest, "limit must be between 1 and 100")
		return 0, 0, false
	}
	return limit, offset, true
}
func ListAdminInvoices(gate billingauth.Gate) func(*httprequest.Request) {
	return func(r *httprequest.Request) {
		limit, offset, ok := invoicePage(r)
		if !ok {
			return
		}
		filter := billingservice.MerchantInvoiceFilter{}
		if raw := strings.TrimSpace(r.Query("customer_id")); raw != "" {
			id, err := uuid.Parse(raw)
			if err != nil || id == uuid.Nil {
				r.ErrorJSON(http.StatusBadRequest, "invalid customer_id")
				return
			}
			filter.CustomerID = &id
		}
		if raw := strings.ToUpper(strings.TrimSpace(r.Query("currency"))); raw != "" {
			if err := money.RequireBillingCurrency(raw); err != nil {
				r.ErrorJSON(http.StatusBadRequest, "invalid invoice currency")
				return
			}
			filter.Currency = &raw
		}
		if raw := strings.TrimSpace(r.Query("status")); raw != "" {
			if !slices.Contains([]string{"draft", "open", "past_due", "paid", "voided", "uncollectible"}, raw) {
				r.ErrorJSON(http.StatusBadRequest, "invalid invoice status")
				return
			}
			filter.Status = &raw
		}
		for name, target := range map[string]**time.Time{"period_from": &filter.PeriodFrom, "period_to": &filter.PeriodTo} {
			if raw := r.Query(name); raw != "" {
				parsed, err := parseSelfTime(raw)
				if err != nil {
					r.ErrorJSON(http.StatusBadRequest, "invalid "+name)
					return
				}
				*target = &parsed
			}
		}
		if filter.PeriodFrom != nil && filter.PeriodTo != nil && !filter.PeriodTo.After(*filter.PeriodFrom) {
			r.ErrorJSON(http.StatusBadRequest, "period_to must be after period_from")
			return
		}
		svc, ok := newAdminBillingService(r)
		if !ok {
			return
		}
		items, total, err := svc.ListMerchantInvoices(r.Request.Context(), filter, limit, offset)
		if err != nil {
			writeInvoiceAdminError(r, err)
			return
		}
		update, collect := invoicePermission(r, gate, permissions.MerchantInvoicesUpdate), invoicePermission(r, gate, permissions.MerchantInvoicesCollect)
		for i := range items {
			permittedInvoiceActions(&items[i], update, collect)
		}
		r.JSON(http.StatusOK, paginatedResponse[billingservice.MerchantInvoiceDTO]{Items: items, Total: total, Limit: limit, Offset: offset})
	}
}

func loadAdminInvoice(r *httprequest.Request) (*billingservice.Service, *billingservice.MerchantInvoiceDTO, bool) {
	id, err := uuid.Parse(r.Param("id"))
	if err != nil || id == uuid.Nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid invoice id")
		return nil, nil, false
	}
	svc, ok := newAdminBillingService(r)
	if !ok {
		return nil, nil, false
	}
	invoice, err := svc.GetMerchantInvoice(r.Request.Context(), id)
	if err != nil {
		writeInvoiceAdminError(r, err)
		return nil, nil, false
	}
	return svc, invoice, true
}

type invoicePaymentMethodOption struct {
	ID       uuid.UUID `json:"id"`
	Rail     string    `json:"rail"`
	LastFour *string   `json:"last_four,omitempty"`
	CardType *string   `json:"card_type,omitempty"`
}

func GetAdminInvoice(gate billingauth.Gate) func(*httprequest.Request) {
	return func(r *httprequest.Request) {
		_, invoice, ok := loadAdminInvoice(r)
		if !ok {
			return
		}
		canCollect := invoicePermission(r, gate, permissions.MerchantInvoicesCollect)
		permittedInvoiceActions(invoice, invoicePermission(r, gate, permissions.MerchantInvoicesUpdate), canCollect)
		methods := make([]invoicePaymentMethodOption, 0)
		if canCollect && r.State.PaymentMethodService != nil {
			rows, err := r.State.PaymentMethodService.GetByUserID(r.Request.Context(), invoice.CustomerID.String())
			if err != nil {
				writeInvoiceAdminError(r, err)
				return
			}
			for _, method := range rows {
				methods = append(methods, invoicePaymentMethodOption{ID: method.ID, Rail: string(method.Rail), LastFour: method.LastFour, CardType: method.CardType})
			}
		}
		r.SuccessJSON(struct {
			billingservice.MerchantInvoiceDTO
			PaymentMethods []invoicePaymentMethodOption `json:"payment_methods"`
		}{*invoice, methods})
	}
}
func ListAdminInvoicePayments(r *httprequest.Request) {
	svc, invoice, ok := loadAdminInvoice(r)
	if !ok {
		return
	}
	limit, offset, ok := invoicePage(r)
	if !ok {
		return
	}
	items, total, err := svc.ListInvoicePaymentAttempts(r.Request.Context(), identity.CustomerID(invoice.CustomerID), invoice.ID, limit, offset)
	if err != nil {
		writeInvoiceAdminError(r, err)
		return
	}
	type entry struct {
		billingservice.InvoicePaymentAttemptDTO
		UnitDecimals int `json:"unit_decimals"`
	}
	rows := make([]entry, 0, len(items))
	for _, item := range items {
		scale, ok := moneyutil.CurrencyScale(item.Currency)
		if !ok {
			r.ErrorJSON(http.StatusInternalServerError, "invoice currency is not registered")
			return
		}
		rows = append(rows, entry{item, scale})
	}
	r.JSON(http.StatusOK, paginatedResponse[entry]{Items: rows, Total: int64(total), Limit: limit, Offset: offset})
}
func MutateAdminInvoice(action billingservice.InvoiceAdminAction) func(*httprequest.Request) {
	return func(r *httprequest.Request) {
		svc, invoice, ok := loadAdminInvoice(r)
		if !ok {
			return
		}
		var body struct {
			Amount    int64  `json:"amount"`
			Reference string `json:"reference"`
		}
		if action == billingservice.InvoiceAdminRecordPayment {
			if !r.BindJSON(&body) {
				return
			}
			body.Reference = strings.TrimSpace(body.Reference)
			if body.Amount <= 0 || body.Reference == "" || len(body.Reference) > 255 {
				r.ErrorJSON(http.StatusBadRequest, "positive amount and a reference of 1–255 bytes are required")
				return
			}
		}
		result, err := svc.ApplyMerchantInvoiceMutation(r.Request.Context(), identity.CustomerID(invoice.CustomerID), invoice.ID, billingservice.InvoiceAdminMutation{Action: action, Amount: body.Amount, Reference: body.Reference})
		if err != nil {
			writeInvoiceAdminError(r, err)
			return
		}
		r.SuccessJSON(result)
	}
}
func RetryAdminInvoiceCollection(r *httprequest.Request) {
	svc, invoice, ok := loadAdminInvoice(r)
	if !ok {
		return
	}
	var body struct {
		PaymentMethodID uuid.UUID `json:"payment_method_id"`
	}
	if !r.BindJSON(&body) {
		return
	}
	key := strings.TrimSpace(r.Request.Header.Get("Idempotency-Key"))
	if body.PaymentMethodID == uuid.Nil || key == "" || len(key) > 255 {
		r.ErrorJSON(http.StatusBadRequest, "payment_method_id and Idempotency-Key (1–255 bytes) are required")
		return
	}
	// Do not gate on the read snapshot: a successful retry replay is valid even
	// after the invoice becomes paid. The existing durable claim owns eligibility.
	result, err := svc.RetryInvoiceCollectionIdempotent(r.Request.Context(), identity.CustomerID(invoice.CustomerID), billingservice.InvoiceCollectionRetryRequest{InvoiceID: invoice.ID, PaymentMethodID: body.PaymentMethodID, IdempotencyKey: key})
	if err != nil {
		writeInvoiceAdminError(r, err)
		return
	}
	status := http.StatusOK
	if result.Attempt.Status != "settled" && result.Attempt.Status != "failed" {
		status = http.StatusAccepted
	}
	r.JSON(status, result)
}
func invoiceProfileCustomer(r *httprequest.Request) (identity.CustomerID, bool) {
	if r.State == nil || r.State.DB == nil {
		r.ErrorJSON(http.StatusServiceUnavailable, "invoice profile unavailable")
		return identity.CustomerID{}, false
	}
	id, err := uuid.Parse(r.Param("customer_id"))
	if err != nil || id == uuid.Nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid customer_id")
		return identity.CustomerID{}, false
	}
	mid, err := merchant.Require(r.Request.Context())
	if err != nil {
		writeInvoiceAdminError(r, err)
		return identity.CustomerID{}, false
	}
	exists, err := r.State.DB.Gen(r.Request.Context()).InvoiceProfileCustomerExists(r.Request.Context(), gen.InvoiceProfileCustomerExistsParams{MerchantID: mid.UUID(), CustomerID: id})
	if err != nil {
		writeInvoiceAdminError(r, err)
		return identity.CustomerID{}, false
	}
	if !exists {
		r.ErrorJSON(http.StatusNotFound, "customer not found")
		return identity.CustomerID{}, false
	}
	return identity.CustomerID(id), true
}
func GetAdminInvoiceProfile(gate billingauth.Gate) func(*httprequest.Request) {
	return func(r *httprequest.Request) {
		payer, ok := invoiceProfileCustomer(r)
		if !ok {
			return
		}
		svc, ok := newAdminBillingService(r)
		if !ok {
			return
		}
		profile, err := svc.GetCustomerInvoiceProfile(r.Request.Context(), payer)
		if err != nil {
			writeInvoiceAdminError(r, err)
			return
		}
		r.SuccessJSON(map[string]any{"customer_id": payer.UUID(), "profile": profile, "can_update": invoicePermission(r, gate, permissions.MerchantCustomerSettingsUpdate)})
	}
}
func PutAdminInvoiceProfile(r *httprequest.Request) {
	payer, ok := invoiceProfileCustomer(r)
	if !ok {
		return
	}
	var body billingservice.InvoiceProfileDTO
	if !r.BindJSON(&body) {
		return
	}
	if body.NetTermsDays < 0 || int64(body.NetTermsDays) > money.MaxInvoiceNetTermsDays || !slices.Contains([]string{"charge_automatically", "send_invoice"}, body.CollectionMethod) {
		r.ErrorJSON(http.StatusBadRequest, "invalid invoice terms or collection method")
		return
	}
	for i := range body.BillingContacts {
		body.BillingContacts[i].Email = strings.TrimSpace(body.BillingContacts[i].Email)
		address, err := mail.ParseAddress(body.BillingContacts[i].Email)
		if err != nil || address.Address != body.BillingContacts[i].Email {
			r.ErrorJSON(http.StatusBadRequest, "billing contacts must contain valid email addresses")
			return
		}
	}
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	if err := svc.SetCustomerInvoiceProfile(r.Request.Context(), payer, body); err != nil {
		writeInvoiceAdminError(r, err)
		return
	}
	profile, err := svc.GetCustomerInvoiceProfile(r.Request.Context(), payer)
	if err != nil {
		writeInvoiceAdminError(r, err)
		return
	}
	r.SuccessJSON(profile)
}
func writeInvoiceAdminError(r *httprequest.Request, err error) {
	switch {
	case db.IsNotFound(err), errors.Is(err, db.ErrCustomerOwnedByAnotherMerchant):
		r.ErrorJSON(http.StatusNotFound, "invoice or customer not found")
	case errors.Is(err, money.ErrInvoiceActionNotAllowed), errors.Is(err, money.ErrInvoiceNotRetryable), errors.Is(err, money.ErrInvoiceRetryInProgress), errors.Is(err, money.ErrInvoiceRetryOutcomeUnknown), errors.Is(err, money.ErrInvoiceRetryIdempotencyConflict), errors.Is(err, money.ErrInvoicePaymentReferenceUsed), errors.Is(err, money.ErrInvoicePaymentExceedsDue):
		r.ErrorJSON(http.StatusConflict, err.Error())
	case errors.Is(err, money.ErrInvoicePaymentInvalid), errors.Is(err, money.ErrCollectionPaymentMethodInvalid), errors.Is(err, money.ErrCollectionPaymentMethodRequired):
		r.ErrorJSON(http.StatusBadRequest, err.Error())
	default:
		log.WithError(err).Warn("merchant invoice operation failed")
		r.ErrorJSON(http.StatusInternalServerError, "invoice operation failed")
	}
}
