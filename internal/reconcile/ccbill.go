package reconcile

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/integrations/ccbill"
	"github.com/open-rails/openrails/internal/shared/timeutil"
)

// ccbillDataLink is the slice of *ccbill.DataLinkClient the fetcher uses. Both
// methods are DataLink batch READS (POST form to /data/main.cgi returning
// CSV); DataLink has no mutation endpoints, so the fetcher is read-only by
// construction.
type ccbillDataLink interface {
	FetchActiveMembers(ctx context.Context) ([]ccbill.CCBillRecord, error)
	FetchTransactionExport(ctx context.Context, start, end time.Time, types []ccbill.DataLinkTxnType) ([]ccbill.DataLinkExportRow, error)
}

// CCBillFetcher pulls CCBill state via DataLink batch exports:
//   - ACTIVEMEMBERS: the roster of currently-active subscriptions.
//   - REBILL/CANCELLATION/EXPIRE/REFUND/CHARGEBACK exports over [Since,Until]:
//     charge events plus subscription terminations.
//
// Provider quirks:
//   - CCBill exposes no vault read => Vault=false.
//   - DataLink is event-based for terminations: a subscription cancelled or
//     expired inside the window appears as a CANCELLATION/EXPIRE row (emitted
//     here as a RemoteSubscription with that terminal status) and is absent
//     from ACTIVEMEMBERS. Terminations OUTSIDE the window are simply absent,
//     so "missing from the snapshot" is NOT proof a subscription never
//     existed — the phase-2 diff engine must treat CCBill absence as
//     inactive-or-out-of-window, not unknown-to-CCBill.
//   - Rebill amounts are USD (DataLink does not echo a currency; legacy
//     integrations hardcode USD likewise).
//   - The ACTIVEMEMBERS roster has no server-side subscription filter;
//     FetchParams.SubscriptionID narrowing is applied client-side here.
type CCBillFetcher struct {
	DataLink ccbillDataLink
}

// NewCCBillFetcher builds a fetcher over an existing DataLink client.
func NewCCBillFetcher(dl *ccbill.DataLinkClient) *CCBillFetcher {
	return &CCBillFetcher{DataLink: dl}
}

func (f *CCBillFetcher) Name() string { return string(ProviderCCBill) }

func (f *CCBillFetcher) Capabilities() Capabilities {
	return Capabilities{
		Subscriptions: true,
		Transactions:  true,
		Refunds:       true,
		Chargebacks:   true,
		Vault:         false,
	}
}

func (f *CCBillFetcher) Fetch(ctx context.Context, params FetchParams) (*RemoteSnapshot, error) {
	snap := &RemoteSnapshot{
		Provider:     ProviderCCBill,
		FetchedAt:    time.Now().UTC(),
		Capabilities: f.Capabilities(),
	}

	active, err := f.DataLink.FetchActiveMembers(ctx)
	if err != nil {
		return nil, fmt.Errorf("ccbill activemembers export: %w", err)
	}
	for _, rec := range active {
		if params.SubscriptionID != "" && rec.SubscriptionID != params.SubscriptionID {
			continue
		}
		snap.Subscriptions = append(snap.Subscriptions, normalizeCCBillActiveMember(rec))
	}

	// Transaction-type exports need a window; default to the trailing 30 days
	// when the caller didn't bound the fetch.
	until := params.Until
	if until.IsZero() {
		until = time.Now().UTC()
	}
	since := params.Since
	if since.IsZero() {
		since = until.Add(-30 * 24 * time.Hour)
	}

	rows, err := f.DataLink.FetchTransactionExport(ctx, since, until, ccbill.AllDataLinkTxnTypes)
	if err != nil {
		return nil, fmt.Errorf("ccbill transaction export: %w", err)
	}
	for _, row := range rows {
		if params.SubscriptionID != "" && row.SubscriptionID() != params.SubscriptionID {
			continue
		}
		switch row.TransactionType {
		case ccbill.DataLinkTxnCancellation, ccbill.DataLinkTxnExpire:
			snap.Subscriptions = append(snap.Subscriptions, normalizeCCBillTermination(row))
		case ccbill.DataLinkTxnRebill, ccbill.DataLinkTxnRefund, ccbill.DataLinkTxnChargeback:
			snap.Transactions = append(snap.Transactions, normalizeCCBillTransaction(row))
		default:
			// Unrequested/unknown row types are preserved as raw-only
			// transactions so nothing in the export is dropped.
			snap.Transactions = append(snap.Transactions, RemoteTransaction{
				SubscriptionID: row.SubscriptionID(),
				Type:           TransactionTypeSale,
				Success:        false,
				Raw:            ccbillRowRaw(row),
			})
		}
	}

	return snap, nil
}

func normalizeCCBillActiveMember(rec ccbill.CCBillRecord) RemoteSubscription {
	sub := RemoteSubscription{
		RailSubscriptionID: rec.SubscriptionID,
		Status:             SubscriptionStatusActive,
		RawStatus:          strings.TrimSpace(rec.Status),
		Email:              strings.TrimSpace(rec.Email),
		Username:           strings.TrimSpace(rec.Username),
		PlanID:             strings.TrimSpace(rec.Field2),
		Currency:           "USD",
		Raw: rawJSON(map[string]any{
			"source": "ccbill_activemembers",
			"record": rec,
		}),
	}
	if !ccbill.IsDataLinkActiveStatus(rec.Status) {
		sub.Status = SubscriptionStatusUnknown
	}
	if next, err := timeutil.ParseFirstUTC(strings.TrimSpace(rec.RebillDate), "2006-01-02", "01/02/2006", time.RFC3339); err == nil {
		sub.NextBillingAt = &next
	}
	return sub
}

func normalizeCCBillTermination(row ccbill.DataLinkExportRow) RemoteSubscription {
	status := SubscriptionStatusCancelled
	if row.TransactionType == ccbill.DataLinkTxnExpire {
		status = SubscriptionStatusExpired
	}
	return RemoteSubscription{
		RailSubscriptionID: row.SubscriptionID(),
		Status:             status,
		RawStatus:          string(row.TransactionType),
		Currency:           "USD",
		Raw:                ccbillRowRaw(row),
	}
}

func normalizeCCBillTransaction(row ccbill.DataLinkExportRow) RemoteTransaction {
	txn := RemoteTransaction{
		TransactionID:  row.TransactionID(),
		SubscriptionID: row.SubscriptionID(),
		Success:        true,
		Currency:       "USD",
		Raw:            ccbillRowRaw(row),
	}
	switch row.TransactionType {
	case ccbill.DataLinkTxnRefund:
		txn.Type = TransactionTypeRefund
	case ccbill.DataLinkTxnChargeback:
		txn.Type = TransactionTypeChargeback
	default:
		txn.Type = TransactionTypeSale
	}
	if cents, err := parseAmountCents(row.Amount()); err == nil {
		txn.AmountCents = cents
	}
	if ts, err := timeutil.ParseFirstUTC(row.Timestamp(), "2006-01-02 15:04:05", "2006-01-02", "20060102150405", "01/02/2006"); err == nil {
		txn.OccurredAt = ts
	}
	return txn
}

func ccbillRowRaw(row ccbill.DataLinkExportRow) []byte {
	return rawJSON(map[string]any{
		"source": "ccbill_datalink_export",
		"type":   string(row.TransactionType),
		"fields": row.Fields,
	})
}
