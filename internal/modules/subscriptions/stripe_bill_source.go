package subscriptions

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/integrations/stripeapi"
)

// StripeBill is one finalized provider invoice, separate from the monetary
// receipts that settle it. Amounts are Stripe minor units, never catalog money.
type StripeBill struct {
	ID, SubscriptionID, CustomerID, Currency, Status, Reason       string
	Metadata                                                       map[string]string
	Total, AmountDue, AmountPaid, AmountRemaining, AmountOffStripe int64
	StartingBalance, PrePaymentCredit, PostPaymentCredit           int64
	Created, FinalizedAt, PaidAt, DueAt                            time.Time
	Lines                                                          []StripeBillLine
	Payments                                                       []StripeBillPayment
	Raw                                                            json.RawMessage
}

// StripeBillLine retains the accepted line amount independently of quantity:
// taxes, discounts and prorations must not be repriced through today's catalog.
type StripeBillLine struct {
	ID, PriceID, Description string
	Quantity                 *int
	Amount                   int64
	PeriodStart, PeriodEnd   time.Time
	Raw                      json.RawMessage
}

// StripeBillPayment is an actual invoice allocation with the verified Charge
// identity and amount. Non-cash settlement never produces a synthetic Charge.
type StripeBillPayment struct {
	ID, ChargeID, PaymentIntentID, Kind, Status, Currency string
	Amount                                                int64
	PaidAt                                                time.Time
}

// StripeBillReader reads provider invoice truth without initiating collection.
// The visitor bounds memory and commits each bill independently during recovery.
type StripeBillReader interface {
	VisitSubscriptionBills(context.Context, string, func(StripeBill) error) error
	ReadBill(context.Context, string) (StripeBill, error)
}

type stripeBillList struct {
	Data    []json.RawMessage `json:"data"`
	HasMore bool              `json:"has_more"`
}

type stripeBillEnvelope struct {
	ID           string          `json:"id"`
	Subscription json.RawMessage `json:"subscription"`
	Customer     json.RawMessage `json:"customer"`
	Parent       struct {
		SubscriptionDetails struct {
			Subscription json.RawMessage `json:"subscription"`
		} `json:"subscription_details"`
	} `json:"parent"`
	Metadata          map[string]string `json:"metadata"`
	Currency          string            `json:"currency"`
	Status            string            `json:"status"`
	Reason            string            `json:"billing_reason"`
	Total             int64             `json:"total"`
	AmountDue         int64             `json:"amount_due"`
	AmountPaid        int64             `json:"amount_paid"`
	AmountRemaining   int64             `json:"amount_remaining"`
	AmountOffStripe   int64             `json:"amount_paid_off_stripe"`
	StartingBalance   int64             `json:"starting_balance"`
	PrePaymentCredit  int64             `json:"pre_payment_credit_notes_amount"`
	PostPaymentCredit int64             `json:"post_payment_credit_notes_amount"`
	Created           int64             `json:"created"`
	DueAt             int64             `json:"due_date"`
	Transitions       struct {
		FinalizedAt int64 `json:"finalized_at"`
		PaidAt      int64 `json:"paid_at"`
	} `json:"status_transitions"`
	Lines    stripeBillList `json:"lines"`
	Payments stripeBillList `json:"payments"`
}

func stripeTime(v int64) time.Time {
	if v <= 0 {
		return time.Time{}
	}
	return time.Unix(v, 0).UTC()
}

func parseStripeBill(raw json.RawMessage) (StripeBill, stripeBillEnvelope, error) {
	var env stripeBillEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return StripeBill{}, env, fmt.Errorf("decode Stripe invoice: %w", err)
	}
	subID := stripeObjectID(env.Parent.SubscriptionDetails.Subscription)
	if subID == "" {
		subID = stripeObjectID(env.Subscription)
	}
	b := StripeBill{ID: env.ID, SubscriptionID: subID, CustomerID: stripeObjectID(env.Customer), Currency: strings.ToUpper(env.Currency),
		Status: env.Status, Reason: env.Reason, Metadata: env.Metadata, Total: env.Total, AmountDue: env.AmountDue,
		AmountPaid: env.AmountPaid, AmountRemaining: env.AmountRemaining, AmountOffStripe: env.AmountOffStripe,
		StartingBalance: env.StartingBalance, PrePaymentCredit: env.PrePaymentCredit, PostPaymentCredit: env.PostPaymentCredit,
		Created: stripeTime(env.Created), FinalizedAt: stripeTime(env.Transitions.FinalizedAt), PaidAt: stripeTime(env.Transitions.PaidAt), DueAt: stripeTime(env.DueAt), Raw: raw}
	if b.ID == "" || b.Currency == "" {
		return b, env, fmt.Errorf("Stripe invoice lacks identity or currency")
	}
	return b, env, nil
}

// VisitSubscriptionBills pages the subscription's invoice history. Reading only
// latest_invoice loses earlier invoices when webhooks arrive late or unordered.
func (p *HTTPStripeLivenessProber) VisitSubscriptionBills(ctx context.Context, subscriptionID string, visit func(StripeBill) error) error {
	if strings.TrimSpace(subscriptionID) == "" || visit == nil {
		return fmt.Errorf("Stripe invoice subscription and visitor are required")
	}
	return p.visitBillList(ctx, "/v1/invoices", url.Values{"subscription": {subscriptionID}}, func(raw json.RawMessage) error {
		var head struct{ ID, Status string }
		if err := json.Unmarshal(raw, &head); err != nil {
			return fmt.Errorf("decode Stripe invoice list entry: %w", err)
		}
		if head.Status == "draft" {
			return nil
		}
		bill, err := p.ReadBill(ctx, head.ID)
		if err != nil {
			return err
		}
		if bill.SubscriptionID != subscriptionID {
			return fmt.Errorf("Stripe invoice %s belongs to a different subscription", bill.ID)
		}
		return visit(bill)
	})
}

// ReadBill retrieves the named invoice and exhausts both nested lists. Its
// transaction identities come from actual provider payment objects, not the
// invoice id, paid flag or retry counter.
func (p *HTTPStripeLivenessProber) ReadBill(ctx context.Context, invoiceID string) (StripeBill, error) {
	if strings.TrimSpace(invoiceID) == "" {
		return StripeBill{}, fmt.Errorf("Stripe invoice ID is required")
	}
	raw, err := p.billGet(ctx, "/v1/invoices/"+url.PathEscape(invoiceID), url.Values{"expand[]": {"amount_paid_off_stripe"}})
	if err != nil {
		return StripeBill{}, err
	}
	bill, env, err := parseStripeBill(raw)
	if err != nil {
		return bill, err
	}
	if bill.ID != invoiceID {
		return bill, fmt.Errorf("Stripe returned a different invoice identity")
	}
	if bill.Status == "draft" {
		return bill, fmt.Errorf("Stripe invoice %s is not finalized", invoiceID)
	}
	appendLine := func(raw json.RawMessage) error {
		line, err := parseStripeBillLine(raw)
		if err != nil {
			return err
		}
		bill.Lines = append(bill.Lines, line)
		return nil
	}
	if env.Lines.HasMore {
		err = p.visitBillList(ctx, "/v1/invoices/"+url.PathEscape(invoiceID)+"/lines", nil, appendLine)
	} else {
		for _, line := range env.Lines.Data {
			if err = appendLine(line); err != nil {
				break
			}
		}
	}
	if err != nil {
		return bill, err
	}
	appendPayment := func(raw json.RawMessage) error {
		payment, err := p.readBillPayment(ctx, invoiceID, raw)
		if err != nil {
			return err
		}
		bill.Payments = append(bill.Payments, payment)
		return nil
	}
	if env.Payments.HasMore {
		err = p.visitBillList(ctx, "/v1/invoice_payments", url.Values{"invoice": {invoiceID}}, appendPayment)
	} else {
		for _, payment := range env.Payments.Data {
			if err = appendPayment(payment); err != nil {
				break
			}
		}
	}
	return bill, err
}

func parseStripeBillLine(raw json.RawMessage) (StripeBillLine, error) {
	var line struct {
		ID          string          `json:"id"`
		Description string          `json:"description"`
		Quantity    *int            `json:"quantity"`
		Amount      int64           `json:"amount"`
		Price       json.RawMessage `json:"price"`
		Pricing     struct {
			PriceDetails struct {
				Price json.RawMessage `json:"price"`
			} `json:"price_details"`
		} `json:"pricing"`
		Period struct{ Start, End int64 } `json:"period"`
	}
	if err := json.Unmarshal(raw, &line); err != nil {
		return StripeBillLine{}, fmt.Errorf("decode Stripe invoice line: %w", err)
	}
	priceID := stripeObjectID(line.Pricing.PriceDetails.Price)
	if priceID == "" {
		priceID = stripeObjectID(line.Price)
	}
	if line.ID == "" {
		return StripeBillLine{}, fmt.Errorf("Stripe invoice line has no identity")
	}
	if line.Quantity != nil && *line.Quantity <= 0 {
		return StripeBillLine{}, fmt.Errorf("Stripe invoice line %s has invalid quantity", line.ID)
	}
	return StripeBillLine{ID: line.ID, PriceID: priceID, Description: line.Description, Quantity: line.Quantity, Amount: line.Amount,
		PeriodStart: stripeTime(line.Period.Start), PeriodEnd: stripeTime(line.Period.End), Raw: raw}, nil
}

func (p *HTTPStripeLivenessProber) readBillPayment(ctx context.Context, invoiceID string, raw json.RawMessage) (StripeBillPayment, error) {
	var env struct {
		ID, Status, Currency string
		Invoice              json.RawMessage `json:"invoice"`
		AmountPaid           *int64          `json:"amount_paid"`
		Payment              struct {
			Type   string          `json:"type"`
			Charge json.RawMessage `json:"charge"`
			Intent json.RawMessage `json:"payment_intent"`
		} `json:"payment"`
		Transitions struct {
			PaidAt int64 `json:"paid_at"`
		} `json:"status_transitions"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return StripeBillPayment{}, fmt.Errorf("decode Stripe invoice payment: %w", err)
	}
	r := StripeBillPayment{ID: env.ID, Status: env.Status, Currency: strings.ToUpper(env.Currency), Kind: env.Payment.Type, PaidAt: stripeTime(env.Transitions.PaidAt)}
	if env.ID == "" || stripeObjectID(env.Invoice) != invoiceID {
		return r, fmt.Errorf("Stripe invoice payment identity mismatch")
	}
	if env.Status != "paid" {
		return r, nil
	}
	if env.AmountPaid == nil || *env.AmountPaid <= 0 {
		return r, fmt.Errorf("paid Stripe invoice payment %s has no positive amount", env.ID)
	}
	r.Amount = *env.AmountPaid
	r.ChargeID, r.PaymentIntentID = stripeObjectID(env.Payment.Charge), stripeObjectID(env.Payment.Intent)
	switch env.Payment.Type {
	case "payment_record":
		return r, nil // separately verified by invoice amount_paid_off_stripe
	case "payment_intent":
		if r.PaymentIntentID == "" {
			return r, fmt.Errorf("Stripe invoice payment lacks PaymentIntent")
		}
		body, err := p.billGet(ctx, "/v1/payment_intents/"+url.PathEscape(r.PaymentIntentID), nil)
		if err != nil {
			return r, err
		}
		var intent struct {
			ID, Status, Currency string
			AmountReceived       int64           `json:"amount_received"`
			LatestCharge         json.RawMessage `json:"latest_charge"`
		}
		if err := json.Unmarshal(body, &intent); err != nil {
			return r, fmt.Errorf("decode Stripe invoice PaymentIntent: %w", err)
		}
		if intent.ID != r.PaymentIntentID || intent.Status != "succeeded" || !strings.EqualFold(intent.Currency, r.Currency) || intent.AmountReceived != r.Amount {
			return r, fmt.Errorf("Stripe invoice payment %s cannot be represented as one complete money movement", r.ID)
		}
		r.ChargeID = stripeObjectID(intent.LatestCharge)
	case "charge":
	default:
		return r, fmt.Errorf("Stripe invoice payment %s has unsupported source %q", r.ID, env.Payment.Type)
	}
	if r.ChargeID == "" {
		return r, fmt.Errorf("Stripe invoice payment %s has no verified Charge", r.ID)
	}
	body, err := p.billGet(ctx, "/v1/charges/"+url.PathEscape(r.ChargeID), nil)
	if err != nil {
		return r, err
	}
	var charge struct {
		ID, Status, Currency string
		Captured, Paid       bool
		Amount               int64           `json:"amount"`
		AmountCaptured       int64           `json:"amount_captured"`
		Intent               json.RawMessage `json:"payment_intent"`
	}
	if err := json.Unmarshal(body, &charge); err != nil {
		return r, fmt.Errorf("decode Stripe invoice Charge: %w", err)
	}
	if charge.ID != r.ChargeID || charge.Status != "succeeded" || !charge.Paid || !charge.Captured || !strings.EqualFold(charge.Currency, r.Currency) || charge.AmountCaptured != r.Amount {
		return r, fmt.Errorf("Stripe invoice payment %s does not match a captured Charge", r.ID)
	}
	if r.PaymentIntentID != "" && stripeObjectID(charge.Intent) != r.PaymentIntentID {
		return r, fmt.Errorf("Stripe invoice payment %s Charge belongs to another PaymentIntent", r.ID)
	}
	return r, nil
}

func (p *HTTPStripeLivenessProber) visitBillList(ctx context.Context, path string, query url.Values, visit func(json.RawMessage) error) error {
	if query == nil {
		query = url.Values{}
	}
	query.Set("limit", "100")
	previous := ""
	for {
		body, err := p.billGet(ctx, path, query)
		if err != nil {
			return err
		}
		var page stripeBillList
		if err := json.Unmarshal(body, &page); err != nil {
			return fmt.Errorf("decode Stripe invoice list: %w", err)
		}
		for _, raw := range page.Data {
			if err := visit(raw); err != nil {
				return err
			}
		}
		if !page.HasMore {
			return nil
		}
		if len(page.Data) == 0 {
			return fmt.Errorf("Stripe invoice list is incomplete")
		}
		next := stripeObjectID(page.Data[len(page.Data)-1])
		if next == "" || next == previous {
			return fmt.Errorf("Stripe invoice list cursor did not advance")
		}
		previous = next
		query.Set("starting_after", next)
	}
}

func (p *HTTPStripeLivenessProber) billGet(ctx context.Context, path string, query url.Values) ([]byte, error) {
	if strings.TrimSpace(p.SecretKey) == "" {
		return nil, fmt.Errorf("Stripe secret key is required")
	}
	base := strings.TrimRight(p.BaseURL, "/")
	if base == "" {
		base = "https://api.stripe.com"
	}
	if len(query) != 0 {
		path += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+p.SecretKey)
	client := p.HTTPClient
	if client == nil {
		client = stripeapi.ReadOnlyClient(30 * time.Second)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("read Stripe billing evidence: %w", err)
	}
	defer resp.Body.Close()
	const limit = 8 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read Stripe billing response: %w", err)
	}
	if len(body) > limit {
		return nil, fmt.Errorf("Stripe billing response exceeds %d bytes", limit)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("read Stripe billing evidence: HTTP %d", resp.StatusCode)
	}
	return body, nil
}
