package nmi

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/url"
	"strings"
)

// RecurringReportRow is one live subscription of the recurring report: the
// identity facts v5 subscriptions omit.
type RecurringReportRow struct {
	SubscriptionID string `xml:"subscription_id"`
	OrderID        string `xml:"orderid"`
	PONumber       string `xml:"ponumber"`
	Email          string `xml:"email"`
	FirstName      string `xml:"first_name"`
	LastName       string `xml:"last_name"`
	PlanID         string `xml:"plan>plan_id"`
}

// RecurringReport reads every live subscription of the recurring report
// (query.php report_type=recurring); NMI omits deleted ones.
func (c *NMIClient) RecurringReport(ctx context.Context) ([]RecurringReportRow, error) {
	if err := c.checkConfiguration(); err != nil {
		return nil, err
	}
	raw, err := c.sendQueryRequest(ctx, url.Values{"security_key": {c.SecurityKey}, "report_type": {"recurring"}})
	if err != nil {
		return nil, err
	}
	return ParseRecurringReport(raw)
}

// ParseRecurringReport parses a recurring report; an error_response is an error.
func ParseRecurringReport(raw string) ([]RecurringReportRow, error) {
	var report struct {
		XMLName       xml.Name             `xml:"nm_response"`
		ErrorResponse string               `xml:"error_response"`
		Subscriptions []RecurringReportRow `xml:"subscription"`
	}
	if err := xml.Unmarshal([]byte(raw), &report); err != nil {
		return nil, fmt.Errorf("parse recurring report: %w", err)
	}
	if msg := strings.TrimSpace(report.ErrorResponse); msg != "" {
		return nil, queryError("recurring report", msg)
	}
	return report.Subscriptions, nil
}
