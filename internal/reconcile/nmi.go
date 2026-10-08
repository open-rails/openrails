package reconcile

import (
	"context"
	"encoding/json"
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
	ListSubscriptionsPage(ctx context.Context, cursor string, perPage int) (nmi.SubscriptionPage, error)
	GetSubscription(ctx context.Context, subscriptionID string) (nmi.V5Subscription, bool, error)
	ListCustomersPage(ctx context.Context, cursor string, perPage int, id string) (nmi.CustomerPage, error)
	GetCustomer(ctx context.Context, id string) (nmi.V5Customer, bool, error)
	SearchTransactions(ctx context.Context, filter nmi.QueryFilter) (string, error)
}

// NMIFetcher pulls NMI state:
// GET /v5/subscriptions (all live recurring subscriptions),
// query.php report_type=transaction (date-ranged search, declines included),
// GET /v5/customers (stored payment methods).
//
// Provider quirks (verified against the live sandbox 2026-06-11):
//   - NMI deletes canceled subscriptions entirely (v5 GET answers 404), so
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

const nmiQueryPageLimit = 1000

// nmiV5PageLimit is the per_page for v5 cursor pagination.
const nmiV5PageLimit = 100

func (f *NMIFetcher) Fetch(ctx context.Context, params FetchParams) (*RemoteSnapshot, error) {
	now := fetchObservedAt(params)
	snap := &RemoteSnapshot{
		Provider:     ProviderNMI,
		FetchedAt:    now,
		Capabilities: f.Capabilities(),
	}

	// Vault first: the subscription mapping joins email/name from it.
	vault, identity, err := f.fetchPaymentMethods(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("nmi customer roster: %w", err)
	}
	snap.PaymentMethods = vault

	subs, err := f.fetchSubscriptions(ctx, params, identity, now)
	if err != nil {
		return nil, fmt.Errorf("nmi subscription roster: %w", err)
	}
	snap.Subscriptions = subs
	// #842: "exhaustive" is an ABSENCE PROOF — it authorizes cancelling every
	// local subscription missing from this list. A successful-but-empty
	// GET /v5/subscriptions is indistinguishable from a complete roster of an
	// empty gateway: a misdeclared account_id, a credential rotated onto a
	// sibling sub-account, or an incident returning an empty first page with
	// has_more=false all look exactly like "this merchant has no subscribers".
	// So a roster only proves absence when it actually returned rows.
	if params.SubscriptionID == "" && len(subs) > 0 {
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

// nmiCustomerIdentity is the email/name joined onto subscriptions by vault id.
type nmiCustomerIdentity struct {
	Email    string
	Username string
}

func (f *NMIFetcher) fetchSubscriptions(ctx context.Context, params FetchParams, identity map[string]nmiCustomerIdentity, now time.Time) ([]RemoteSubscription, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var subs []nmi.V5Subscription
	if params.SubscriptionID != "" {
		sub, found, err := f.Client.GetSubscription(ctx, params.SubscriptionID)
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
			page, err := f.Client.ListSubscriptionsPage(ctx, cursor, nmiV5PageLimit)
			if err != nil {
				return nil, err
			}
			subs = append(subs, page.Subscriptions...)
			next := string(page.NextCursor)
			if !page.HasMore {
				break
			}
			if next == "" {
				return nil, fmt.Errorf("nmi subscription pagination omitted the next cursor")
			}
			if seenCursor[next] {
				return nil, fmt.Errorf("nmi subscription pagination repeated cursor %s; refusing incomplete snapshot", next)
			}
			seenCursor[next] = true
			cursor = next
		}
	}

	today := now.Truncate(24 * time.Hour)
	out := make([]RemoteSubscription, 0, len(subs))
	for _, s := range subs {
		if strings.TrimSpace(s.ID) == "" {
			return nil, fmt.Errorf("nmi subscription roster row has no identity")
		}
		railCustomerRef := strings.TrimSpace(s.CustomerVaultID)
		sub := RemoteSubscription{
			RailSubscriptionID: strings.TrimSpace(s.ID),
			// NMI declares no per-subscription status (see fetcher doc);
			// RawStatus stays empty on purpose.
			Status:     SubscriptionStatusUnknown,
			CustomerID: railCustomerRef,
			Currency:   "", // the subscription resource does not echo currency
			Raw:        rawJSON(map[string]any{"source": "nmi_recurring_v5", "subscription": s}),
		}
		sub.Paused = nmiFlag(s.PausedSubscription)
		if who, ok := identity[railCustomerRef]; ok {
			sub.Email = who.Email
			sub.Username = who.Username
		}
		if s.Plan != nil {
			sub.PlanID = strings.TrimSpace(s.Plan.ID)
		}
		// This resource has no currency. Keep its decimal in Raw and compare
		// against the uniquely bound local price later; do not guess USD cents.
		if next, err := parseNMIV5Date(s.NextBillingDate); err == nil {
			sub.NextBillingAt = &next
			if next.Before(today) {
				sub.Status = SubscriptionStatusPastDue
			} else {
				sub.Status = SubscriptionStatusActive
			}
		}
		if sub.Paused {
			// A paused schedule bills nothing and is not dead: neither live
			// nor terminal, so status comparison skips it and the drift check
			// reports it.
			sub.Status, sub.RawStatus = SubscriptionStatusUnknown, "paused"
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

// fetchTransactions runs a date-ranged transaction search. No condition or
// action_type filter is sent, so NMI returns transactions in EVERY condition
// — including failed/declined ones, which the dunning-forensics report needs.
func (f *NMIFetcher) fetchTransactions(ctx context.Context, params FetchParams) ([]RemoteTransaction, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	filter := nmi.QueryFilter{}
	if !params.Since.IsZero() {
		filter.StartDate = params.Since.UTC().Format(nmi.QueryTimeFormat)
	}
	if !params.Until.IsZero() {
		filter.EndDate = params.Until.UTC().Format(nmi.QueryTimeFormat)
	}
	filter.ResultLimit = nmiQueryPageLimit
	var out []RemoteTransaction
	seenFirst := map[string]bool{}
	// Query API pages are zero-based; omitted page_number is the first page.
	for page := 0; ; page++ {
		filter.PageNumber = page
		raw, err := f.Client.SearchTransactions(ctx, filter)
		if err != nil {
			return nil, err
		}
		parsed, err := nmi.ParseTransactionReport(raw)
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
			if err := qualifyNMITransaction(t); err != nil {
				return nil, err
			}
			out = append(out, normalizeNMITransaction(t)...)
		}
		if len(parsed.Transactions) < nmiQueryPageLimit {
			break
		}
	}
	return out, nil
}

// An unreadable money fact cannot disappear while its window is marked read.
// Non-financial actions and additional fields retain their existing semantics.
func qualifyNMITransaction(t nmi.QueryTransaction) error {
	if strings.TrimSpace(t.TransactionID) == "" {
		return fmt.Errorf("nmi transaction has no identity")
	}
	for _, action := range t.Actions {
		if _, relevant := normalizeNMIAction(strings.TrimSpace(strings.ToLower(action.ActionType))); !relevant {
			continue
		}
		if strings.TrimSpace(action.Success) != "0" && strings.TrimSpace(action.Success) != "1" {
			return fmt.Errorf("nmi transaction %s has unreadable outcome", t.TransactionID)
		}
		if _, err := nmi.ParseAmountMinor(action.Amount, t.Currency); err != nil {
			return fmt.Errorf("nmi transaction %s has unreadable amount", t.TransactionID)
		}
		if _, ok := action.At(); !ok {
			return fmt.Errorf("nmi transaction %s has unreadable action time", t.TransactionID)
		}
		if strings.TrimSpace(t.Currency) == "" {
			return fmt.Errorf("nmi transaction %s has no currency", t.TransactionID)
		}
	}
	return nil
}

func normalizeNMITransaction(t nmi.QueryTransaction) []RemoteTransaction {
	// A voided or fully refunded sale paid nothing: its sale is neither a
	// payment nor a decline. Its refunds stay visible to the refund plane.
	reversed := t.Reversed()
	var out []RemoteTransaction
	for _, a := range t.Actions {
		txnType, ok := normalizeNMIAction(strings.TrimSpace(strings.ToLower(a.ActionType)))
		if ok && reversed && txnType == TransactionTypeSale {
			continue
		}
		if !ok {
			// settle/check/void/etc. — settlement plumbing, not a
			// charge-level event the diff engine consumes.
			continue
		}
		success := a.Succeeded()
		if txnType == TransactionTypeSale && !success {
			txnType = TransactionTypeDecline
		}
		txn := RemoteTransaction{
			TransactionID: strings.TrimSpace(t.TransactionID),
			// Most NMI reports omit schedule identity; preserve it when present
			// instead of weakening exact attribution into a vault match.
			SubscriptionID: strings.TrimSpace(t.SubscriptionID),
			Type:           txnType,
			Success:        success,
			Currency:       strings.TrimSpace(t.Currency),
			Answer:         t.Evidence(a),
			Raw: rawJSON(map[string]any{
				"source":            "nmi_transaction",
				"condition":         strings.TrimSpace(t.Condition),
				"order_id":          strings.TrimSpace(t.OrderID),
				"order_description": strings.TrimSpace(t.OrderDescription),
				"customerid":        strings.TrimSpace(t.CustomerID),
				"customer_vault_id": strings.TrimSpace(t.CustomerVaultID),
				"email":             strings.TrimSpace(t.Email),
				"action":            a,
			}),
		}
		if minor, err := nmi.ParseAmountMinor(a.Amount, t.Currency); err == nil {
			txn.AmountCents = int64(minor)
		}
		if ts, ok := a.At(); ok {
			txn.OccurredAt = ts
		}
		if !success {
			txn.DeclineReason, txn.DeclineCode = txn.Answer.Text, txn.Answer.Code
		}
		out = append(out, txn)
	}
	return out
}

// nmiScheduleAmount uses the bound catalog currency only for comparison, never
// to manufacture a transaction's currency or a missing provider payment.
func nmiScheduleAmount(remote *RemoteSubscription, currency string) (int64, error) {
	var wire struct {
		Source       string              `json:"source"`
		Subscription *nmi.V5Subscription `json:"subscription"`
	}
	if err := json.Unmarshal(remote.Raw, &wire); err != nil || wire.Source != "nmi_recurring_v5" {
		return remote.AmountCents, nil // typed imports already carry rail minor units
	}
	if wire.Subscription == nil {
		return 0, fmt.Errorf("NMI schedule has no amount record")
	}
	amount := strings.TrimSpace(wire.Subscription.Amount)
	if amount == "" && wire.Subscription.Plan != nil {
		amount = strings.TrimSpace(wire.Subscription.Plan.PlanAmount)
	}
	if amount == "" {
		return 0, nil // optional amount was not reported
	}
	minor, err := nmi.ParseAmountMinor(amount, currency)
	return int64(minor), err
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

func (f *NMIFetcher) fetchPaymentMethods(ctx context.Context, params FetchParams) ([]RemotePaymentMethod, map[string]nmiCustomerIdentity, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	var customers []nmi.V5Customer
	if id := strings.TrimSpace(params.CustomerID); id != "" {
		customer, found, err := f.Client.GetCustomer(ctx, id)
		if err != nil {
			return nil, nil, err
		}
		if found {
			customers = append(customers, customer)
		}
		return f.paymentMethodsFromCustomers(customers)
	}
	cursor := ""
	seenCursor := map[string]bool{}
	for {
		page, err := f.Client.ListCustomersPage(ctx, cursor, nmiV5PageLimit, params.CustomerID)
		if err != nil {
			return nil, nil, err
		}
		customers = append(customers, page.Customers...)
		next := string(page.NextCursor)
		if !page.HasMore {
			break
		}
		if next == "" {
			return nil, nil, fmt.Errorf("nmi customer pagination omitted the next cursor")
		}
		if seenCursor[next] {
			return nil, nil, fmt.Errorf("nmi customer pagination repeated cursor %s; refusing incomplete snapshot", next)
		}
		seenCursor[next] = true
		cursor = next
	}
	return f.paymentMethodsFromCustomers(customers)
}

// ConfirmPaymentMethods re-reads one vault (GET /v5/customers/{id}); an
// absent vault yields no cards.
func (f *NMIFetcher) ConfirmPaymentMethods(ctx context.Context, railCustomerRef string) ([]RemotePaymentMethod, error) {
	customer, found, err := f.Client.GetCustomer(ctx, railCustomerRef)
	if err != nil || !found {
		return nil, err
	}
	out, _, err := f.paymentMethodsFromCustomers([]nmi.V5Customer{customer})
	return out, err
}

func (f *NMIFetcher) paymentMethodsFromCustomers(customers []nmi.V5Customer) ([]RemotePaymentMethod, map[string]nmiCustomerIdentity, error) {
	out := make([]RemotePaymentMethod, 0, len(customers))
	identity := make(map[string]nmiCustomerIdentity, len(customers))
	for _, c := range customers {
		if strings.TrimSpace(c.ID) == "" {
			return nil, nil, fmt.Errorf("nmi customer roster row has no identity")
		}
		entry := RemotePaymentMethod{
			RailCustomerRef: strings.TrimSpace(c.ID),
			Raw:             rawJSON(map[string]any{"source": "nmi_customer_vault_v5", "customer": c}),
		}
		primary := c.PrimaryBilling()
		if primary == nil {
			out = append(out, entry)
			continue
		}
		identity[entry.RailCustomerRef] = nmiCustomerIdentity{
			Email:    strings.TrimSpace(primary.Email),
			Username: strings.TrimSpace(strings.TrimSpace(primary.FirstName) + " " + strings.TrimSpace(primary.LastName)),
		}
		// One entry per card, primary first: a multi-card vault's other
		// cards are compared on their own billing ids.
		billings := []*nmi.V5CustomerBilling{primary}
		for i := range c.Billing {
			if &c.Billing[i] != primary {
				billings = append(billings, &c.Billing[i])
			}
		}
		for _, billing := range billings {
			card := entry
			card.RailMethodRef = strings.TrimSpace(billing.ID)
			card.CardLast4 = cardLast4(billing.PaymentDetails.CardNumber)
			card.CardExpiry = strings.TrimSpace(billing.PaymentDetails.CardExp)
			card.Email = strings.TrimSpace(billing.Email)
			out = append(out, card)
		}
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

// nmiFlag reads a v5 boolean NMI serializes as "0"/"1", a number or a bool.
func nmiFlag(v any) bool {
	text := strings.TrimSpace(fmt.Sprint(v))
	return text == "1" || strings.EqualFold(text, "true")
}
