package nmi

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/billing/decline"
)

// Read-only liveness probes over the NMI Query API (query.php). Shared by the
// dunning manual-rebill verifier (#358) and the unknown-cohort per-subscription
// probe (#665, reconcile.NMISubscriptionProber): both answer "what does the
// provider believe happened?" by READS — no direct-post mutation is reachable
// from this file.

// queryAPITimeFormat is the Query API start_date/end_date (and action <date>)
// timestamp layout: YYYYMMDDhhmmss.
const queryAPITimeFormat = "20060102150405"

// SaleProbeResult summarizes the sale actions found for an order reference.
// SuccessFound wins over DeclineFound: a charge from ANY attempt counts as
// charged (the no-double-charge invariant), regardless of interleaved
// declines.
type SaleProbeResult struct {
	// Sales is every sale action on/after the probe's since, successes and
	// declines, in report order: a schedule billed several times since (a
	// daily cadence, a missed notice) mirrors each charge once.
	Sales []SaleAction
	// SuccessFound + SuccessTransactionID: a successful sale exists.
	// SuccessAt/SuccessAmount/SuccessCurrency carry that action's verbatim
	// evidence (zero/empty when the report omitted or garbled them).
	SuccessFound         bool
	SuccessTransactionID string
	SuccessAt            time.Time
	SuccessAmount        string
	SuccessCurrency      string
	// DeclineFound + decline evidence: at least one failed sale exists (the
	// latest one's id/time/amount and response code/text are carried for
	// hard/soft classification and payment backfill).
	DeclineFound         bool
	DeclineTransactionID string
	DeclineAt            time.Time
	DeclineAmount        string
	DeclineCurrency      string
	DeclineResponseCode  int
	DeclineReason        string
	// ReversedFound: an approved sale was voided or refunded in full. It
	// executed but paid nothing: neither a success nor a decline.
	ReversedFound         bool
	ReversedTransactionID string
}

// ProbeSalesByOrderID queries NMI for transactions carrying the given order
// reference and classifies the sale actions found. since (optional, zero =
// unbounded) restricts the probe to actions on/after that instant — the
// unknown-cohort prober uses it to ask "was THIS period charged?" without
// matching the signup-time sale that shares the subscription's order reference.
func (c *NMIClient) ProbeSalesByOrderID(ctx context.Context, orderID string, since time.Time) (SaleProbeResult, error) {
	if strings.TrimSpace(orderID) == "" {
		return SaleProbeResult{}, errors.New("orderID is required")
	}
	return c.probeSales(ctx, QueryFilter{OrderID: orderID}, orderID, since)
}

// ProbeSalesBySubscriptionID is ProbeSalesByOrderID for the sales NMI's
// recurring engine made for one schedule. A schedule created outside
// OpenRails (an imported legacy book) carries its own order reference, so
// only the schedule id finds its renewals.
func (c *NMIClient) ProbeSalesBySubscriptionID(ctx context.Context, subscriptionID string, since time.Time) (SaleProbeResult, error) {
	if strings.TrimSpace(subscriptionID) == "" {
		return SaleProbeResult{}, errors.New("subscriptionID is required")
	}
	return c.probeSales(ctx, QueryFilter{SubscriptionID: subscriptionID}, "", since)
}

func (c *NMIClient) probeSales(ctx context.Context, filter QueryFilter, orderID string, since time.Time) (SaleProbeResult, error) {
	var result SaleProbeResult
	if !since.IsZero() {
		filter.StartDate = since.UTC().Format(queryAPITimeFormat)
	}
	report, err := c.TransactionReport(ctx, filter)
	if err != nil {
		return result, err
	}
	for _, txn := range report.Transactions {
		if orderID != "" && strings.TrimSpace(txn.OrderID) != "" && strings.TrimSpace(txn.OrderID) != orderID {
			continue
		}
		if txn.Reversed() {
			result.ReversedFound, result.ReversedTransactionID = true, strings.TrimSpace(txn.TransactionID)
			continue
		}
		result.Sales = append(result.Sales, saleActions(txn, since)...)
	}
	var latestDecline time.Time
	for _, sale := range result.Sales {
		if sale.Success {
			result.SuccessFound = true
			result.SuccessTransactionID = sale.TransactionID
			result.SuccessAt = sale.At // zero when unparseable
			result.SuccessAmount = sale.Amount
			result.SuccessCurrency = sale.Currency
			return result, nil
		}
		dated := !sale.At.IsZero()
		if !result.DeclineFound || !dated || sale.At.After(latestDecline) {
			result.DeclineFound = true
			result.DeclineTransactionID = sale.TransactionID
			result.DeclineAt = sale.At
			result.DeclineAmount = sale.Amount
			result.DeclineCurrency = sale.Currency
			result.DeclineReason = sale.Evidence.Text
			result.DeclineResponseCode, _ = strconv.Atoi(sale.Evidence.Code)
			if dated {
				latestDecline = sale.At
			}
		}
	}
	return result, nil
}

// SaleAction is one sale action from a transaction report.
type SaleAction struct {
	TransactionID string
	Success       bool
	At            time.Time // zero when the report garbled the date
	Amount        string
	Currency      string
	// Evidence is the answer and the card, as the report gives them.
	Evidence decline.Evidence
}

// saleActions is the transaction's sale actions on or after since. An action
// with an unparseable date still counts: the server already filtered by date.
func saleActions(txn QueryTransaction, since time.Time) []SaleAction {
	var out []SaleAction
	for _, action := range txn.Actions {
		if !action.Is("sale") {
			continue
		}
		at, _ := action.At()
		if !since.IsZero() && !at.IsZero() && at.Before(since.UTC()) {
			continue
		}
		out = append(out, SaleAction{
			TransactionID: strings.TrimSpace(txn.TransactionID), Success: action.Succeeded(), At: at,
			Amount: strings.TrimSpace(action.Amount), Currency: strings.TrimSpace(txn.Currency), Evidence: txn.Evidence(action),
		})
	}
	return out
}

// FindSuccessfulSaleByOrderID reports the first successful sale carrying the
// order reference (no date bound) — the manual-rebill verifier's question:
// every attempt for a period shares the order reference, so a hit from ANY
// attempt counts (the no-double-charge invariant).
func (c *NMIClient) FindSuccessfulSaleByOrderID(ctx context.Context, orderID string) (transactionID string, found bool, err error) {
	result, err := c.ProbeSalesByOrderID(ctx, orderID, time.Time{})
	if err != nil {
		return "", false, err
	}
	if !result.SuccessFound && result.ReversedFound {
		return "", false, fmt.Errorf("%w: %s", ErrSaleReversed, result.ReversedTransactionID)
	}
	return result.SuccessTransactionID, result.SuccessFound, nil
}

// ErrSaleReversed: the order's sale executed and was voided or refunded in
// full. It is neither payment nor an unsent submission, so a verifier must
// neither grant it nor send again; the operation waits for review.
var ErrSaleReversed = errors.New("nmi sale was voided or refunded in full")

// RecurringLiveness is the parsed remote-truth view of one NMI recurring
// subscription. Found=false means NMI no longer knows the subscription id —
// at NMI that IS terminal (cancelled records are deleted, and the v5 GET
// answers 404). NextChargeDate is zero when absent/unparseable.
type RecurringLiveness struct {
	Found          bool
	NextChargeDate time.Time
}

// GetRecurringLiveness reads GET /v5/subscriptions/{id} and reports whether
// the remote recurring record is still alive and when it will next charge.
func (c *NMIClient) GetRecurringLiveness(ctx context.Context, subscriptionID string) (RecurringLiveness, error) {
	var out RecurringLiveness
	if strings.TrimSpace(subscriptionID) == "" {
		return out, errors.New("subscriptionID is required")
	}
	sub, found, err := c.GetSubscription(ctx, subscriptionID)
	if err != nil {
		return out, err
	}
	if !found {
		return out, nil
	}
	out.Found = true
	if next, perr := parseV5Date(sub.NextBillingDate); perr == nil {
		out.NextChargeDate = next
	}
	return out, nil
}

// parseV5Date accepts the date shapes v5 emits (ISO 8601 timestamp or bare
// date) and returns a UTC time.
func parseV5Date(raw string) (time.Time, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return time.Time{}, errors.New("empty date")
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02"} {
		if ts, err := time.ParseInLocation(layout, trimmed, time.UTC); err == nil {
			return ts.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized v5 date %q", raw)
}
