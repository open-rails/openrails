package nmi

import (
	"context"
	"encoding/xml"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/booktag"
	"github.com/open-rails/openrails/internal/decline"
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
	TransactionID         string `xml:"transaction_id"`
	OriginalTransactionID string `xml:"original_transaction_id"`
	// SubscriptionID is set only when NMI names the schedule; live accounts
	// usually do not.
	SubscriptionID   string `xml:"subscription_id"`
	OrderID          string `xml:"order_id"`
	OrderDescription string `xml:"order_description"`
	CustomerID       string `xml:"customerid"`
	CustomerVaultID  string `xml:"customer_vault_id"`
	Email            string `xml:"email"`
	// MerchantDefinedFields are the merchant-defined fields, by id.
	MerchantDefinedFields []MerchantDefinedField `xml:"merchant_defined_field"`
	Condition             string                 `xml:"condition"`
	Currency              string                 `xml:"currency"`
	// CCNumber is masked (first six and last four digits).
	CCNumber    string `xml:"cc_number"`
	CCBin       string `xml:"cc_bin"`
	CCType      string `xml:"cc_type"`
	AVSResponse string `xml:"avs_response"`
	CSCResponse string `xml:"csc_response"`
	// PartialPaymentBalance is what a partially approved sale left unpaid.
	PartialPaymentBalance string        `xml:"partial_payment_balance"`
	Actions               []QueryAction `xml:"action"`
}

// MerchantDefinedField is one merchant-defined field of a query report.
type MerchantDefinedField struct {
	ID    string `xml:"id,attr"`
	Value string `xml:",chardata"`
}

// BookID is the billing book the transaction carries, "" for none.
func (t QueryTransaction) BookID() string {
	for _, f := range t.MerchantDefinedFields {
		if strings.TrimSpace(f.ID) == booktag.NMIFieldID {
			return strings.TrimSpace(f.Value)
		}
	}
	return ""
}

// Vault is the customer vault the transaction charged: the report names it
// customerid (customer_vault_id on older reports).
func (t QueryTransaction) Vault() string {
	if v := strings.TrimSpace(t.CustomerVaultID); v != "" {
		return v
	}
	return strings.TrimSpace(t.CustomerID)
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
		return report, queryError("transaction query", msg)
	}
	return report, nil
}

// PageTransactions reads every page of filter's transaction report, oldest
// first (result_order=standard), handing each to page as it arrives; a
// transaction may be handed twice. NMI selects by last change, which only
// moves later. A report with an end_date only loses transactions while it is
// read, and those after a lost one shift a place toward the first page: its
// pages are read last to first, the ones still to read below, so a
// transaction that stays is never skipped at a page boundary. One with no end
// is read so through asOf, the read's own start on NMI's clock, then the
// short tail after it first to last, which only gains transactions, each at
// its own place. UNCONFIRMED: NMI's guide orders a report "oldest to newest"
// without naming the date; for the tail, the transaction's own, which a
// change does not move, is assumed. A zero asOf reads all of an open report
// first to last.
func (c *NMIClient) PageTransactions(ctx context.Context, filter QueryFilter, asOf time.Time, page func([]QueryTransaction) error) error {
	if filter.EndDate == "" && !asOf.IsZero() {
		end := asOf.UTC().Truncate(time.Second)
		bounded, tail := filter, filter
		bounded.EndDate = end.Format(QueryTimeFormat)
		tail.StartDate = end.Add(time.Second).Format(QueryTimeFormat)
		if start, err := time.Parse(QueryTimeFormat, filter.StartDate); err != nil || !start.After(end) {
			if err := c.PageTransactions(ctx, bounded, time.Time{}, page); err != nil {
				return err
			}
		}
		return c.PageTransactions(ctx, tail, time.Time{}, page)
	}
	filter.ResultLimit, filter.ResultOrder = QueryPageLimit, "standard"
	firsts := map[string]int{}
	read := func(n int) (int, error) {
		filter.PageNumber = n
		report, err := c.TransactionReport(ctx, filter)
		if err != nil {
			return 0, fmt.Errorf("nmi transactions, page %d: %w", n, err)
		}
		if len(report.Transactions) > 0 {
			first := strings.TrimSpace(report.Transactions[0].TransactionID)
			if at, seen := firsts[first]; seen && at != n {
				return 0, fmt.Errorf("nmi transaction pagination repeated page starting at transaction_id=%s; refusing incomplete snapshot", first)
			}
			firsts[first] = n
		}
		return len(report.Transactions), page(report.Transactions)
	}
	got, err := read(0)
	if err != nil || got < QueryPageLimit {
		return err
	}
	if filter.EndDate == "" {
		for n := 1; ; n++ {
			if got, err = read(n); err != nil || got < QueryPageLimit {
				return err
			}
		}
	}
	last, err := lastPage(read)
	if err != nil {
		return err
	}
	for n := last; n >= 0; n-- {
		if _, err := read(n); err != nil {
			return err
		}
	}
	return nil
}

// lastPage finds the last page holding a transaction, page 0 being full:
// doubling to a page that is not, then halving back to the last.
func lastPage(read func(int) (int, error)) (int, error) {
	full, probe := 0, 1
	for {
		got, err := read(probe)
		switch {
		case err != nil:
			return 0, err
		case got == QueryPageLimit:
			full, probe = probe, 2*probe
			continue
		case got > 0:
			return probe, nil
		}
		break
	}
	for empty := probe; empty-full > 1; {
		mid := full + (empty-full)/2
		got, err := read(mid)
		switch {
		case err != nil:
			return 0, err
		case got == 0:
			empty = mid
		case got < QueryPageLimit:
			return mid, nil
		default:
			full = mid
		}
	}
	return full, nil
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

// DefinitiveDecline distinguishes a completed refusal from errors, unreadable
// outcomes, and transactions the gateway can still execute.
func (a QueryAction) DefinitiveDecline(condition string) (int, bool) {
	if strings.TrimSpace(a.Success) != "0" {
		return 0, false
	}
	state := strings.ToLower(strings.TrimSpace(condition))
	code, err := strconv.Atoi(strings.TrimSpace(a.ResponseCode))
	return code, err == nil && (code >= 200 && code < 300 || a.Is("sale") && IsSaleRefusalCode(code) && !duplicateRefusal(code, a.ResponseText)) &&
		(state == "" || state == "failed" || state == "complete")
}

// At is the action's time; false when the report garbled it.
func (a QueryAction) At() (time.Time, bool) {
	at, err := time.ParseInLocation(QueryTimeFormat, strings.TrimSpace(a.Date), time.UTC)
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
		bin = BINOf(t.CCNumber)
	}
	return decline.Evidence{
		Rail: "nmi", Code: strings.TrimSpace(a.ResponseCode), Text: strings.TrimSpace(a.ResponseText),
		AVS: strings.TrimSpace(t.AVSResponse), CVV: strings.TrimSpace(t.CSCResponse),
		IssuerCode: strings.TrimSpace(a.ProcessorResponseCode), IssuerText: strings.TrimSpace(a.ProcessorResponseText),
		CardBIN: bin, CardBrand: cardBrand(t.CCType, t.CCNumber), CardLast4: last4Of(t.CCNumber),
		NetworkToken: flagSet(a.NetworkTokenUsed),
	}
}

// TransactionCard is the brand and last four of the card one transaction
// charged or tried, as the gateway's own report shows them.
func (c *NMIClient) TransactionCard(ctx context.Context, transactionID string) (brand, last4 string, err error) {
	report, err := c.scoped().TransactionReport(ctx, QueryFilter{TransactionID: transactionID})
	if err != nil {
		return "", "", err
	}
	if len(report.Transactions) != 1 || report.Transactions[0].TransactionID != transactionID {
		return "", "", fmt.Errorf("transaction %s lookup is not exact", transactionID)
	}
	txn := report.Transactions[0]
	return cardBrand(txn.CCType, txn.CCNumber), last4Of(txn.CCNumber), nil
}

// IssuerAnswer is what the Query API reports of one transaction that a
// Direct Post answer leaves out: the issuer's raw code and text, and the
// card's BIN.
func (c *NMIClient) IssuerAnswer(ctx context.Context, transactionID string) (decline.Evidence, error) {
	report, err := c.scoped().TransactionReport(ctx, QueryFilter{TransactionID: transactionID})
	if err != nil {
		return decline.Evidence{}, err
	}
	if len(report.Transactions) != 1 || strings.TrimSpace(report.Transactions[0].TransactionID) != transactionID {
		return decline.Evidence{}, fmt.Errorf("transaction %s lookup is not exact", transactionID)
	}
	txn := report.Transactions[0]
	action, ok := txn.Authorization()
	if !ok {
		return decline.Evidence{}, fmt.Errorf("transaction %s has no authorization", transactionID)
	}
	e := txn.Evidence(action)
	return decline.Evidence{IssuerCode: e.IssuerCode, IssuerText: e.IssuerText, CardBIN: e.CardBIN}, nil
}

// BINOf is the leading six digits of a masked card number; empty when the
// mask hides them.
func BINOf(masked string) string {
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
