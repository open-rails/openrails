package reconcile

import (
	"math"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/stretchr/testify/require"
)

func TestNMISeparateRefunds(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Second)
	original := LocalPayment{ID: uuid.New(), CustomerID: uuid.New(), Rail: "nmi", TransactionID: "12682585392", AmountNative: 4_230_000, Currency: "USD", Status: "succeeded", PurchasedAt: now.Add(-90 * 24 * time.Hour)}
	refund := RemoteTransaction{TransactionID: "12682586568", OriginalTransactionID: original.TransactionID, Type: TransactionTypeRefund, Success: true, AmountCents: -101, Currency: "USD", OccurredAt: now.Add(-time.Hour)}
	for _, tc := range []struct {
		name   string
		change func(*RemoteTransaction, *LocalPayment)
		valid  bool
	}{
		{"partial", func(*RemoteTransaction, *LocalPayment) {}, true},
		{"full", func(r *RemoteTransaction, _ *LocalPayment) { r.AmountCents = -423 }, true},
		{"positive amount", func(r *RemoteTransaction, _ *LocalPayment) { r.AmountCents = 101 }, true},
		{"currency precision", func(r *RemoteTransaction, p *LocalPayment) {
			r.Currency, p.Currency, p.AmountNative = "JPY", "JPY", 423_000_000
		}, true},
		{"wrong currency", func(r *RemoteTransaction, _ *LocalPayment) { r.Currency = "EUR" }, false},
		{"invalid precision", func(r *RemoteTransaction, _ *LocalPayment) { r.Amount = "-1.001" }, false},
		{"zero", func(r *RemoteTransaction, _ *LocalPayment) { r.AmountCents = 0 }, false},
		{"overflow", func(r *RemoteTransaction, _ *LocalPayment) { r.AmountCents = math.MinInt64 }, false},
		{"too much", func(r *RemoteTransaction, _ *LocalPayment) { r.AmountCents = -424 }, false},
		{"missing original", func(r *RemoteTransaction, _ *LocalPayment) { r.OriginalTransactionID = "missing" }, false},
		{"unpaid original", func(_ *RemoteTransaction, p *LocalPayment) { p.Status = "pending" }, false},
		{"original is reversal", func(_ *RemoteTransaction, p *LocalPayment) { p.RefundedPaymentID = new(uuid.New()) }, false},
		{"before original", func(r *RemoteTransaction, p *LocalPayment) { r.OccurredAt = p.PurchasedAt.Add(-time.Hour) }, false},
		{"future", func(r *RemoteTransaction, _ *LocalPayment) { r.OccurredAt = now.Add(time.Hour) }, false},
		{"usage payment", func(_ *RemoteTransaction, p *LocalPayment) { p.UsageOrderID = new(uuid.New()) }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, p := refund, original
			tc.change(&r, &p)
			snap := &RemoteSnapshot{Provider: ProviderNMI, Capabilities: Capabilities{Transactions: true, Refunds: true}, Transactions: []RemoteTransaction{r}}
			require.Contains(t, collectTxnLookupIDs(snap), r.OriginalTransactionID, "original is loaded even outside this refund-only report window")
			findings := diffProvider(ProviderNMI, snap, &LocalState{}, []LocalPayment{p}, now, diffOptions{})
			require.Len(t, findings, 1)
			if !tc.valid {
				require.Nil(t, findings[0].Apply)
				require.True(t, findings[0].RequiresAdmin)
				return
			}
			a := findings[0].Apply.RecordRefund
			require.Equal(t, p.ID, *a.RefundedPaymentID)
			require.Equal(t, r.TransactionID, a.TransactionID)
			require.Positive(t, a.AmountCents)
			require.True(t, r.OccurredAt.Equal(a.PurchasedAt))
			amount, err := moneyutil.RailMinorToNative(r.Currency, moneyutil.Cents(a.AmountCents))
			require.NoError(t, err)
			proposal := findings[0].Change.To["refund"].(map[string]any)
			require.Equal(t, strconv.FormatInt(amount, 10), proposal["amount"], "the reviewed amount must match the writer")
		})
	}
	prior := LocalPayment{ID: uuid.New(), CustomerID: original.CustomerID, TransactionID: refund.TransactionID, RefundedPaymentID: &original.ID, AmountNative: -1_010_000, Currency: "USD", Status: "succeeded", ReversalKind: payments.ReversalRefund, PurchasedAt: refund.OccurredAt}
	for _, tc := range []struct {
		name    string
		change  func(*LocalPayment)
		matches bool
	}{
		{"same receipt", func(*LocalPayment) {}, true},
		{"different original", func(p *LocalPayment) { p.RefundedPaymentID = new(uuid.New()) }, false},
		{"different customer", func(p *LocalPayment) { p.CustomerID = uuid.New() }, false},
		{"different amount", func(p *LocalPayment) { p.AmountNative-- }, false},
		{"different date", func(p *LocalPayment) { p.PurchasedAt = p.PurchasedAt.Add(time.Hour) }, false},
		{"different currency", func(p *LocalPayment) { p.Currency = "EUR" }, false},
		{"chargeback", func(p *LocalPayment) { p.ReversalKind = payments.ReversalChargeback }, false},
		{"pending", func(p *LocalPayment) { p.Status = "pending" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := prior
			tc.change(&p)
			f, found := makeNMIRefund(&refund, map[string]*LocalPayment{original.TransactionID: &original, p.TransactionID: &p}, now)
			require.Equal(t, !tc.matches, found)
			require.Nil(t, f.Apply)
		})
	}
	second := refund
	second.TransactionID = "12682589153"
	snap := &RemoteSnapshot{Provider: ProviderNMI, Capabilities: Capabilities{Transactions: true, Refunds: true}, Transactions: []RemoteTransaction{refund, second}}
	findings := diffProvider(ProviderNMI, snap, &LocalState{}, []LocalPayment{original, prior}, now, diffOptions{})
	require.Len(t, findings, 1, "one recorded partial must not hide another equal partial")
	require.Equal(t, second.TransactionID, findings[0].Apply.RecordRefund.TransactionID)
}

func TestNMIAmbiguousRefundsRequireReview(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Second)
	for _, tc := range []struct {
		name, refundID, originalID string
	}{
		{"same ID", "123456", "123456"},
		{"same ID without original link", "123456", ""},
		{"separate ID without original link", "654321", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, status := range []string{"succeeded", "refunded"} {
				t.Run(status, func(t *testing.T) {
					original := LocalPayment{ID: uuid.New(), CustomerID: uuid.New(), Rail: "nmi", TransactionID: "123456", AmountNative: 4_230_000, AmountCents: 423, Currency: "USD", Status: status, PurchasedAt: now.Add(-time.Hour)}
					refund := RemoteTransaction{TransactionID: tc.refundID, OriginalTransactionID: tc.originalID, Type: TransactionTypeRefund, Success: true, AmountCents: 101, Currency: "USD", OccurredAt: now,
						Raw: rawJSON(map[string]string{"charge": original.TransactionID})}
					second := refund
					second.OccurredAt = now.Add(-time.Minute)
					snapshot := &RemoteSnapshot{Provider: ProviderNMI, Capabilities: Capabilities{Transactions: true, Refunds: true}, Transactions: []RemoteTransaction{refund, second}}
					findings := diffProvider(ProviderNMI, snapshot, &LocalState{}, []LocalPayment{original}, now, diffOptions{})
					require.Len(t, findings, 2, "neither an existing payment nor a refund status qualifies ambiguous receipts")
					for _, finding := range findings {
						require.Equal(t, FindingStatusRequiresReview, finding.Status)
						require.True(t, finding.RequiresAdmin)
						require.Nil(t, finding.Apply)
						require.Equal(t, tc.refundID, finding.RemoteEvidence["transaction_id"])
					}
					snapshot.Transactions = []RemoteTransaction{refund}
					snapshot.Transactions[0].Success = false
					require.Empty(t, diffProvider(ProviderNMI, snapshot, &LocalState{}, []LocalPayment{original}, now, diffOptions{}))
				})
			}
		})
	}
}
