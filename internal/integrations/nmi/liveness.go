package nmi

import (
	"encoding/xml"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Read-only liveness probes over the NMI Query API (query.php). Shared by the
// dunning manual-rebill verifier (#358) and the subscription liveness sync
// (#367): both answer "what does the provider believe happened?" by READS —
// no direct-post mutation is reachable from this file.

// saleQueryResponse mirrors the slice of the Query API transaction report the
// probes read (search by order_id).
type saleQueryResponse struct {
	XMLName      xml.Name `xml:"nm_response"`
	Transactions []struct {
		TransactionID string `xml:"transaction_id"`
		OrderID       string `xml:"order_id"`
		Actions       []struct {
			ActionType   string `xml:"action_type"`
			Success      string `xml:"success"`
			Date         string `xml:"date"`
			ResponseCode string `xml:"response_code"`
			ResponseText string `xml:"response_text"`
		} `xml:"action"`
	} `xml:"transaction"`
	ErrorResponse string `xml:"error_response"`
}

// queryAPITimeFormat is the Query API start_date/end_date (and action <date>)
// timestamp layout: YYYYMMDDhhmmss.
const queryAPITimeFormat = "20060102150405"

// SaleProbeResult summarizes the sale actions found for an order reference.
// SuccessFound wins over DeclineFound: a charge from ANY attempt counts as
// charged (the no-double-charge invariant), regardless of interleaved
// declines.
type SaleProbeResult struct {
	// SuccessFound + SuccessTransactionID: a successful sale exists.
	SuccessFound         bool
	SuccessTransactionID string
	// DeclineFound + decline evidence: at least one failed sale exists (the
	// latest one's response code/text is carried for hard/soft
	// classification).
	DeclineFound        bool
	DeclineResponseCode int
	DeclineReason       string
}

// ProbeSalesByOrderID queries NMI for transactions carrying the given order
// reference and classifies the sale actions found. since (optional, zero =
// unbounded) restricts the probe to actions on/after that instant — the #367
// liveness prober uses it to ask "was THIS period charged?" without matching
// the signup-time sale that shares the subscription's order reference.
func (c *NMIClient) ProbeSalesByOrderID(orderID string, since time.Time) (SaleProbeResult, error) {
	var result SaleProbeResult
	if strings.TrimSpace(orderID) == "" {
		return result, errors.New("orderID is required")
	}

	filter := QueryFilter{OrderID: orderID}
	if !since.IsZero() {
		filter.StartDate = since.UTC().Format(queryAPITimeFormat)
	}
	raw, err := c.SearchTransactions(filter)
	if err != nil {
		return result, err
	}

	var parsed saleQueryResponse
	if err := xml.Unmarshal([]byte(raw), &parsed); err != nil {
		return result, fmt.Errorf("parse transaction query response: %w", err)
	}
	if msg := strings.TrimSpace(parsed.ErrorResponse); msg != "" {
		return result, fmt.Errorf("transaction query error_response: %s", msg)
	}

	var latestDecline time.Time
	for _, txn := range parsed.Transactions {
		if strings.TrimSpace(txn.OrderID) != "" && strings.TrimSpace(txn.OrderID) != orderID {
			continue
		}
		for _, action := range txn.Actions {
			if strings.ToLower(strings.TrimSpace(action.ActionType)) != "sale" {
				continue
			}
			actionAt, dateOK := time.Time{}, false
			if ts, perr := time.ParseInLocation(queryAPITimeFormat, strings.TrimSpace(action.Date), time.UTC); perr == nil {
				actionAt, dateOK = ts, true
			}
			// Defensive client-side date filter on top of the server-side
			// start_date: an action with an unparseable date still counts
			// (fail open on evidence, the server already filtered).
			if !since.IsZero() && dateOK && actionAt.Before(since.UTC()) {
				continue
			}
			if strings.TrimSpace(action.Success) == "1" {
				result.SuccessFound = true
				result.SuccessTransactionID = strings.TrimSpace(txn.TransactionID)
				return result, nil
			}
			if !result.DeclineFound || !dateOK || actionAt.After(latestDecline) {
				result.DeclineFound = true
				result.DeclineReason = strings.TrimSpace(action.ResponseText)
				result.DeclineResponseCode, _ = strconv.Atoi(strings.TrimSpace(action.ResponseCode))
				if dateOK {
					latestDecline = actionAt
				}
			}
		}
	}
	return result, nil
}

// FindSuccessfulSaleByOrderID reports the first successful sale carrying the
// order reference (no date bound) — the manual-rebill verifier's question:
// every attempt for a period shares the order reference, so a hit from ANY
// attempt counts (the no-double-charge invariant).
func (c *NMIClient) FindSuccessfulSaleByOrderID(orderID string) (transactionID string, found bool, err error) {
	result, err := c.ProbeSalesByOrderID(orderID, time.Time{})
	if err != nil {
		return "", false, err
	}
	return result.SuccessTransactionID, result.SuccessFound, nil
}

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
func (c *NMIClient) GetRecurringLiveness(subscriptionID string) (RecurringLiveness, error) {
	var out RecurringLiveness
	if strings.TrimSpace(subscriptionID) == "" {
		return out, errors.New("subscriptionID is required")
	}
	sub, found, err := c.GetSubscription(subscriptionID)
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
