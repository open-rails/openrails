package nmi

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Batched Query API reads for resolving unverified schedules (#1094 §12).
// query.php takes comma-separated subscription_id lists; NMI documents no
// list-size or rate limit, so callers stay at MaxQueryIDs per request.

// MaxQueryIDs is the most schedule ids one query.php request carries.
const MaxQueryIDs = 50

// QueryPageLimit is the result_limit of one paged transaction read.
const QueryPageLimit = 1000

// ScheduleRecord is one schedule from the recurring report.
type ScheduleRecord struct {
	SubscriptionID string
	OrderID        string
	NextChargeDate time.Time // zero when absent or unparseable
}

// ScheduleSale is one sale action with the fields a batched read attributes
// it by. The transaction report names no schedule on live accounts, so
// SubscriptionID is usually empty and callers attribute by vault or order.
type ScheduleSale struct {
	SaleAction
	SubscriptionID string
	OrderID        string
	VaultID        string
}

// ReadSchedules reads the recurring report for up to MaxQueryIDs schedules in
// one request. A schedule NMI deleted is absent.
func (c *NMIClient) ReadSchedules(ctx context.Context, ids []string) (map[string]ScheduleRecord, error) {
	if err := c.checkConfiguration(); err != nil {
		return nil, err
	}
	list, err := idList(ids)
	if err != nil {
		return nil, err
	}
	raw, err := c.sendQueryRequest(ctx, url.Values{"security_key": {c.SecurityKey}, "report_type": {"recurring"}, "subscription_id": {list}})
	if err != nil {
		return nil, err
	}
	var report struct {
		XMLName       xml.Name `xml:"nm_response"`
		ErrorResponse string   `xml:"error_response"`
		Subscriptions []struct {
			SubscriptionID string `xml:"subscription_id"`
			OrderID        string `xml:"orderid"`
			NextChargeDate string `xml:"next_charge_date"`
		} `xml:"subscription"`
	}
	if err := xml.Unmarshal([]byte(raw), &report); err != nil {
		return nil, fmt.Errorf("parse recurring report: %w", err)
	}
	if msg := strings.TrimSpace(report.ErrorResponse); msg != "" {
		return nil, fmt.Errorf("recurring report error_response: %s", msg)
	}
	out := make(map[string]ScheduleRecord, len(report.Subscriptions))
	for _, s := range report.Subscriptions {
		rec := ScheduleRecord{SubscriptionID: strings.TrimSpace(s.SubscriptionID), OrderID: strings.TrimSpace(s.OrderID)}
		if next, err := parseV5Date(s.NextChargeDate); err == nil {
			rec.NextChargeDate = next
		}
		if rec.SubscriptionID != "" {
			out[rec.SubscriptionID] = rec
		}
	}
	return out, nil
}

// SalesForSchedules reads every sale action on or after since for up to
// MaxQueryIDs schedules, paging until the report is exhausted.
func (c *NMIClient) SalesForSchedules(ctx context.Context, ids []string, since time.Time) ([]ScheduleSale, error) {
	list, err := idList(ids)
	if err != nil {
		return nil, err
	}
	var out []ScheduleSale
	for page := 1; ; page++ {
		sales, n, err := c.salesPage(ctx, QueryFilter{SubscriptionID: list}, since, time.Time{}, page)
		if err != nil {
			return nil, err
		}
		out = append(out, sales...)
		if n < QueryPageLimit {
			return out, nil
		}
	}
}

// SalesPage reads one page of every sale action in [since, until). It
// reports how many transactions the page held, so a caller checkpointing a
// bulk read knows when the report is exhausted (fewer than QueryPageLimit).
func (c *NMIClient) SalesPage(ctx context.Context, since, until time.Time, page int) ([]ScheduleSale, int, error) {
	return c.salesPage(ctx, QueryFilter{}, since, until, page)
}

func (c *NMIClient) salesPage(ctx context.Context, filter QueryFilter, since, until time.Time, page int) ([]ScheduleSale, int, error) {
	if !since.IsZero() {
		filter.StartDate = since.UTC().Format(queryAPITimeFormat)
	}
	if !until.IsZero() {
		filter.EndDate = until.UTC().Format(queryAPITimeFormat)
	}
	filter.ResultLimit, filter.PageNumber = QueryPageLimit, page
	raw, err := c.SearchTransactions(ctx, filter)
	if err != nil {
		return nil, 0, err
	}
	var parsed struct {
		XMLName       xml.Name `xml:"nm_response"`
		ErrorResponse string   `xml:"error_response"`
		Transactions  []struct {
			TransactionID   string `xml:"transaction_id"`
			SubscriptionID  string `xml:"subscription_id"`
			OrderID         string `xml:"order_id"`
			CustomerVaultID string `xml:"customer_vault_id"`
			Condition       string `xml:"condition"`
			Currency        string `xml:"currency"`
			Actions         []struct {
				Amount       string `xml:"amount"`
				ActionType   string `xml:"action_type"`
				Success      string `xml:"success"`
				Date         string `xml:"date"`
				ResponseCode string `xml:"response_code"`
				ResponseText string `xml:"response_text"`
			} `xml:"action"`
		} `xml:"transaction"`
	}
	if err := xml.Unmarshal([]byte(raw), &parsed); err != nil {
		return nil, 0, fmt.Errorf("parse transaction query response: %w", err)
	}
	if msg := strings.TrimSpace(parsed.ErrorResponse); msg != "" {
		return nil, 0, fmt.Errorf("transaction query error_response: %s", msg)
	}
	var out []ScheduleSale
	for _, t := range parsed.Transactions {
		actions := make([]TransactionAction, 0, len(t.Actions))
		for _, a := range t.Actions {
			actions = append(actions, TransactionAction{Type: a.ActionType, Success: a.Success, Amount: a.Amount})
		}
		if SaleReversed(t.Condition, actions) {
			continue // voided or refunded in full: no payment, no decline
		}
		for _, a := range t.Actions {
			if !strings.EqualFold(strings.TrimSpace(a.ActionType), "sale") {
				continue
			}
			at, err := time.ParseInLocation(queryAPITimeFormat, strings.TrimSpace(a.Date), time.UTC)
			if err != nil {
				at = time.Time{}
			}
			if !since.IsZero() && !at.IsZero() && at.Before(since.UTC()) {
				continue
			}
			out = append(out, ScheduleSale{
				SaleAction: SaleAction{
					TransactionID: strings.TrimSpace(t.TransactionID), Success: strings.TrimSpace(a.Success) == "1", At: at,
					Amount: strings.TrimSpace(a.Amount), Currency: strings.TrimSpace(t.Currency),
					ResponseCode: strings.TrimSpace(a.ResponseCode), ResponseText: strings.TrimSpace(a.ResponseText),
				},
				SubscriptionID: strings.TrimSpace(t.SubscriptionID),
				OrderID:        strings.TrimSpace(t.OrderID),
				VaultID:        strings.TrimSpace(t.CustomerVaultID),
			})
		}
	}
	return out, len(parsed.Transactions), nil
}

func idList(ids []string) (string, error) {
	if len(ids) == 0 || len(ids) > MaxQueryIDs {
		return "", fmt.Errorf("nmi query: %d schedule ids (1..%d)", len(ids), MaxQueryIDs)
	}
	clean := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || strings.Contains(id, ",") {
			return "", fmt.Errorf("nmi query: invalid schedule id %q", id)
		}
		clean = append(clean, id)
	}
	return strings.Join(clean, ","), nil
}
