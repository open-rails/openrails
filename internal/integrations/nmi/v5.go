package nmi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/open-rails/openrails/internal/shared/moneyutil"
	log "github.com/sirupsen/logrus"
)

// v5 JSON API transport (#663). Everything except three classic survivors runs
// through here:
//   - transact.php keeps ONLY recurring=add_subscription (atomic first-charge +
//     enroll + delayed start has no v5 equivalent) and recurring=rebill_subscription
//     (no v5 equivalent at all);
//   - query.php keeps ONLY transaction SEARCH (v5 payments has no list/search,
//     v4's report endpoint is partner-key-only).
//
// Auth: the classic security key IS the v5 credential ("no need to generate new
// API keys"), sent as the ENTIRE Authorization header value — no Bearer/scheme.

const DefaultV5BaseURL = "https://secure.nmi.com/api/v5"

// ErrV5NotFound marks a v5 404 — the resource does not exist at NMI.
var ErrV5NotFound = errors.New("nmi: resource not found")

// v5Error is the modern error envelope (HTTP 4xx/5xx bodies).
type v5Error struct {
	Type      string `json:"type"`
	ErrorCode string `json:"error_code"`
	Message   string `json:"message"`
	RefID     string `json:"ref_id"`
}

// centsJSONAmount renders integer cents as an exact two-decimal JSON number
// (1099 -> 10.99). Never float math: this is a money wire boundary (#671).
func centsJSONAmount(cents moneyutil.Cents) json.RawMessage {
	neg := ""
	if cents < 0 {
		neg, cents = "-", -cents
	}
	return json.RawMessage(fmt.Sprintf("%s%d.%02d", neg, cents/100, cents%100))
}

// v5AmountToCents parses a v5 decimal amount string ("10.99", "-5.00") into
// cents without float rounding surprises on the happy path.
func v5AmountToCents(amount string) (int64, error) {
	trimmed := strings.TrimSpace(amount)
	if trimmed == "" {
		return 0, errors.New("empty amount")
	}
	neg := strings.HasPrefix(trimmed, "-")
	trimmed = strings.TrimPrefix(trimmed, "-")
	whole, frac, _ := strings.Cut(trimmed, ".")
	if whole == "" {
		whole = "0"
	}
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, err
	}
	var f int64
	switch len(frac) {
	case 0:
	case 1:
		f, err = strconv.ParseInt(frac, 10, 64)
		f *= 10
	case 2:
		f, err = strconv.ParseInt(frac, 10, 64)
	default:
		f, err = strconv.ParseInt(frac[:2], 10, 64)
	}
	if err != nil {
		return 0, err
	}
	cents := w*100 + f
	if neg {
		cents = -cents
	}
	return cents, nil
}

// sendV5Request performs one v5 JSON round-trip. Non-GET requests honor the
// read-only guard exactly like sendDirectRequest. out may be nil (delete
// endpoints). A non-2xx status decodes the modern error envelope; 404 wraps
// ErrV5NotFound so callers can treat "gone at NMI" as a state, not a failure.
func (c *NMIClient) sendV5Request(method, path string, body any, out any) (err error) {
	if method != http.MethodGet && c.ReadOnly {
		log.WithFields(log.Fields{
			"provider": c.providerName,
			"method":   method,
			"path":     path,
		}).Warn("NMI v5 request blocked: provider is read-only (mode=readonly)")
		return ErrProviderReadOnly
	}

	var reqBody io.Reader
	if body != nil {
		encoded, merr := json.Marshal(body)
		if merr != nil {
			return fmt.Errorf("encode v5 request: %w", merr)
		}
		reqBody = bytes.NewReader(encoded)
	}

	req, err := http.NewRequest(method, c.v5BaseURL()+path, reqBody)
	if err != nil {
		return fmt.Errorf("build v5 request: %w", err)
	}
	// The bare private key is the whole header value — no Bearer/ApiKey scheme.
	req.Header.Set("Authorization", c.SecurityKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	// Non-GET = mutation: failures past the send may have executed at the
	// gateway and are wrapped transport-ambiguous (#674). Parsed 4xx envelopes
	// stay clean (the gateway rejected the REQUEST); 5xx / lost responses do not.
	mutating := method != http.MethodGet
	classify := func(err error) error {
		if mutating {
			return ambiguous(err)
		}
		return err
	}

	resp, err := c.client().Do(req)
	if err != nil {
		return classify(fmt.Errorf("send v5 request: %w", err))
	}
	defer func() {
		cerr := resp.Body.Close()
		if err == nil {
			err = cerr
		}
	}()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return classify(fmt.Errorf("read v5 response: %w", err))
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var envelope v5Error
		_ = json.Unmarshal(raw, &envelope)
		msg := strings.TrimSpace(envelope.Message)
		if msg == "" {
			msg = strings.TrimSpace(string(raw))
		}
		if resp.StatusCode == http.StatusNotFound {
			return fmt.Errorf("%w: %s %s: %s", ErrV5NotFound, method, path, msg)
		}
		err := fmt.Errorf("nmi v5 %s %s: status %d (%s %s): %s",
			method, path, resp.StatusCode, envelope.Type, envelope.ErrorCode, msg)
		if resp.StatusCode >= 500 {
			return classify(err)
		}
		return err
	}

	if out != nil && len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			// 2xx arrived: the mutation executed; only the result is unreadable.
			return classify(fmt.Errorf("decode v5 response: %w", err))
		}
	}
	return nil
}

func (c *NMIClient) v5BaseURL() string {
	if strings.TrimSpace(c.V5BaseURL) != "" {
		return strings.TrimRight(c.V5BaseURL, "/")
	}
	return DefaultV5BaseURL
}

// --- payments ---

// v5PaymentDetails is the request-side payment_details object. Exactly one
// variant: raw card (test-mode probe only) or Collect.js / Payment Component
// token. Vault charges use the top-level customer_vault:{id} object instead
// (the documented payment_details.customer_vault_id is rejected live).
type v5PaymentDetails struct {
	CardNumber   string `json:"card_number,omitempty"`
	CardExp      string `json:"card_exp,omitempty"`
	PaymentToken string `json:"payment_token,omitempty"`
}

type v5OrderDetails struct {
	ID               string `json:"id,omitempty"`
	OrderDescription string `json:"order_description,omitempty"`
}

// v5CustomerVaultRef charges a stored vault customer. The docs show
// payment_details.customer_vault_id for vault sales, but the live gateway
// rejects that as an extra parameter and wants a top-level
// customer_vault:{id} object (verified 2026-07-01).
type v5CustomerVaultRef struct {
	ID string `json:"id"`
}

type v5PaymentRequest struct {
	Amount         json.RawMessage     `json:"amount,omitempty"`
	Currency       string              `json:"currency,omitempty"`
	PaymentDetails *v5PaymentDetails   `json:"payment_details,omitempty"`
	CustomerVault  *v5CustomerVaultRef `json:"customer_vault,omitempty"`
	OrderDetails   *v5OrderDetails     `json:"order_details,omitempty"`
}

// v5Transaction is the slice of TransactionResponse openrails consumes.
// response keeps the classic 1/2/3 semantics; response_code the classic
// gateway/processor result-code space (100 approved, 2xx declined, ...).
type v5Transaction struct {
	Object          string        `json:"object"`
	ID              string        `json:"id"`
	Amount          string        `json:"amount"`
	Currency        string        `json:"currency"`
	AuthCode        string        `json:"auth_code"`
	CustomerVaultID string        `json:"customer_vault_id"`
	Status          string        `json:"status"`
	Response        string        `json:"response"`
	ResponseText    string        `json:"response_text"`
	ResponseCode    string        `json:"response_code"`
	Actions         []v5TxnAction `json:"actions"`
}

type v5TxnAction struct {
	ID           string `json:"id"`
	Type         string `json:"type"`
	Amount       string `json:"amount"`
	Success      bool   `json:"success"`
	Response     string `json:"response"`
	ResponseCode string `json:"response_code"`
}

func (t *v5Transaction) approved() bool { return strings.TrimSpace(t.Response) == "1" }

func (t *v5Transaction) resultCode() int {
	code, _ := strconv.Atoi(strings.TrimSpace(t.ResponseCode))
	if code == 0 && strings.TrimSpace(t.Response) == "2" {
		return 200
	}
	return code
}

// newV5TransactionError maps a declined/errored v5 transaction onto the same
// CustomerVaultError shape the classic path produced, so caller error handling
// (localization ids, hard/soft decline classification) is unchanged.
func newV5TransactionError(prefix string, txn *v5Transaction) error {
	message := strings.TrimSpace(txn.ResponseText)
	if message == "" {
		message = "declined"
	}
	code := txn.resultCode()
	rawResponse, _ := json.Marshal(txn)
	return &CustomerVaultError{
		Message:        fmt.Sprintf("%s: %s", prefix, message),
		ResponseCode:   code,
		LocalizationID: nmiLocalizationID(code),
		Detail:         nmiResponseDetail(code),
		RawResponse:    string(rawResponse),
	}
}

// --- customers (Customer Vault) ---

type v5CustomerBillingRequest struct {
	// ID targets an existing billing entry on update; the live gateway
	// REQUIRES it (the documented omit-for-priority-1 behavior 400s).
	ID             string            `json:"id,omitempty"`
	Currency       string            `json:"currency,omitempty"`
	PaymentDetails *v5PaymentDetails `json:"payment_details,omitempty"`
	FirstName      string            `json:"first_name,omitempty"`
	LastName       string            `json:"last_name,omitempty"`
	Company        string            `json:"company,omitempty"`
	Address1       string            `json:"address1,omitempty"`
	Address2       string            `json:"address2,omitempty"`
	City           string            `json:"city,omitempty"`
	State          string            `json:"state,omitempty"`
	Zip            string            `json:"zip,omitempty"`
	Country        string            `json:"country,omitempty"`
	Phone          string            `json:"phone,omitempty"`
	Email          string            `json:"email,omitempty"`
}

type V5CustomerBilling struct {
	ID             string            `json:"id"`
	FirstName      string            `json:"first_name"`
	LastName       string            `json:"last_name"`
	Email          string            `json:"email"`
	Priority       *int              `json:"priority"`
	PaymentDetails V5BillingCardData `json:"payment_details"`
}

// v5BillingCardData is the response-side stored payment method (masked).
type V5BillingCardData struct {
	CardNumber string `json:"card_number"`
	CardExp    string `json:"card_exp"`
	CardType   string `json:"card_type"`
}

type V5Customer struct {
	Object  string              `json:"object"`
	ID      string              `json:"id"`
	Created string              `json:"created"`
	Billing []V5CustomerBilling `json:"billing"`
}

// PrimaryBilling returns the priority-1 (or first) billing record.
func (c *V5Customer) PrimaryBilling() *V5CustomerBilling {
	for i := range c.Billing {
		if c.Billing[i].Priority != nil && *c.Billing[i].Priority == 1 {
			return &c.Billing[i]
		}
	}
	if len(c.Billing) > 0 {
		return &c.Billing[0]
	}
	return nil
}

// V5Cursor is a v5 pagination cursor. The docs declare next_cursor as a
// nullable integer, but the live gateway returns a STRING (verified
// 2026-07-01) — decode both, normalize to string ("" = no more pages).
type V5Cursor string

func (c *V5Cursor) UnmarshalJSON(raw []byte) error {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "null" || trimmed == `""` {
		*c = ""
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		*c = V5Cursor(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err != nil {
		return fmt.Errorf("unrecognized cursor %s", trimmed)
	}
	*c = V5Cursor(n.String())
	return nil
}

// CustomerPage is one cursor page of the v5 customer roster.
type CustomerPage struct {
	Customers  []V5Customer `json:"customers"`
	NextCursor V5Cursor     `json:"next_cursor"`
	HasMore    bool         `json:"has_more"`
}

// ListCustomersPage pulls one page of the customer vault roster
// (GET /v5/customers). id optionally filters to a single vault id.
func (c *NMIClient) ListCustomersPage(cursor string, perPage int, id string) (CustomerPage, error) {
	var page CustomerPage
	if err := c.checkConfiguration(); err != nil {
		return page, err
	}
	q := url.Values{}
	if strings.TrimSpace(cursor) != "" {
		q.Set("cursor", strings.TrimSpace(cursor))
	}
	if perPage > 0 {
		q.Set("per_page", strconv.Itoa(perPage))
	}
	if strings.TrimSpace(id) != "" {
		q.Set("id", strings.TrimSpace(id))
	}
	path := "/customers"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	if err := c.sendV5Request(http.MethodGet, path, nil, &page); err != nil {
		return page, err
	}
	return page, nil
}

// --- subscriptions ---

// V5Subscription is the slice of the v5 subscription resource openrails reads.
type V5Subscription struct {
	Object          string `json:"object"`
	ID              string `json:"id"`
	StartDate       string `json:"start_date"`
	NextBillingDate string `json:"next_billing_date"`
	Amount          string `json:"amount"`
	CustomerVaultID string `json:"customer_vault_id"`
	// DelayedCondition is NMI's de-facto lifecycle flag (live-verified
	// 2026-07-01): "active" on every live subscription, "inactive" after a
	// delete. The v5 GET keeps answering 200 for deleted subscriptions (a
	// tombstone) — only never-existed ids 404. The LIST excludes deleted ones.
	DelayedCondition   string  `json:"delayed_condition"`
	PausedSubscription any     `json:"paused_subscription"` // "0"/"1" string on the wire despite docs
	Plan               *V5Plan `json:"plan"`
}

// cancelledAtNMI reports whether the record is a deletion tombstone.
func (s *V5Subscription) cancelledAtNMI() bool {
	return strings.EqualFold(strings.TrimSpace(s.DelayedCondition), "inactive")
}

// V5Plan mirrors the v5 plan resource (all-string serializer).
type V5Plan struct {
	Object         string `json:"object"`
	ID             string `json:"id"`
	PlanName       string `json:"plan_name"`
	PlanAmount     string `json:"plan_amount"`
	PlanPayments   string `json:"plan_payments"`
	DayFrequency   string `json:"day_frequency"`
	MonthFrequency string `json:"month_frequency"`
	DayOfMonth     string `json:"day_of_month"`
}

// SubscriptionPage is one cursor page of the v5 subscription roster.
type SubscriptionPage struct {
	Subscriptions []V5Subscription `json:"subscriptions"`
	NextCursor    V5Cursor         `json:"next_cursor"`
	HasMore       bool             `json:"has_more"`
}

// ListSubscriptionsPage pulls one page of the recurring roster
// (GET /v5/subscriptions).
func (c *NMIClient) ListSubscriptionsPage(cursor string, perPage int) (SubscriptionPage, error) {
	var page SubscriptionPage
	if err := c.checkConfiguration(); err != nil {
		return page, err
	}
	q := url.Values{}
	if strings.TrimSpace(cursor) != "" {
		q.Set("cursor", strings.TrimSpace(cursor))
	}
	if perPage > 0 {
		q.Set("per_page", strconv.Itoa(perPage))
	}
	path := "/subscriptions"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	if err := c.sendV5Request(http.MethodGet, path, nil, &page); err != nil {
		return page, err
	}
	return page, nil
}

// GetSubscription fetches one subscription by id. found=false when the
// subscription is GONE at NMI — either a 404 (never existed) or a deletion
// tombstone (200 with delayed_condition=inactive; live-verified 2026-07-01).
// found therefore means "live recurring record", matching the classic
// recurring report's absence-is-terminal semantics that #664/#665 depend on.
func (c *NMIClient) GetSubscription(subscriptionID string) (V5Subscription, bool, error) {
	var sub V5Subscription
	if err := c.checkConfiguration(); err != nil {
		return sub, false, err
	}
	if strings.TrimSpace(subscriptionID) == "" {
		return sub, false, errors.New("subscriptionID is required")
	}
	err := c.sendV5Request(http.MethodGet, "/subscriptions/"+url.PathEscape(strings.TrimSpace(subscriptionID)), nil, &sub)
	if errors.Is(err, ErrV5NotFound) {
		return sub, false, nil
	}
	if err != nil {
		return sub, false, err
	}
	if sub.cancelledAtNMI() {
		return sub, false, nil
	}
	return sub, true, nil
}

// GetPayment fetches one transaction by id (GET /v5/payments/{id}).
// found=false on 404.
func (c *NMIClient) GetPayment(transactionID string) (v5Transaction, bool, error) {
	var txn v5Transaction
	if err := c.checkConfiguration(); err != nil {
		return txn, false, err
	}
	if strings.TrimSpace(transactionID) == "" {
		return txn, false, errors.New("transactionID is required")
	}
	err := c.sendV5Request(http.MethodGet, "/payments/"+url.PathEscape(strings.TrimSpace(transactionID)), nil, &txn)
	if errors.Is(err, ErrV5NotFound) {
		return txn, false, nil
	}
	if err != nil {
		return txn, false, err
	}
	return txn, true, nil
}

// PaymentAction is one action on a payment (sale/refund/credit/void/...),
// normalized for the refund verifier.
type PaymentAction struct {
	TransactionID string
	Type          string
	AmountCents   int64
	Success       bool
}

// GetPaymentActions returns the actions recorded on a transaction. found=false
// when the transaction id is unknown at NMI.
func (c *NMIClient) GetPaymentActions(transactionID string) ([]PaymentAction, bool, error) {
	txn, found, err := c.GetPayment(transactionID)
	if err != nil || !found {
		return nil, found, err
	}
	out := make([]PaymentAction, 0, len(txn.Actions))
	for _, a := range txn.Actions {
		cents, cerr := v5AmountToCents(a.Amount)
		if cerr != nil {
			cents = 0
		}
		if cents < 0 {
			cents = -cents
		}
		id := strings.TrimSpace(a.ID)
		if id == "" {
			id = strings.TrimSpace(txn.ID)
		}
		out = append(out, PaymentAction{
			TransactionID: id,
			Type:          strings.ToLower(strings.TrimSpace(a.Type)),
			AmountCents:   cents,
			Success:       a.Success,
		})
	}
	return out, true, nil
}

// --- plans ---

// v5PlanCreateRequest: the docs call the identifier `plan_id`, but the live
// gateway rejects that as an extra parameter and wants `id` (verified
// 2026-07-01).
type v5PlanCreateRequest struct {
	PlanID       string          `json:"id"`
	PlanName     string          `json:"plan_name"`
	PlanAmount   json.RawMessage `json:"plan_amount"`
	PlanPayments int             `json:"plan_payments"`
	DayFrequency int             `json:"day_frequency,omitempty"`
}

// PlanPage is one cursor page of the v5 plan list.
type PlanPage struct {
	Plans      []V5Plan `json:"plans"`
	NextCursor V5Cursor `json:"next_cursor"`
	HasMore    bool     `json:"has_more"`
}

// ListRecurringPlans pulls the complete plan catalog (GET /v5/plans, all
// cursor pages). Empty pages mid-stream are legal (observed live) — the loop
// ends only when the gateway stops handing out a cursor.
func (c *NMIClient) ListRecurringPlans() ([]V5Plan, error) {
	if err := c.checkConfiguration(); err != nil {
		return nil, err
	}
	var out []V5Plan
	cursor := ""
	seen := map[string]bool{}
	for {
		q := url.Values{}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		path := "/plans"
		if len(q) > 0 {
			path += "?" + q.Encode()
		}
		var page PlanPage
		if err := c.sendV5Request(http.MethodGet, path, nil, &page); err != nil {
			return nil, err
		}
		out = append(out, page.Plans...)
		next := string(page.NextCursor)
		if !page.HasMore || next == "" {
			return out, nil
		}
		if seen[next] {
			return nil, fmt.Errorf("nmi plan pagination repeated cursor %s", next)
		}
		seen[next] = true
		cursor = next
	}
}
