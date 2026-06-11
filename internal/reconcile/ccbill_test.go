package reconcile

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/integrations/ccbill"
)

// fakeDataLink supplies canned DataLink results and records the export window.
type fakeDataLink struct {
	active     []ccbill.CCBillRecord
	exportRows []ccbill.DataLinkExportRow

	exportStart time.Time
	exportEnd   time.Time
	exportTypes []ccbill.DataLinkTxnType
}

func (f *fakeDataLink) FetchActiveMembers(ctx context.Context) ([]ccbill.CCBillRecord, error) {
	return f.active, nil
}

func (f *fakeDataLink) FetchTransactionExport(ctx context.Context, start, end time.Time, types []ccbill.DataLinkTxnType) ([]ccbill.DataLinkExportRow, error) {
	f.exportStart, f.exportEnd, f.exportTypes = start, end, types
	return f.exportRows, nil
}

func exportRow(fields ...string) ccbill.DataLinkExportRow {
	return ccbill.DataLinkExportRow{
		TransactionType: ccbill.DataLinkTxnType(fields[0]),
		Fields:          fields,
	}
}

func TestCCBillFetcher_Fetch(t *testing.T) {
	t.Parallel()

	dl := &fakeDataLink{
		active: []ccbill.CCBillRecord{
			{
				TransactionType: "ACTIVEMEMBERS",
				SubscriptionID:  "0125217202000000017",
				Date:            "2026-05-03",
				Username:        "user1",
				Email:           "u1@example.com",
				Status:          "1",
				RebillDate:      "2026-07-03",
				ExpiryDate:      "2026-07-03",
			},
		},
		exportRows: []ccbill.DataLinkExportRow{
			exportRow("REBILL", "123", "x", "0125217202000000017", "2026-06-01 04:05:06", "918273645", "23.99"),
			exportRow("CANCELLATION", "123", "x", "0999000000000000001", "2026-06-02"),
			exportRow("EXPIRE", "123", "x", "0999000000000000002", "2026-06-03"),
			exportRow("REFUND", "123", "x", "0125217202000000017", "2026-06-04", "23.99"),
			exportRow("CHARGEBACK", "123", "x", "0125217202000000017", "2026-06-05", "23.99"),
		},
	}
	fetcher := &CCBillFetcher{DataLink: dl}

	since := time.Date(2026, 5, 12, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 6, 11, 0, 0, 0, 0, time.UTC)
	snap, err := fetcher.Fetch(context.Background(), FetchParams{Since: since, Until: until})
	require.NoError(t, err)

	require.Equal(t, ProviderCCBill, snap.Provider)
	require.False(t, snap.Capabilities.Vault)
	require.True(t, snap.Capabilities.Chargebacks)

	// Window + full type set forwarded.
	require.Equal(t, since, dl.exportStart)
	require.Equal(t, until, dl.exportEnd)
	require.Equal(t, ccbill.AllDataLinkTxnTypes, dl.exportTypes)

	// Subscriptions: 1 active roster entry + cancellation + expiry events.
	require.Len(t, snap.Subscriptions, 3)
	active := snap.Subscriptions[0]
	require.Equal(t, "0125217202000000017", active.ProcessorSubscriptionID)
	require.Equal(t, SubscriptionStatusActive, active.Status)
	require.Equal(t, "1", active.RawStatus)
	require.Equal(t, "u1@example.com", active.Email)
	require.Equal(t, "user1", active.Username)
	require.NotNil(t, active.NextBillingAt)
	require.Equal(t, time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC), *active.NextBillingAt)

	cancelled := snap.Subscriptions[1]
	require.Equal(t, "0999000000000000001", cancelled.ProcessorSubscriptionID)
	require.Equal(t, SubscriptionStatusCancelled, cancelled.Status)
	require.Equal(t, "CANCELLATION", cancelled.RawStatus)

	expired := snap.Subscriptions[2]
	require.Equal(t, SubscriptionStatusExpired, expired.Status)

	// Transactions: rebill, refund, chargeback.
	require.Len(t, snap.Transactions, 3)
	rebill := snap.Transactions[0]
	require.Equal(t, TransactionTypeSale, rebill.Type)
	require.Equal(t, "918273645", rebill.TransactionID)
	require.Equal(t, "0125217202000000017", rebill.SubscriptionID)
	require.Equal(t, int64(2399), rebill.AmountCents)
	require.Equal(t, "USD", rebill.Currency)
	require.Equal(t, time.Date(2026, 6, 1, 4, 5, 6, 0, time.UTC), rebill.OccurredAt)

	refund := snap.Transactions[1]
	require.Equal(t, TransactionTypeRefund, refund.Type)
	require.Equal(t, int64(2399), refund.AmountCents)
	require.Empty(t, refund.TransactionID) // REFUND rows carry no txn id

	chargeback := snap.Transactions[2]
	require.Equal(t, TransactionTypeChargeback, chargeback.Type)
	require.Contains(t, string(chargeback.Raw), "ccbill_datalink_export")
}

func TestCCBillFetcher_SubscriptionIDFilterAppliedClientSide(t *testing.T) {
	t.Parallel()

	dl := &fakeDataLink{
		active: []ccbill.CCBillRecord{
			{SubscriptionID: "111", Status: "1"},
			{SubscriptionID: "222", Status: "1"},
		},
		exportRows: []ccbill.DataLinkExportRow{
			exportRow("REBILL", "123", "x", "111", "2026-06-01", "5", "9.99"),
			exportRow("REBILL", "123", "x", "222", "2026-06-01", "6", "9.99"),
		},
	}
	snap, err := (&CCBillFetcher{DataLink: dl}).Fetch(context.Background(), FetchParams{SubscriptionID: "111"})
	require.NoError(t, err)
	require.Len(t, snap.Subscriptions, 1)
	require.Equal(t, "111", snap.Subscriptions[0].ProcessorSubscriptionID)
	require.Len(t, snap.Transactions, 1)
	require.Equal(t, "111", snap.Transactions[0].SubscriptionID)
}
