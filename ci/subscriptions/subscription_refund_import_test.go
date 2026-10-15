//go:build e2e && integration

package subscriptions_test

import (
	"context"
	"errors"
	"fmt"
	"github.com/open-rails/openrails/internal/billingimport"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/failpoint"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/reconcile"

	"testing"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/nmimock"
	"github.com/stretchr/testify/require"
)

func TestSubscriptionRefundImport(t *testing.T) {
	for _, tp := range []topology{embedded, remote} {
		for _, canceled := range []bool{false, true} {
			name := string(tp) + "/active"
			if canceled {
				name = string(tp) + "/canceled"
			}
			t.Run(name, func(t *testing.T) {
				w := newWorld(t)
				ctx, client := t.Context(), w.client[tp]
				tier := w.bookTier("refund-history", 999, 30)
				now := w.clock.Now()
				m := w.historyMember(tier, now.Add(10*day), 3)
				old := w.nmi.AddScheduleSale(m.schedule, m.started).TransactionID
				book := billing.DeclaredBilling{AsOf: now, DefaultPSP: billing.PSPRef{Key: "nmi"}}
				m.declare(&book, tier, billing.DeclaredTransaction{SourceID: "older", TransactionID: old, Success: true, Amount: 9_990_000, OccurredAt: m.started})
				book.Transactions[0].SourceID = "current"
				if canceled {
					book.Subscriptions[0].Cancel = billing.CancelEvidence{Kind: "user_canceled", At: now.Add(-day)}
				}
				refund := func(id, original string, amount int64) billing.DeclaredTransaction {
					return billing.DeclaredTransaction{SourceID: id, RailSubscriptionID: m.schedule, TransactionID: id, OriginalTransactionID: original,
						Type: "refund", Success: true, Amount: amount, Currency: "USD", OccurredAt: now.Add(-time.Hour)}
				}
				// Provider report order must not decide whether a refund can find its charge.
				book.Transactions = append([]billing.DeclaredTransaction{refund("9000000001", m.current, 1_000_000), refund("9000000002", m.current, 1_000_000), refund("9000000003", old, 9_990_000)}, book.Transactions...)
				calls := len(w.nmi.Calls())
				type outcome struct {
					result *billing.BillingImportResult
					err    error
				}
				done := make(chan outcome, 2)
				for range 2 {
					go func() { r, err := client.ImportBilling(ctx, book); done <- outcome{r, err} }()
				}
				imported := 0
				for range 2 {
					r := <-done
					require.NoError(t, r.err)
					require.Empty(t, r.result.Blocked, "%+v", r.result)
					imported += len(r.result.Imported)
				}
				require.Equal(t, 6, imported, "concurrent first imports create each source once")
				sub := w.subscriptionOf(m.c)
				status := billing.SubscriptionActive
				if canceled {
					status = billing.SubscriptionCanceled
				}
				require.Equal(t, status, sub.Status)
				require.True(t, sub.CurrentPeriodEndsAt.Equal(m.paid))
				require.Equal(t, 5, w.countRows(`SELECT count(*) FROM billing.payments`))
				require.Equal(t, 3, w.countRows(`SELECT count(*) FROM billing.payments r JOIN billing.payments p ON p.id = r.refunded_payment_id AND p.merchant_id = r.merchant_id WHERE r.subscription_id = p.subscription_id AND r.customer_id = p.customer_id AND r.psp_id = p.psp_id AND r.currency = p.currency AND r.purchased_at = $1`, now.Add(-time.Hour)))
				grants := w.countRows(`SELECT count(*) FROM billing.grants`)
				// The same book can race another import after a lost response.

				for range 2 {
					go func() { r, err := client.ImportBilling(ctx, book); done <- outcome{r, err} }()
				}
				for range 2 {
					r := <-done
					require.NoError(t, r.err)
					require.Empty(t, r.result.Blocked)
					require.Len(t, r.result.Skipped, 6)
				}
				// Discover the remainder later. The original payment and order stay put.
				book.Transactions = append(book.Transactions, refund("9000000004", m.current, 7_990_000))
				result, err := client.ImportBilling(ctx, book)
				require.NoError(t, err)
				require.Empty(t, result.Blocked)
				require.Equal(t, []string{"9000000004"}, result.Imported)
				require.Equal(t, 6, w.countRows(`SELECT count(*) FROM billing.payments`))
				require.Equal(t, grants, w.countRows(`SELECT count(*) FROM billing.grants`))
				require.Equal(t, status, w.subscriptionOf(m.c).Status)
				require.Equal(t, calls, len(w.nmi.Calls()), "importing refunds creates no provider request")
			})
		}
	}
}

func TestSubscriptionRefundImportRollsBackGroup(t *testing.T) {
	for _, current := range []bool{false, true} {
		name := "historical"
		if current {
			name = "current"
		}
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			ctx, client := t.Context(), w.client[embedded]
			tier := w.bookTier("refund-conflict", 999, 30)
			now := w.clock.Now()
			m := w.historyMember(tier, now.Add(10*day), 3)
			old := w.nmi.AddSale(nmimock.Sale{Vault: m.vault, Amount: "9.99", At: m.started}).TransactionID
			book := billing.DeclaredBilling{AsOf: now, DefaultPSP: billing.PSPRef{Key: "nmi"}}
			m.declare(&book, tier, billing.DeclaredTransaction{SourceID: "older", TransactionID: old, Success: true, Amount: 9_990_000, OccurredAt: m.started})
			book.Transactions[0].SourceID = "current"
			// This earlier historical group must stay "imported" after a later group's rollback.
			earlier := w.nmi.AddSale(nmimock.Sale{Vault: m.vault, Amount: "9.99", At: m.started.Add(-day)}).TransactionID
			book.Transactions = append(book.Transactions, billing.DeclaredTransaction{SourceID: "earlier", RailSubscriptionID: m.schedule, TransactionID: earlier, Success: true, Amount: 9_990_000, Currency: "USD", OccurredAt: m.started.Add(-day)})
			original, blocked, landed := old, "older", "current"
			if current {
				original, blocked, landed = m.current, "current", "older"
			}
			for _, id := range []string{"9000000001", "9000000002"} {
				book.Transactions = append(book.Transactions, billing.DeclaredTransaction{SourceID: id, RailSubscriptionID: m.schedule, TransactionID: id, OriginalTransactionID: original,
					Type: "refund", Success: true, Amount: 6_000_000, Currency: "USD", OccurredAt: now.Add(-time.Hour)})
			}
			result, err := client.ImportBilling(ctx, book)
			require.NoError(t, err)
			require.ElementsMatch(t, []string{blocked, "9000000001", "9000000002"}, result.Blocked, "%+v", result)
			require.Contains(t, result.Imported, landed)
			require.Contains(t, result.Imported, "earlier")
			require.Zero(t, w.countRows(`SELECT count(*) FROM billing.payments WHERE transaction_id IN ($1, '9000000001', '9000000002')`, original), "a second refund conflict rolls back the first refund and its new charge")
			require.Equal(t, 2, w.countRows(`SELECT count(*) FROM billing.payments`), "the unrelated receipts still land")
			book.Transactions[len(book.Transactions)-1].Amount = 3_990_000
			result, err = client.ImportBilling(ctx, book)
			require.NoError(t, err)
			require.Empty(t, result.Blocked, "%+v", result)
			require.Equal(t, 5, w.countRows(`SELECT count(*) FROM billing.payments`))
		})
	}
}

func TestSubscriptionRefundImportRecovery(t *testing.T) {
	w := newWorld(t)
	ctx, client := t.Context(), w.client[embedded]
	tier := w.bookTier("refund-recovery", 999, 30)
	now := w.clock.Now()
	m := w.historyMember(tier, now.Add(10*day), 3)
	old := w.nmi.AddScheduleSale(m.schedule, m.started).TransactionID
	book := billing.DeclaredBilling{AsOf: now, DefaultPSP: billing.PSPRef{Key: "nmi"}}
	m.declare(&book, tier, billing.DeclaredTransaction{SourceID: "older", TransactionID: old, Success: true, Amount: 9_990_000, OccurredAt: m.started})
	book.Transactions[0].SourceID = "current"
	for i, original := range []string{m.current, old} {
		id := []string{"9000000001", "9000000002"}[i]
		book.Transactions = append(book.Transactions, billing.DeclaredTransaction{SourceID: id, RailSubscriptionID: m.schedule, TransactionID: id, OriginalTransactionID: original,
			Type: "refund", Success: true, Amount: 1_000_000, Currency: "USD", OccurredAt: now})
	}
	remove := failpoint.Set(func(ctx context.Context, site failpoint.Site) error {
		mid, _ := merchant.FromContext(ctx)
		if mid == client.MerchantID() && site.Kind == billingimport.FailpointKind && site.Point == failpoint.Committed {
			return errors.New("connection lost after book commit")
		}
		return nil
	})
	t.Cleanup(remove)
	_, err := client.ImportBilling(ctx, book)
	remove()
	require.Error(t, err)
	require.Equal(t, 2, w.countRows(`SELECT count(*) FROM billing.payments`), "the current payment and refund committed together")
	result, err := client.ImportBilling(ctx, book)
	require.NoError(t, err)
	require.Empty(t, result.Blocked)
	require.Contains(t, result.Skipped, "current")
	require.Contains(t, result.Imported, "older")
	require.Equal(t, 4, w.countRows(`SELECT count(*) FROM billing.payments`))
	d, err := db.NewWithPGXPool(w.pool, w.schema)
	require.NoError(t, err)
	scoped := db.WithPSPID(merchant.WithID(ctx, client.MerchantID()), w.psp["nmi"].UUID())
	svc := payments.NewPaymentService(d, w.clock)
	original, err := svc.GetByPSPTransactionID(scoped, models.RailNMI, m.current)
	require.NoError(t, err)
	reserved, err := svc.ReserveRefund(scoped, original.ID, "pending-subscription-import", 1_000_000, nil)
	require.NoError(t, err)
	later := book.Transactions[2]
	later.SourceID, later.TransactionID = "9000000003", "9000000003"
	book.Transactions = append(book.Transactions, later)
	result, err = client.ImportBilling(ctx, book)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"current", "9000000001", "9000000003"}, result.Blocked)
	require.Contains(t, result.Reasons["current"], "in flight")
	require.Equal(t, 5, w.countRows(`SELECT count(*) FROM billing.payments`))
	_, err = svc.CompleteRefundReservation(scoped, reserved.ID, later.TransactionID, nil)
	require.NoError(t, err)
	result, err = client.ImportBilling(ctx, book)
	require.NoError(t, err)
	require.Empty(t, result.Blocked)
	require.Equal(t, 5, w.countRows(`SELECT count(*) FROM billing.payments`))
	// A replay with changed provider facts cannot silently reuse an earlier receipt.
	book.Transactions[2].OccurredAt = now.Add(-time.Minute)
	result, err = client.ImportBilling(ctx, book)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"current", "9000000001", "9000000003"}, result.Blocked)
	require.Equal(t, 5, w.countRows(`SELECT count(*) FROM billing.payments`))
	receipt, err := svc.GetByPSPTransactionID(scoped, models.RailNMI, "9000000001")
	require.NoError(t, err)
	require.True(t, receipt.PurchasedAt.Equal(now))
}

func TestSubscriptionRefundImportSharesReconciliation(t *testing.T) {
	for _, order := range []string{"import_first", "sync_first", "concurrent"} {
		t.Run(order, func(t *testing.T) {
			w := newWorld(t)
			client := w.client[embedded]
			tier := w.bookTier("refund-sync", 999, 30)
			now := w.clock.Now()
			m := w.historyMember(tier, now.Add(10*day), 3)
			old := w.nmi.AddScheduleSale(m.schedule, m.started).TransactionID
			book := billing.DeclaredBilling{AsOf: now, DefaultPSP: billing.PSPRef{Key: "nmi"}}
			m.declare(&book, tier, billing.DeclaredTransaction{SourceID: "older", TransactionID: old, Success: true, Amount: 9_990_000, OccurredAt: m.started})
			book.Transactions[0].SourceID = "current"
			_, err := client.ImportBilling(t.Context(), book)
			require.NoError(t, err)
			grants := w.countRows(`SELECT count(*) FROM billing.grants`)
			report := nmiRefundReport{}
			for i, original := range []string{m.current, old} {
				id := []string{"9000000001", "9000000002"}[i]
				book.Transactions = append(book.Transactions, billing.DeclaredTransaction{SourceID: id, RailSubscriptionID: m.schedule, TransactionID: id, OriginalTransactionID: original,
					Type: "refund", Success: true, Amount: 1_000_000, Currency: "USD", OccurredAt: now})
				report.transactions = append(report.transactions, reconcile.RemoteTransaction{TransactionID: id, OriginalTransactionID: original, Type: reconcile.TransactionTypeRefund, Success: true, AmountCents: -100, Currency: "USD", OccurredAt: now})
			}
			d, err := db.NewWithPGXPool(w.pool, w.schema)
			require.NoError(t, err)
			ctx := merchant.WithID(t.Context(), client.MerchantID())
			reader := reconcile.NewEngine(d, nil, nil, map[reconcile.Provider]reconcile.RailFetcher{reconcile.ProviderNMI: report}, nil)
			reader.Now = w.clock.Now
			reader.Writer = &reconcile.PGLocalWriter{DB: d, Now: w.clock.Now}
			sync := func() error {
				return d.RunInMerchantConn(ctx, func(ctx context.Context) error {
					result, err := reader.Run(ctx, reconcile.RunParams{Mode: reconcile.ModeEnforce, Mutations: &reconcile.LocalMutationPolicy{Insert: true}, Since: now.Add(-day), Until: now, PSPs: map[reconcile.Provider]reconcile.PSPBinding{reconcile.ProviderNMI: {ID: w.psp["nmi"].UUID(), Rail: "nmi", AccountID: nmiAcct}}})
					if err != nil {
						return err
					}
					for _, p := range result.Summary.Providers {
						if p.Error != "" || len(p.ApplyErrors) != 0 {
							return fmt.Errorf("reconciliation: %s %v", p.Error, p.ApplyErrors)
						}
					}
					return nil
				})
			}
			importReceipts := func() {
				result, err := client.ImportBilling(t.Context(), book)
				require.NoError(t, err)
				require.Empty(t, result.Blocked, "%+v", result)
			}
			switch order {
			case "import_first":
				importReceipts()
				require.NoError(t, sync())
			case "sync_first":
				require.NoError(t, sync())
				importReceipts()
			case "concurrent":
				done := make(chan error, 1)
				go func() { done <- sync() }()
				importReceipts()
				require.NoError(t, <-done)
			}
			require.Equal(t, 4, w.countRows(`SELECT count(*) FROM billing.payments`))
			require.Equal(t, grants, w.countRows(`SELECT count(*) FROM billing.grants`))
			require.Equal(t, billing.SubscriptionActive, w.subscriptionOf(m.c).Status)
			require.True(t, m.c.entitled(tier.ent))
		})
	}
}

func TestSubscriptionRefundImportRejectsAnotherCustomersReceipt(t *testing.T) {
	w := newWorld(t)
	w.waive("recorded", "the conflicting refund deliberately keeps its new original charge unimported")
	ctx, client := t.Context(), w.client[embedded]
	tier := w.bookTier("refund-owner", 999, 30)
	now := w.clock.Now()
	owner := w.newCustomer()
	sale := w.nmi.AddSale(nmimock.Sale{Vault: w.nmi.AddVault(visa), Amount: "9.99", At: now.Add(-day)})
	_, err := client.ImportBilling(ctx, billing.DeclaredBilling{AsOf: now, DefaultPSP: billing.PSPRef{Key: "nmi"}, HistoricalPayments: []billing.DeclaredHistoricalPayment{{
		SourceID: "existing", Customer: owner.cid(), Rail: "nmi", TransactionID: sale.TransactionID, Amount: 9_990_000, Currency: "USD", PaidAt: sale.At,
		Refunds: []billing.DeclaredHistoricalRefund{{TransactionID: "9000000001", Amount: 1_000_000, RefundedAt: now}},
	}}})
	require.NoError(t, err)
	m := w.historyMember(tier, now.Add(10*day), 3)
	book := billing.DeclaredBilling{AsOf: now, DefaultPSP: billing.PSPRef{Key: "nmi"}}
	m.declare(&book, tier)
	book.Transactions[0].SourceID = "current"
	book.Transactions = append(book.Transactions, billing.DeclaredTransaction{SourceID: "refund", RailSubscriptionID: m.schedule, TransactionID: "9000000001", OriginalTransactionID: m.current,
		Type: "refund", Success: true, Amount: 1_000_000, Currency: "USD", OccurredAt: now})
	result, err := client.ImportBilling(ctx, book)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"current", "refund"}, result.Blocked)
	require.Equal(t, 2, w.countRows(`SELECT count(*) FROM billing.payments`), "the refused group creates no new charge")
	page, err := client.ListPayments(ctx, billing.PaymentListParams{CustomerID: owner.cid(), TransactionID: sale.TransactionID})
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	require.Equal(t, int64(1_000_000), page.Items[0].AmountRefunded)
	rival := w.rival()
	book.DefaultPSP = billing.PSPRef{ID: new(w.psp["nmi"])}
	_, err = rival.client.ImportBilling(ctx, book)
	require.Error(t, err, "a different merchant cannot import through this PSP")
	require.Equal(t, 2, w.countRows(`SELECT count(*) FROM billing.payments`))
}
