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

// DataLink transaction-type batch exports (#107). Beyond the ACTIVEMEMBERS
// roster, DataLink's main.cgi exports per-event CSV batches over a date range:
// successful rebills, cancellations, expirations, refunds, and chargebacks.
// The transactionTypes values below are the ones the long-standing DataLink
// integrations (s2member, aMember, magic-members) request; CCBill's public doc
// site no longer serves the original DataLink spec, so the per-type CSV column
// meanings encoded in DataLinkExportRow accessors follow those integrations:
// column 0 = transaction type, column 3 = subscription id for every type;
// REBILL rows carry transaction id at column 5 and amount at column 6;
// REFUND/CHARGEBACK rows carry the amount at column 5. All columns are always
// preserved verbatim in Fields. NOTE: not yet validated against a live
// DataLink account (no credentials available at build time) — treat the typed
// accessors as best-effort and the raw Fields as authoritative.
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

// dataLinkLocation is the timezone DataLink interprets startTime/endTime in:
// CCBill operates on US Mountain Standard Time (Arizona — no DST).
var dataLinkLocation = time.FixedZone("MST", -7*60*60)

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
// [start, end] in one DataLink request (types are comma-joined, matching how
// existing integrations batch them). start/end are converted to CCBill's MST
// clock. Returns the parsed rows in response order; rows whose leading type
// column is not one of the requested types are returned too (type preserved)
// so nothing in the export is silently dropped.
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

	formData := url.Values{}
	formData.Set("transactionTypes", strings.Join(names, ","))
	formData.Set("startTime", start.In(dataLinkLocation).Format(dataLinkTimeFormat))
	formData.Set("endTime", end.In(dataLinkLocation).Format(dataLinkTimeFormat))

	content, err := c.fetchDataLink(ctx, formData)
	if err != nil {
		return nil, err
	}
	return parseDataLinkExportCSV(content)
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
