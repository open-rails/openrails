package subscriptions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

type StripeInvoiceCollectionParams struct {
	CustomerID      string
	PaymentMethodID string
	// AmountCents is rail minor units (typed Cents, #671).
	AmountCents         moneyutil.Cents
	Currency            string
	Description         string
	IdempotencyKey      string
	OpenRailsInvoiceID  string
	OpenRailsMerchantID string
	OpenRailsCustomerID string
}

type StripeInvoiceCollectionResult struct {
	InvoiceID       string
	PaymentIntentID string
	ChargeID        string
	Status          string
}

type StripeAPIError struct {
	StatusCode  int
	Message     string
	Code        string
	DeclineCode string
}

func (e *StripeAPIError) Error() string {
	if e == nil {
		return ""
	}
	msg := strings.TrimSpace(e.Message)
	if msg == "" {
		msg = "stripe API error"
	}
	if e.StatusCode > 0 {
		return fmt.Sprintf("%s (%d)", msg, e.StatusCode)
	}
	return msg
}

func (e *StripeAPIError) FailureCode() string {
	if e == nil {
		return "stripe_error"
	}
	if code := strings.TrimSpace(e.DeclineCode); code != "" {
		return code
	}
	if code := strings.TrimSpace(e.Code); code != "" {
		return code
	}
	return "stripe_error"
}

func (e *StripeAPIError) IsHardDecline() bool {
	if e == nil {
		return false
	}
	switch e.FailureCode() {
	case "card_declined", "expired_card", "incorrect_cvc", "incorrect_number",
		"insufficient_funds", "lost_card", "pickup_card", "stolen_card",
		"do_not_honor", "transaction_not_allowed":
		return true
	default:
		return false
	}
}

func (s *StripeService) CollectInvoice(ctx context.Context, params StripeInvoiceCollectionParams) (*StripeInvoiceCollectionResult, error) {
	if err := params.validate(); err != nil {
		return nil, err
	}
	if _, err := s.stripePostForm(ctx, "/v1/invoiceitems", params.invoiceItemValues(), params.IdempotencyKey+":invoice_item"); err != nil {
		return nil, fmt.Errorf("stripe invoice item create: %w", err)
	}

	invoiceBody, err := s.stripePostForm(ctx, "/v1/invoices", params.invoiceValues(), params.IdempotencyKey+":invoice")
	if err != nil {
		return nil, fmt.Errorf("stripe invoice create: %w", err)
	}
	invoice, err := parseStripeCollectionInvoice(invoiceBody)
	if err != nil {
		return nil, err
	}
	if invoice.ID == "" {
		return nil, errors.New("stripe invoice create returned empty id")
	}

	finalizedBody, err := s.stripePostForm(ctx, "/v1/invoices/"+url.PathEscape(invoice.ID)+"/finalize", url.Values{"auto_advance": {"false"}}, params.IdempotencyKey+":finalize")
	if err != nil {
		return nil, fmt.Errorf("stripe invoice finalize: %w", err)
	}
	invoice, err = parseStripeCollectionInvoice(finalizedBody)
	if err != nil {
		return nil, err
	}
	if strings.EqualFold(invoice.Status, "paid") {
		if moneyutil.Cents(invoice.AmountPaid) < params.AmountCents {
			return nil, fmt.Errorf("stripe invoice %s paid only %d of %d", invoice.ID, invoice.AmountPaid, params.AmountCents)
		}
		return invoice.result(), nil
	}

	paidBody, err := s.stripePostForm(ctx, "/v1/invoices/"+url.PathEscape(invoice.ID)+"/pay", url.Values{"payment_method": {params.PaymentMethodID}}, params.IdempotencyKey+":pay")
	if err != nil {
		return nil, fmt.Errorf("stripe invoice pay: %w", err)
	}
	invoice, err = parseStripeCollectionInvoice(paidBody)
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(invoice.Status, "paid") {
		return nil, fmt.Errorf("stripe invoice %s not paid after collection attempt: status=%s", invoice.ID, invoice.Status)
	}
	if moneyutil.Cents(invoice.AmountPaid) < params.AmountCents {
		return nil, fmt.Errorf("stripe invoice %s paid only %d of %d", invoice.ID, invoice.AmountPaid, params.AmountCents)
	}
	return invoice.result(), nil
}

func (p StripeInvoiceCollectionParams) validate() error {
	if strings.TrimSpace(p.CustomerID) == "" {
		return errors.New("stripe customer_id is required")
	}
	if strings.TrimSpace(p.PaymentMethodID) == "" {
		return errors.New("stripe payment_method_id is required")
	}
	if p.AmountCents <= 0 {
		return errors.New("amount_cents must be positive")
	}
	if strings.TrimSpace(p.Currency) == "" {
		return errors.New("currency is required")
	}
	if strings.TrimSpace(p.IdempotencyKey) == "" {
		return errors.New("idempotency_key is required")
	}
	return nil
}

func (p StripeInvoiceCollectionParams) invoiceItemValues() url.Values {
	values := url.Values{}
	values.Set("customer", strings.TrimSpace(p.CustomerID))
	values.Set("amount", strconv.FormatInt(int64(p.AmountCents), 10))
	values.Set("currency", strings.ToLower(strings.TrimSpace(p.Currency)))
	if description := strings.TrimSpace(p.Description); description != "" {
		values.Set("description", description)
	}
	addStripeCollectionMetadata(values, p)
	return values
}

func (p StripeInvoiceCollectionParams) invoiceValues() url.Values {
	values := url.Values{}
	values.Set("customer", strings.TrimSpace(p.CustomerID))
	values.Set("collection_method", "charge_automatically")
	values.Set("default_payment_method", strings.TrimSpace(p.PaymentMethodID))
	values.Set("auto_advance", "false")
	values.Set("pending_invoice_items_behavior", "include")
	addStripeCollectionMetadata(values, p)
	return values
}

func addStripeCollectionMetadata(values url.Values, p StripeInvoiceCollectionParams) {
	if invoiceID := strings.TrimSpace(p.OpenRailsInvoiceID); invoiceID != "" {
		values.Set("metadata[openrails_invoice_id]", invoiceID)
	}
	if merchantID := strings.TrimSpace(p.OpenRailsMerchantID); merchantID != "" {
		values.Set("metadata[openrails_merchant_id]", merchantID)
	}
	if customerID := strings.TrimSpace(p.OpenRailsCustomerID); customerID != "" {
		values.Set("metadata[openrails_customer_id]", customerID)
	}
}

func (s *StripeService) stripePostForm(ctx context.Context, path string, values url.Values, idempotencyKey string) ([]byte, error) {
	_, secretKey, err := RequireStripeSecretKey(ctx, s.Rails)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.stripeBaseURL()+path, strings.NewReader(values.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+secretKey)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	stripeapi.SetIdempotencyKey(req, strings.TrimSpace(idempotencyKey))

	resp, err := stripeapi.Client(s.Config, 0).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read stripe response: %w", err)
	}
	if resp.StatusCode >= 400 {
		return nil, parseStripeAPIError(resp.StatusCode, body)
	}
	return body, nil
}

func parseStripeAPIError(statusCode int, body []byte) error {
	var out struct {
		Error struct {
			Message     string `json:"message"`
			Code        string `json:"code"`
			DeclineCode string `json:"decline_code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return &StripeAPIError{StatusCode: statusCode, Message: fmt.Sprintf("stripe API failed (%d)", statusCode)}
	}
	msg := strings.TrimSpace(out.Error.Message)
	if msg == "" {
		msg = fmt.Sprintf("stripe API failed (%d)", statusCode)
	}
	return &StripeAPIError{
		StatusCode:  statusCode,
		Message:     msg,
		Code:        strings.TrimSpace(out.Error.Code),
		DeclineCode: strings.TrimSpace(out.Error.DeclineCode),
	}
}

type stripeCollectionInvoice struct {
	ID            string `json:"id"`
	Status        string `json:"status"`
	AmountPaid    int64  `json:"amount_paid"`
	PaymentIntent string `json:"payment_intent"`
	Charge        string `json:"charge"`
}

func parseStripeCollectionInvoice(body []byte) (stripeCollectionInvoice, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return stripeCollectionInvoice{}, fmt.Errorf("parse stripe invoice response: %w", err)
	}
	out := stripeCollectionInvoice{}
	out.ID = rawString(raw["id"])
	out.Status = rawString(raw["status"])
	out.AmountPaid = rawInt64(raw["amount_paid"])
	out.PaymentIntent = rawID(raw["payment_intent"])
	out.Charge = rawID(raw["charge"])
	if out.Charge == "" {
		out.Charge = rawID(raw["latest_charge"])
	}
	return out, nil
}

func rawID(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	if s := rawString(raw); s != "" {
		return s
	}
	var obj struct {
		ID           string          `json:"id"`
		LatestCharge json.RawMessage `json:"latest_charge"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return ""
	}
	if id := strings.TrimSpace(obj.ID); id != "" {
		return id
	}
	return rawID(obj.LatestCharge)
}

func rawString(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return strings.TrimSpace(s)
}

func rawInt64(raw json.RawMessage) int64 {
	var n int64
	if err := json.Unmarshal(raw, &n); err == nil {
		return n
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		return int64(f)
	}
	return 0
}

func (i stripeCollectionInvoice) result() *StripeInvoiceCollectionResult {
	return &StripeInvoiceCollectionResult{
		InvoiceID:       strings.TrimSpace(i.ID),
		PaymentIntentID: strings.TrimSpace(i.PaymentIntent),
		ChargeID:        strings.TrimSpace(i.Charge),
		Status:          strings.TrimSpace(i.Status),
	}
}
