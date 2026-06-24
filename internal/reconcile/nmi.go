package reconcile

import (
	"context"
	"encoding/xml"
	"fmt"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/integrations/nmi"
)

// nmiQueryClient is the slice of *nmi.NMIClient the fetcher uses. Every method
// is backed by the NMI Query API (query.php via sendQueryRequest) — read-only
// by construction; none of the direct-post mutation paths are reachable from
// here.
type nmiQueryClient interface {
	QueryRecurringSubscriptions(params nmi.RecurringQueryParams) (string, error)
	SearchTransactions(filter nmi.QueryFilter) (string, error)
	GetCustomerVaultData(customerVaultID string) (string, error)
}

// NMIFetcher pulls NMI state via the Query API:
// report_type=recurring (all live recurring subscriptions),
// report_type=transaction (date-ranged transaction search, declines included),
// report_type=customer_vault (stored payment methods).
//
// Provider quirks (verified against the live sandbox 2026-06-11):
//   - The recurring report has NO status field: NMI deletes cancelled
//     subscriptions from the report entirely, so every listed subscription is
//     live. Status is therefore inferred: next_charge_date today-or-later =>
//     active; in the past => past_due (NMI stopped advancing the charge date);
//     unparseable => unknown. RawStatus is left empty to record that NMI
//     declared no status.
//   - Transactions do not carry the NMI subscription_id. Recurring rebills
//     inherit the subscription's order_id/ponumber (which OpenRails sets to a
//     local identifier at signup), preserved in Raw for phase-2 correlation.
//   - Declines surface as action_type=sale with success=0 (and condition
//     "failed"); response_text carries the decline reason.
//   - Chargebacks are NOT exposed by the Query API => Chargebacks=false.
type NMIFetcher struct {
	Client nmiQueryClient
}

// NewNMIFetcher builds a fetcher over an existing NMI client (query API only).
func NewNMIFetcher(client *nmi.NMIClient) *NMIFetcher {
	return &NMIFetcher{Client: client}
}

func (f *NMIFetcher) Name() string { return string(ProviderNMI) }

func (f *NMIFetcher) Capabilities() Capabilities {
	return Capabilities{
		Subscriptions: true,
		Transactions:  true,
		Refunds:       true,
		Chargebacks:   false,
		Vault:         true,
	}
}

// nmiQueryTimeFormat is the Query API start_date/end_date (and action <date>)
// timestamp layout: YYYYMMDDhhmmss.
const nmiQueryTimeFormat = "20060102150405"
const nmiQueryPageLimit = 1000

func (f *NMIFetcher) Fetch(ctx context.Context, params FetchParams) (*RemoteSnapshot, error) {
	snap := &RemoteSnapshot{
		Provider:     ProviderNMI,
		FetchedAt:    time.Now().UTC(),
		Capabilities: f.Capabilities(),
	}

	subs, err := f.fetchSubscriptions(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("nmi recurring query: %w", err)
	}
	snap.Subscriptions = subs
	if params.SubscriptionID == "" {
		snap.Coverage.SubscriptionsExhaustive = true
	}

	txns, err := f.fetchTransactions(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("nmi transaction query: %w", err)
	}
	snap.Transactions = txns
	snap.Coverage.TransactionsExhaustive = true
	snap.Coverage.TransactionsPaginatedComplete = true
	snap.Coverage.TransactionWindowSince = timePtrIfSet(params.Since)
	snap.Coverage.TransactionWindowUntil = timePtrIfSet(params.Until)

	vault, err := f.fetchVault(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("nmi customer_vault query: %w", err)
	}
	snap.VaultEntries = vault

	return snap, nil
}

// --- report_type=recurring ---

type nmiRecurringResponse struct {
	XMLName       xml.Name             `xml:"nm_response"`
	Subscriptions []nmiSubscriptionXML `xml:"subscription"`
	ErrorResponse string               `xml:"error_response"`
}

type nmiSubscriptionXML struct {
	SubscriptionID    string     `xml:"subscription_id"`
	Plan              nmiPlanXML `xml:"plan"`
	NextChargeDate    string     `xml:"next_charge_date"`
	CompletedPayments string     `xml:"completed_payments"`
	AttemptedPayments string     `xml:"attempted_payments"`
	RemainingPayments string     `xml:"remaining_payments"`
	OrderID           string     `xml:"orderid"`
	PONumber          string     `xml:"ponumber"`
	FirstName         string     `xml:"first_name"`
	LastName          string     `xml:"last_name"`
	Email             string     `xml:"email"`
	CustomerVaultID   string     `xml:"customer_vault_id"`
	CCNumber          string     `xml:"cc_number"`
	CCExp             string     `xml:"cc_exp"`
	InnerXML          string     `xml:",innerxml" json:"-"`
}

type nmiPlanXML struct {
	PlanID       string `xml:"plan_id"`
	PlanName     string `xml:"plan_name"`
	PlanAmount   string `xml:"plan_amount"`
	DayFrequency string `xml:"day_frequency"`
	PlanPayments string `xml:"plan_payments"`
}

func (f *NMIFetcher) fetchSubscriptions(ctx context.Context, params FetchParams) ([]RemoteSubscription, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var pages []nmiSubscriptionXML
	if params.SubscriptionID != "" {
		raw, err := f.Client.QueryRecurringSubscriptions(nmi.RecurringQueryParams{
			SubscriptionID: params.SubscriptionID,
			// PageNumber -1 keeps the existing client from emitting page_number=0.
			PageNumber: -1,
		})
		if err != nil {
			return nil, err
		}
		parsed, err := parseNMIRecurringPage(raw)
		if err != nil {
			return nil, err
		}
		pages = parsed.Subscriptions
	} else {
		seenFirst := map[string]bool{}
		for page := 1; ; page++ {
			raw, err := f.Client.QueryRecurringSubscriptions(nmi.RecurringQueryParams{
				ResultLimit: nmiQueryPageLimit,
				PageNumber:  page,
				ResultOrder: "asc",
			})
			if err != nil {
				return nil, err
			}
			parsed, err := parseNMIRecurringPage(raw)
			if err != nil {
				return nil, err
			}
			if len(parsed.Subscriptions) == 0 {
				break
			}
			first := strings.TrimSpace(parsed.Subscriptions[0].SubscriptionID)
			if seenFirst[first] {
				return nil, fmt.Errorf("nmi recurring pagination repeated page starting at subscription_id=%s; refusing incomplete snapshot", first)
			}
			seenFirst[first] = true
			pages = append(pages, parsed.Subscriptions...)
			if len(parsed.Subscriptions) < nmiQueryPageLimit {
				break
			}
		}
	}
	today := time.Now().UTC().Truncate(24 * time.Hour)
	out := make([]RemoteSubscription, 0, len(pages))
	for _, s := range pages {
		sub := RemoteSubscription{
			RailSubscriptionID: strings.TrimSpace(s.SubscriptionID),
			// NMI declares no per-subscription status (see fetcher doc);
			// RawStatus stays empty on purpose.
			Status:     SubscriptionStatusUnknown,
			CustomerID: strings.TrimSpace(s.CustomerVaultID),
			Email:      strings.TrimSpace(s.Email),
			Username:   strings.TrimSpace(strings.TrimSpace(s.FirstName + " " + s.LastName)),
			PlanID:     strings.TrimSpace(s.Plan.PlanID),
			Currency:   "", // the recurring report does not echo currency
			Raw:        rawJSON(map[string]string{"source": "nmi_recurring", "xml": s.InnerXML}),
		}
		if cents, err := parseAmountCents(s.Plan.PlanAmount); err == nil {
			sub.AmountCents = cents
		}
		if next, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(s.NextChargeDate), time.UTC); err == nil {
			sub.NextBillingAt = &next
			if next.Before(today) {
				sub.Status = SubscriptionStatusPastDue
			} else {
				sub.Status = SubscriptionStatusActive
			}
		}
		out = append(out, sub)
	}
	return out, nil
}

func parseNMIRecurringPage(raw string) (nmiRecurringResponse, error) {
	var parsed nmiRecurringResponse
	if err := xml.Unmarshal([]byte(raw), &parsed); err != nil {
		return parsed, fmt.Errorf("parse recurring XML: %w", err)
	}
	if msg := strings.TrimSpace(parsed.ErrorResponse); msg != "" {
		return parsed, fmt.Errorf("nmi error_response: %s", msg)
	}
	return parsed, nil
}

// --- report_type=transaction ---

type nmiTransactionResponse struct {
	XMLName       xml.Name            `xml:"nm_response"`
	Transactions  []nmiTransactionXML `xml:"transaction"`
	ErrorResponse string              `xml:"error_response"`
}

type nmiTransactionXML struct {
	TransactionID   string         `xml:"transaction_id"`
	Condition       string         `xml:"condition"`
	OrderID         string         `xml:"order_id"`
	CustomerID      string         `xml:"customerid"`
	CustomerVaultID string         `xml:"customer_vault_id"`
	Email           string         `xml:"email"`
	Currency        string         `xml:"currency"`
	Actions         []nmiActionXML `xml:"action"`
}

type nmiActionXML struct {
	Amount       string `xml:"amount"`
	ActionType   string `xml:"action_type"`
	Date         string `xml:"date"`
	Success      string `xml:"success"`
	Source       string `xml:"source"`
	ResponseText string `xml:"response_text"`
	ResponseCode string `xml:"response_code"`
}

// fetchTransactions runs a date-ranged transaction search. No condition or
// action_type filter is sent, so NMI returns transactions in EVERY condition
// — including failed/declined ones, which the dunning-forensics report needs.
func (f *NMIFetcher) fetchTransactions(ctx context.Context, params FetchParams) ([]RemoteTransaction, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	filter := nmi.QueryFilter{}
	if !params.Since.IsZero() {
		filter.StartDate = params.Since.UTC().Format(nmiQueryTimeFormat)
	}
	if !params.Until.IsZero() {
		filter.EndDate = params.Until.UTC().Format(nmiQueryTimeFormat)
	}
	filter.ResultLimit = nmiQueryPageLimit
	var out []RemoteTransaction
	seenFirst := map[string]bool{}
	for page := 1; ; page++ {
		filter.PageNumber = page
		raw, err := f.Client.SearchTransactions(filter)
		if err != nil {
			return nil, err
		}
		parsed, err := parseNMITransactionPage(raw)
		if err != nil {
			return nil, err
		}
		if len(parsed.Transactions) == 0 {
			break
		}
		first := strings.TrimSpace(parsed.Transactions[0].TransactionID)
		if seenFirst[first] {
			return nil, fmt.Errorf("nmi transaction pagination repeated page starting at transaction_id=%s; refusing incomplete snapshot", first)
		}
		seenFirst[first] = true
		for _, t := range parsed.Transactions {
			out = append(out, normalizeNMITransaction(t)...)
		}
		if len(parsed.Transactions) < nmiQueryPageLimit {
			break
		}
	}
	return out, nil
}

func parseNMITransactionPage(raw string) (nmiTransactionResponse, error) {
	var parsed nmiTransactionResponse
	if err := xml.Unmarshal([]byte(raw), &parsed); err != nil {
		return parsed, fmt.Errorf("parse transaction XML: %w", err)
	}
	if msg := strings.TrimSpace(parsed.ErrorResponse); msg != "" {
		return parsed, fmt.Errorf("nmi error_response: %s", msg)
	}
	return parsed, nil
}

func normalizeNMITransaction(t nmiTransactionXML) []RemoteTransaction {
	var out []RemoteTransaction
	for _, a := range t.Actions {
		txnType, ok := normalizeNMIAction(strings.TrimSpace(strings.ToLower(a.ActionType)))
		if !ok {
			// settle/check/void/etc. — settlement plumbing, not a
			// charge-level event the diff engine consumes.
			continue
		}
		success := strings.TrimSpace(a.Success) == "1"
		if txnType == TransactionTypeSale && !success {
			txnType = TransactionTypeDecline
		}
		txn := RemoteTransaction{
			TransactionID: strings.TrimSpace(t.TransactionID),
			// NMI does not echo the recurring subscription_id on
			// transactions; order_id correlation lives in Raw.
			SubscriptionID: "",
			Type:           txnType,
			Success:        success,
			Currency:       strings.TrimSpace(t.Currency),
			Raw: rawJSON(map[string]any{
				"source":            "nmi_transaction",
				"condition":         strings.TrimSpace(t.Condition),
				"order_id":          strings.TrimSpace(t.OrderID),
				"customerid":        strings.TrimSpace(t.CustomerID),
				"customer_vault_id": strings.TrimSpace(t.CustomerVaultID),
				"email":             strings.TrimSpace(t.Email),
				"action":            a,
			}),
		}
		if cents, err := parseAmountCents(a.Amount); err == nil {
			txn.AmountCents = cents
		}
		if ts, err := time.ParseInLocation(nmiQueryTimeFormat, strings.TrimSpace(a.Date), time.UTC); err == nil {
			txn.OccurredAt = ts
		}
		if !success {
			txn.DeclineReason = strings.TrimSpace(a.ResponseText)
		}
		out = append(out, txn)
	}
	return out
}

// normalizeNMIAction maps NMI action_type values onto the normalized
// TransactionType. Returns ok=false for action types that are settlement
// plumbing rather than charge events.
func normalizeNMIAction(actionType string) (TransactionType, bool) {
	switch actionType {
	case "sale":
		return TransactionTypeSale, true
	case "auth":
		return TransactionTypeAuth, true
	case "refund", "credit":
		return TransactionTypeRefund, true
	default:
		return "", false
	}
}

// --- report_type=customer_vault ---

type nmiVaultResponse struct {
	XMLName       xml.Name      `xml:"nm_response"`
	CustomerVault nmiVaultInner `xml:"customer_vault"`
	ErrorResponse string        `xml:"error_response"`
}

type nmiVaultInner struct {
	Customers []nmiVaultCustomerXML `xml:"customer"`
}

type nmiVaultCustomerXML struct {
	CustomerVaultID string `xml:"customer_vault_id"`
	Email           string `xml:"email"`
	CCNumber        string `xml:"cc_number"`
	CCExp           string `xml:"cc_exp"`
	CCType          string `xml:"cc_type"`
	Created         string `xml:"created"`
	Updated         string `xml:"updated"`
	InnerXML        string `xml:",innerxml" json:"-"`
}

func (f *NMIFetcher) fetchVault(ctx context.Context, params FetchParams) ([]RemoteVaultEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	raw, err := f.Client.GetCustomerVaultData(params.CustomerID)
	if err != nil {
		return nil, err
	}

	var parsed nmiVaultResponse
	if err := xml.Unmarshal([]byte(raw), &parsed); err != nil {
		return nil, fmt.Errorf("parse customer_vault XML: %w", err)
	}
	if msg := strings.TrimSpace(parsed.ErrorResponse); msg != "" {
		return nil, fmt.Errorf("nmi error_response: %s", msg)
	}

	out := make([]RemoteVaultEntry, 0, len(parsed.CustomerVault.Customers))
	for _, c := range parsed.CustomerVault.Customers {
		out = append(out, RemoteVaultEntry{
			CustomerVaultID: strings.TrimSpace(c.CustomerVaultID),
			CardLast4:       cardLast4(c.CCNumber),
			CardExpiry:      strings.TrimSpace(c.CCExp),
			Email:           strings.TrimSpace(c.Email),
			Raw:             rawJSON(map[string]string{"source": "nmi_customer_vault", "xml": c.InnerXML}),
		})
	}
	return out, nil
}

// cardLast4 extracts the trailing four digits from a masked PAN such as
// "4xxxxxxxxxxx1111".
func cardLast4(masked string) string {
	masked = strings.TrimSpace(masked)
	if len(masked) < 4 {
		return ""
	}
	last4 := masked[len(masked)-4:]
	for _, r := range last4 {
		if r < '0' || r > '9' {
			return ""
		}
	}
	return last4
}
