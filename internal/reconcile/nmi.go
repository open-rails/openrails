package reconcile

import (
	"context"
	"encoding/xml"
	"fmt"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/integrations/nmi"
)

// nmiQueryClient is the slice of *nmi.NMIClient the fetcher uses — read-only
// by construction; none of the mutation paths are reachable from here.
// Subscriptions and the vault roster read the v5 JSON API; the transaction
// search stays on query.php (#663: v5 payments has no list/search).
type nmiQueryClient interface {
	ListSubscriptionsPage(cursor string, perPage int) (nmi.SubscriptionPage, error)
	GetSubscription(subscriptionID string) (nmi.V5Subscription, bool, error)
	ListCustomersPage(cursor string, perPage int, id string) (nmi.CustomerPage, error)
	SearchTransactions(filter nmi.QueryFilter) (string, error)
}

// NMIFetcher pulls NMI state:
// GET /v5/subscriptions (all live recurring subscriptions),
// query.php report_type=transaction (date-ranged search, declines included),
// GET /v5/customers (stored payment methods).
//
// Provider quirks (verified against the live sandbox 2026-06-11):
//   - NMI deletes cancelled subscriptions entirely (v5 GET answers 404), so
//     every listed subscription is live. Status is therefore inferred:
//     next_billing_date today-or-later => active; in the past => past_due
//     (NMI stopped advancing the charge date); unparseable => unknown.
//     RawStatus is left empty to record that NMI declared no status.
//   - The v5 subscription resource carries no email/name; identity fields are
//     joined from the customer roster via customer_vault_id (the vault pull
//     runs first for exactly this reason).
//   - Transactions do not carry the NMI subscription_id. Recurring rebills
//     inherit the subscription's order_id/ponumber (which OpenRails sets to a
//     local identifier at signup), preserved in Raw for phase-2 correlation.
//   - Declines surface as action_type=sale with success=0 (and condition
//     "failed"); response_text carries the decline reason.
//   - Chargebacks are NOT exposed by either read API => Chargebacks=false.
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

// nmiV5PageLimit is the per_page for v5 cursor pagination.
const nmiV5PageLimit = 100

func (f *NMIFetcher) Fetch(ctx context.Context, params FetchParams) (*RemoteSnapshot, error) {
	snap := &RemoteSnapshot{
		Provider:     ProviderNMI,
		FetchedAt:    time.Now().UTC(),
		Capabilities: f.Capabilities(),
	}

	// Vault first: the subscription mapping joins email/name from it.
	vault, identity, err := f.fetchVault(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("nmi customer roster: %w", err)
	}
	snap.VaultEntries = vault

	subs, err := f.fetchSubscriptions(ctx, params, identity)
	if err != nil {
		return nil, fmt.Errorf("nmi subscription roster: %w", err)
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

	return snap, nil
}

// --- GET /v5/subscriptions ---

// nmiVaultIdentity is the email/name joined onto subscriptions by vault id.
type nmiVaultIdentity struct {
	Email    string
	Username string
}

func (f *NMIFetcher) fetchSubscriptions(ctx context.Context, params FetchParams, identity map[string]nmiVaultIdentity) ([]RemoteSubscription, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var subs []nmi.V5Subscription
	if params.SubscriptionID != "" {
		sub, found, err := f.Client.GetSubscription(params.SubscriptionID)
		if err != nil {
			return nil, err
		}
		if found {
			subs = append(subs, sub)
		}
	} else {
		// Live-verified pagination contract: next_cursor is a STRING and empty
		// pages mid-stream are legal — stop only on has_more=false/no cursor.
		cursor := ""
		seenCursor := map[string]bool{}
		for {
			page, err := f.Client.ListSubscriptionsPage(cursor, nmiV5PageLimit)
			if err != nil {
				return nil, err
			}
			subs = append(subs, page.Subscriptions...)
			next := string(page.NextCursor)
			if !page.HasMore || next == "" {
				break
			}
			if seenCursor[next] {
				return nil, fmt.Errorf("nmi subscription pagination repeated cursor %s; refusing incomplete snapshot", next)
			}
			seenCursor[next] = true
			cursor = next
		}
	}

	today := time.Now().UTC().Truncate(24 * time.Hour)
	out := make([]RemoteSubscription, 0, len(subs))
	for _, s := range subs {
		vaultID := strings.TrimSpace(s.CustomerVaultID)
		sub := RemoteSubscription{
			RailSubscriptionID: strings.TrimSpace(s.ID),
			// NMI declares no per-subscription status (see fetcher doc);
			// RawStatus stays empty on purpose.
			Status:     SubscriptionStatusUnknown,
			CustomerID: vaultID,
			Currency:   "", // the subscription resource does not echo currency
			Raw:        rawJSON(map[string]any{"source": "nmi_recurring_v5", "subscription": s}),
		}
		if who, ok := identity[vaultID]; ok {
			sub.Email = who.Email
			sub.Username = who.Username
		}
		if s.Plan != nil {
			sub.PlanID = strings.TrimSpace(s.Plan.ID)
		}
		if cents, err := parseAmountCents(s.Amount); err == nil && cents > 0 {
			sub.AmountCents = cents
		} else if s.Plan != nil {
			if cents, err := parseAmountCents(s.Plan.PlanAmount); err == nil {
				sub.AmountCents = cents
			}
		}
		if next, err := parseNMIV5Date(s.NextBillingDate); err == nil {
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

// parseNMIV5Date accepts the date shapes v5 emits (ISO 8601 timestamp or bare
// date) and returns a UTC time.
func parseNMIV5Date(raw string) (time.Time, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return time.Time{}, fmt.Errorf("empty date")
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02"} {
		if ts, err := time.ParseInLocation(layout, trimmed, time.UTC); err == nil {
			return ts.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized v5 date %q", raw)
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

// --- GET /v5/customers ---

func (f *NMIFetcher) fetchVault(ctx context.Context, params FetchParams) ([]RemoteVaultEntry, map[string]nmiVaultIdentity, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	var customers []nmi.V5Customer
	cursor := ""
	seenCursor := map[string]bool{}
	for {
		page, err := f.Client.ListCustomersPage(cursor, nmiV5PageLimit, params.CustomerID)
		if err != nil {
			return nil, nil, err
		}
		customers = append(customers, page.Customers...)
		next := string(page.NextCursor)
		if !page.HasMore || next == "" {
			break
		}
		if seenCursor[next] {
			return nil, nil, fmt.Errorf("nmi customer pagination repeated cursor %s; refusing incomplete snapshot", next)
		}
		seenCursor[next] = true
		cursor = next
	}

	out := make([]RemoteVaultEntry, 0, len(customers))
	identity := make(map[string]nmiVaultIdentity, len(customers))
	for _, c := range customers {
		entry := RemoteVaultEntry{
			CustomerVaultID: strings.TrimSpace(c.ID),
			Raw:             rawJSON(map[string]any{"source": "nmi_customer_vault_v5", "customer": c}),
		}
		if billing := c.PrimaryBilling(); billing != nil {
			entry.CardLast4 = cardLast4(billing.PaymentDetails.CardNumber)
			entry.CardExpiry = strings.TrimSpace(billing.PaymentDetails.CardExp)
			entry.Email = strings.TrimSpace(billing.Email)
			identity[entry.CustomerVaultID] = nmiVaultIdentity{
				Email:    entry.Email,
				Username: strings.TrimSpace(strings.TrimSpace(billing.FirstName) + " " + strings.TrimSpace(billing.LastName)),
			}
		}
		out = append(out, entry)
	}
	return out, identity, nil
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
