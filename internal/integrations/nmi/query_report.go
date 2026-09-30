package nmi

import (
	"context"
	"encoding/xml"
	"fmt"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/billing/decline"
)

// TransactionReport is the Query API's transaction report (query.php,
// report_type=transaction), the one shape every reader parses.
type TransactionReport struct {
	XMLName       xml.Name           `xml:"nm_response"`
	Transactions  []QueryTransaction `xml:"transaction"`
	ErrorResponse string             `xml:"error_response"`
}

// QueryTransaction is one transaction of the report: the card it used and
// every action taken on it.
type QueryTransaction struct {
	TransactionID string `xml:"transaction_id"`
	// SubscriptionID is set only when NMI names the schedule; live accounts
	// usually do not.
	SubscriptionID  string `xml:"subscription_id"`
	OrderID         string `xml:"order_id"`
	CustomerID      string `xml:"customerid"`
	CustomerVaultID string `xml:"customer_vault_id"`
	Email           string `xml:"email"`
	Condition       string `xml:"condition"`
	Currency        string `xml:"currency"`
	// CCNumber is masked (first six and last four digits).
	CCNumber    string        `xml:"cc_number"`
	CCBin       string        `xml:"cc_bin"`
	CCType      string        `xml:"cc_type"`
	AVSResponse string        `xml:"avs_response"`
	CSCResponse string        `xml:"csc_response"`
	Actions     []QueryAction `xml:"action"`
}

// QueryAction is one action on a transaction: a sale, validate, refund, void.
type QueryAction struct {
	Amount     string `xml:"amount"`
	ActionType string `xml:"action_type"`
	Date       string `xml:"date"`
	Success    string `xml:"success"`
	// Source is how the action was made: api, recurring, virtual_terminal...
	Source       string `xml:"source"`
	ResponseCode string `xml:"response_code"`
	ResponseText string `xml:"response_text"`
	// ProcessorResponseCode/Text are the issuer's raw answer.
	ProcessorResponseCode string `xml:"processor_response_code"`
	ProcessorResponseText string `xml:"processor_response_text"`
	NetworkTokenUsed      string `xml:"network_token_used"`
}

// ParseTransactionReport parses a transaction report; an error_response is
// an error.
func ParseTransactionReport(raw string) (TransactionReport, error) {
	var report TransactionReport
	if err := xml.Unmarshal([]byte(raw), &report); err != nil {
		return report, fmt.Errorf("parse transaction query response: %w", err)
	}
	if msg := strings.TrimSpace(report.ErrorResponse); msg != "" {
		return report, fmt.Errorf("transaction query error_response: %s", msg)
	}
	return report, nil
}

// TransactionReport reads one page of the transaction report.
func (c *NMIClient) TransactionReport(ctx context.Context, filter QueryFilter) (TransactionReport, error) {
	raw, err := c.SearchTransactions(ctx, filter)
	if err != nil {
		return TransactionReport{}, err
	}
	return ParseTransactionReport(raw)
}

// Is reports whether the action is of kind ("sale", "validate", ...).
func (a QueryAction) Is(kind string) bool {
	return strings.EqualFold(strings.TrimSpace(a.ActionType), kind)
}

// Succeeded reports an approved action.
func (a QueryAction) Succeeded() bool { return strings.TrimSpace(a.Success) == "1" }

// At is the action's time; false when the report garbled it.
func (a QueryAction) At() (time.Time, bool) {
	at, err := time.ParseInLocation(queryAPITimeFormat, strings.TrimSpace(a.Date), time.UTC)
	return at, err == nil
}

// Authorization is the action that asked the issuer: the sale, validate or
// auth.
func (t QueryTransaction) Authorization() (QueryAction, bool) {
	for _, a := range t.Actions {
		if a.Is("sale") || a.Is("validate") || a.Is("auth") {
			return a, true
		}
	}
	return QueryAction{}, false
}

// Reversed reports whether the transaction's approved sale was taken back.
func (t QueryTransaction) Reversed() bool {
	actions := make([]TransactionAction, 0, len(t.Actions))
	for _, a := range t.Actions {
		actions = append(actions, TransactionAction{Type: a.ActionType, Success: a.Success, Amount: a.Amount})
	}
	return SaleReversed(t.Condition, actions)
}

// Evidence is what the report says about one of the transaction's actions:
// the answer and the card.
func (t QueryTransaction) Evidence(a QueryAction) decline.Evidence {
	bin := strings.TrimSpace(t.CCBin)
	if bin == "" {
		bin = binOf(t.CCNumber)
	}
	return decline.Evidence{
		Rail: "nmi", Code: strings.TrimSpace(a.ResponseCode), Text: strings.TrimSpace(a.ResponseText),
		AVS: strings.TrimSpace(t.AVSResponse), CVV: strings.TrimSpace(t.CSCResponse),
		IssuerCode: strings.TrimSpace(a.ProcessorResponseCode), IssuerText: strings.TrimSpace(a.ProcessorResponseText),
		CardBIN: bin, CardBrand: cardBrand(t.CCType, t.CCNumber), CardLast4: last4Of(t.CCNumber),
		NetworkToken: flagSet(a.NetworkTokenUsed),
	}
}

func binOf(masked string) string {
	digits := ""
	for _, r := range strings.TrimSpace(masked) {
		if r < '0' || r > '9' {
			break
		}
		digits += string(r)
	}
	if len(digits) >= 6 {
		return digits[:6]
	}
	return ""
}

func last4Of(masked string) string {
	masked = strings.TrimSpace(masked)
	if len(masked) < 4 {
		return ""
	}
	tail := masked[len(masked)-4:]
	for _, r := range tail {
		if r < '0' || r > '9' {
			return ""
		}
	}
	return tail
}

func cardBrand(ccType, masked string) string {
	switch brand := strings.ToLower(strings.TrimSpace(ccType)); brand {
	case "":
		return CardBrandFromMaskedPAN(masked)
	case "mc":
		return "mastercard"
	case "disc":
		return "discover"
	default:
		return brand
	}
}

func flagSet(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes":
		return true
	}
	return false
}
