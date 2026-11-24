package ccbill

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"
)

// DataLink transaction-type batch exports: per-event CSV batches over a date
// range (rebills, cancellations, expirations, refunds, chargebacks). CCBill's
// public docs omit the DataLink spec, so transactionTypes and column meanings
// follow long-standing integrations (s2member, aMember): column 0 = type,
// 3 = subscription id; REBILL 5 = transaction id, 6 = amount; REFUND/CHARGEBACK
// 5 = amount. Unvalidated against a live account: the typed accessors are
// best-effort, the verbatim Fields authoritative.
type DataLinkTxnType string

const (
	DataLinkTxnRebill       DataLinkTxnType = "REBILL"
	DataLinkTxnCancellation DataLinkTxnType = "CANCELLATION"
	DataLinkTxnExpire       DataLinkTxnType = "EXPIRE"
	DataLinkTxnRefund       DataLinkTxnType = "REFUND"
	DataLinkTxnChargeback   DataLinkTxnType = "CHARGEBACK"
)

// AllDataLinkTxnTypes is every supported batch-export type, for callers that
// want the full event feed over a window.
var AllDataLinkTxnTypes = []DataLinkTxnType{
	DataLinkTxnRebill,
	DataLinkTxnCancellation,
	DataLinkTxnExpire,
	DataLinkTxnRefund,
	DataLinkTxnChargeback,
}

// dataLinkTimeFormat is the startTime/endTime layout (YYYYMMDDHHMMSS).
const dataLinkTimeFormat = "20060102150405"

// dataLinkLocation is the timezone DataLink's startTime/endTime are sent in:
// US Mountain Standard Time (Arizona, no DST), where CCBill operates.
// UNCONFIRMED: the DataLink guide names no zone; see timeZone.
var dataLinkLocation = time.FixedZone("MST", -7*60*60)

// maxDataLinkWindow is the longest range one export answers: CCBill refuses
// a startTime to endTime range over 24 hours.
const maxDataLinkWindow = 24 * time.Hour

// dataLinkWindows splits [start, end) into export requests of whole seconds:
// at most maxDataLinkWindow each, an inclusive endTime the second before the
// next startTime.
func dataLinkWindows(start, end time.Time) [][2]time.Time {
	start, end = start.Truncate(time.Second), end.Truncate(time.Second)
	var out [][2]time.Time
	for from := start; from.Before(end); {
		to := from.Add(maxDataLinkWindow)
		if to.After(end) {
			to = end
		}
		out = append(out, [2]time.Time{from, to.Add(-time.Second)})
		from = to
	}
	return out
}

// DataLinkExportRow is one CSV row from a transaction-type batch export. The
// full row is preserved in Fields (quoted columns, in order); the typed
// accessors decode the per-type column meanings documented above.
type DataLinkExportRow struct {
	TransactionType DataLinkTxnType
	Fields          []string
}

// field returns the i-th CSV column, trimmed, or "" when the row is short.
func (r DataLinkExportRow) field(i int) string {
	if i < 0 || i >= len(r.Fields) {
		return ""
	}
	return strings.TrimSpace(r.Fields[i])
}

// SubscriptionID returns the CCBill subscription id (column 3, every type).
func (r DataLinkExportRow) SubscriptionID() string { return r.field(3) }

// Timestamp returns the event timestamp column (column 4) verbatim.
func (r DataLinkExportRow) Timestamp() string { return r.field(4) }

// TransactionID returns the per-charge transaction id for REBILL rows
// (column 5). Empty for other types, which don't carry one.
func (r DataLinkExportRow) TransactionID() string {
	if r.TransactionType == DataLinkTxnRebill {
		return r.field(5)
	}
	return ""
}

// Amount returns the row's money amount as the raw decimal string: column 6
// for REBILL, column 5 for REFUND/CHARGEBACK, "" for types without an amount.
func (r DataLinkExportRow) Amount() string {
	switch r.TransactionType {
	case DataLinkTxnRebill:
		return r.field(6)
	case DataLinkTxnRefund, DataLinkTxnChargeback:
		return r.field(5)
	default:
		return ""
	}
}

// FetchTransactionExport pulls the requested transaction-type batches over
// [start, end), one request per day at most (dataLinkWindows), in CCBill's
// MST clock; a live account's range is one day at most. Rows of unrequested
// types are returned too, so nothing in the export is dropped.
func (c *DataLinkClient) FetchTransactionExport(ctx context.Context, start, end time.Time, types []DataLinkTxnType) ([]DataLinkExportRow, error) {
	if len(types) == 0 {
		return nil, fmt.Errorf("at least one transaction type is required")
	}
	if start.IsZero() || end.IsZero() {
		return nil, fmt.Errorf("start and end times are required")
	}
	if end.Before(start) {
		return nil, fmt.Errorf("end time precedes start time")
	}

	names := make([]string, 0, len(types))
	for _, t := range types {
		names = append(names, string(t))
	}

	windows := dataLinkWindows(start, end)
	if c.Paced() && len(windows) > 1 {
		return nil, fmt.Errorf("ccbill datalink: a live account answers one day an hour; read [%s, %s) a day at a time", start.UTC().Format(time.RFC3339), end.UTC().Format(time.RFC3339))
	}
	var rows []DataLinkExportRow
	for _, window := range windows {
		formData := url.Values{}
		formData.Set("transactionTypes", strings.Join(names, ","))
		formData.Set("startTime", window[0].In(dataLinkLocation).Format(dataLinkTimeFormat))
		formData.Set("endTime", window[1].In(dataLinkLocation).Format(dataLinkTimeFormat))
		content, err := c.fetchDataLink(ctx, formData)
		if err != nil {
			return nil, err
		}
		parsed, err := parseDataLinkExportCSV(content)
		if err != nil {
			return nil, err
		}
		rows = append(rows, parsed...)
	}
	return rows, nil
}

// parseDataLinkExportCSV decodes a transaction-export response body. An empty
// body (no events in the window) yields zero rows.
func parseDataLinkExportCSV(content string) ([]DataLinkExportRow, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil, nil
	}

	reader := csv.NewReader(strings.NewReader(content))
	reader.FieldsPerRecord = -1

	var rows []DataLinkExportRow
	for {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading datalink export CSV: %w", err)
		}
		if len(record) == 0 {
			continue
		}
		rows = append(rows, DataLinkExportRow{
			TransactionType: DataLinkTxnType(strings.TrimSpace(record[0])),
			Fields:          record,
		})
	}
	return rows, nil
}
