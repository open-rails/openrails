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

// StripeCollectionKeyMetadata carries the operation's provider identity on the
// Stripe invoice so an operator-supplied receipt can be matched exactly.
const StripeCollectionKeyMetadata = "openrails_collection_key"

func addStripeCollectionMetadata(values url.Values, p StripeInvoiceCollectionParams) {
	if key := strings.TrimSpace(p.IdempotencyKey); key != "" {
		values.Set("metadata["+StripeCollectionKeyMetadata+"]", key)
	}
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
	Currency      string `json:"currency"`
	PaymentIntent string `json:"payment_intent"`
	Charge        string `json:"charge"`
	Metadata      map[string]string
}

// StripeCollectionReceipt is one Stripe invoice read back for reconciliation.
type StripeCollectionReceipt struct {
	InvoiceID       string
	Status          string
	AmountPaid      int64
	Currency        string
	ChargeID        string
	PaymentIntentID string
	// CollectionKey is the operation identity stamped at creation.
	CollectionKey string
}

func (i stripeCollectionInvoice) receipt() StripeCollectionReceipt {
	r := StripeCollectionReceipt{InvoiceID: i.ID, Status: i.Status, AmountPaid: i.AmountPaid, Currency: i.Currency,
		ChargeID: strings.TrimSpace(i.Charge), PaymentIntentID: strings.TrimSpace(i.PaymentIntent)}
	r.CollectionKey = strings.TrimSpace(i.Metadata[StripeCollectionKeyMetadata])
	return r
}

// GetCollectionInvoice reads one Stripe invoice by its exact id. found=false
// on 404.
func (s *StripeService) GetCollectionInvoice(ctx context.Context, invoiceID string) (StripeCollectionReceipt, bool, error) {
	invoiceID = strings.TrimSpace(invoiceID)
	if invoiceID == "" {
		return StripeCollectionReceipt{}, false, errors.New("stripe invoice id is required")
	}
	body, status, err := s.stripeGet(ctx, "/v1/invoices/"+url.PathEscape(invoiceID), nil)
	if err != nil {
		return StripeCollectionReceipt{}, false, err
	}
	if status == http.StatusNotFound {
		return StripeCollectionReceipt{}, false, nil
	}
	if status >= 400 {
		return StripeCollectionReceipt{}, false, parseStripeAPIError(status, body)
	}
	inv, err := parseStripeCollectionInvoice(body)
	if err != nil {
		return StripeCollectionReceipt{}, false, err
	}
	return inv.receipt(), true, nil
}

// FindCollectionInvoiceByKey scans the customer's most recent invoices for
// the one stamped with the operation key. A bounded list read, not a search
// index: absence means "not among the last 100", never proof of non-execution.
func (s *StripeService) FindCollectionInvoiceByKey(ctx context.Context, customerID, key string) (StripeCollectionReceipt, bool, error) {
	customerID, key = strings.TrimSpace(customerID), strings.TrimSpace(key)
	if customerID == "" || key == "" {
		return StripeCollectionReceipt{}, false, errors.New("stripe customer id and collection key are required")
	}
	body, status, err := s.stripeGet(ctx, "/v1/invoices", url.Values{"customer": {customerID}, "limit": {"100"}})
	if err != nil {
		return StripeCollectionReceipt{}, false, err
	}
	if status >= 400 {
		return StripeCollectionReceipt{}, false, parseStripeAPIError(status, body)
	}
	var list struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return StripeCollectionReceipt{}, false, fmt.Errorf("parse stripe invoice list: %w", err)
	}
	for _, raw := range list.Data {
		inv, err := parseStripeCollectionInvoice(raw)
		if err != nil {
			return StripeCollectionReceipt{}, false, err
		}
		if r := inv.receipt(); r.CollectionKey == key {
			return r, true, nil
		}
	}
	return StripeCollectionReceipt{}, false, nil
}

func (s *StripeService) stripeGet(ctx context.Context, path string, query url.Values) ([]byte, int, error) {
	_, secretKey, err := RequireStripeSecretKey(ctx, s.Rails)
	if err != nil {
		return nil, 0, err
	}
	target := s.stripeBaseURL() + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+secretKey)
	resp, err := stripeapi.Client(s.Config, 0).Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, fmt.Errorf("read stripe response: %w", err)
	}
	return body, resp.StatusCode, nil
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
	out.Currency = strings.ToUpper(rawString(raw["currency"]))
	if len(raw["metadata"]) > 0 {
		_ = json.Unmarshal(raw["metadata"], &out.Metadata)
	}
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

// rawInt64 decodes a Stripe integer field. Stripe amounts are minor units and
// always integral on the wire; a non-integral value is a decode failure, not
// something to truncate through a float64 (MONEY-3).
func rawInt64(raw json.RawMessage) int64 {
	var n int64
	if err := json.Unmarshal(raw, &n); err == nil {
		return n
	}
	var num json.Number
	if err := json.Unmarshal(raw, &num); err == nil {
		if parsed, perr := strconv.ParseInt(num.String(), 10, 64); perr == nil {
			return parsed
		}
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
