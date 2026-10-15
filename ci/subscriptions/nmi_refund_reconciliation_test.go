//go:build e2e && integration

package subscriptions_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/nmimock"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/stretchr/testify/require"
)

type nmiRefundReport struct{ transactions []reconcile.RemoteTransaction }

func (nmiRefundReport) Name() string { return "nmi" }
func (nmiRefundReport) Capabilities() reconcile.Capabilities {
	return reconcile.Capabilities{Transactions: true, Refunds: true}
}
func (r nmiRefundReport) Fetch(context.Context, reconcile.FetchParams) (*reconcile.RemoteSnapshot, error) {
	return &reconcile.RemoteSnapshot{Provider: reconcile.ProviderNMI, Capabilities: r.Capabilities(), Transactions: append([]reconcile.RemoteTransaction(nil), r.transactions...)}, nil
}

func TestNMIRefundReconciliationSharesImportedReceipts(t *testing.T) {
	for _, order := range []string{"import_first", "sync_first", "concurrent"} {
		t.Run(order, func(t *testing.T) {
			w := newWorld(t)
			client, buyer := w.client[embedded], w.newCustomer()
			d, err := db.NewWithPGXPool(w.pool, w.schema)
			require.NoError(t, err)
			ctx := merchant.WithID(t.Context(), client.MerchantID())
			psp := w.psp["nmi"].UUID()
			paid := w.clock.Now().Add(-90 * day)
			sale := w.nmi.AddSale(nmimock.Sale{Vault: w.nmi.AddVault(visa), Amount: "4.23", At: paid})
			p := billing.DeclaredHistoricalPayment{SourceID: "old-sale", Customer: buyer.cid(), Rail: "nmi", TransactionID: sale.TransactionID, Amount: 4_230_000, Currency: "USD", PaidAt: paid}
			book := billing.DeclaredBilling{AsOf: w.clock.Now(), DefaultPSP: billing.PSPRef{Key: "nmi"}, HistoricalPayments: []billing.DeclaredHistoricalPayment{p}}
			_, err = client.ImportBilling(t.Context(), book)
			require.NoError(t, err)
			refunds := []billing.DeclaredHistoricalRefund{
				{TransactionID: "12682586568", Amount: 1_010_000, RefundedAt: w.clock.Now().Add(-2 * time.Hour)},
				{TransactionID: "12682589153", Amount: 1_010_000, RefundedAt: w.clock.Now().Add(-time.Hour)},
			}
			book.HistoricalPayments[0].Refunds = refunds
			report := nmiRefundReport{}
			for _, r := range refunds {
				report.transactions = append(report.transactions, reconcile.RemoteTransaction{TransactionID: r.TransactionID, OriginalTransactionID: p.TransactionID, Type: reconcile.TransactionTypeRefund, Success: true, AmountCents: -r.Amount / 10_000, Currency: "USD", OccurredAt: r.RefundedAt})
			}
			reader := reconcile.NewEngine(d, nil, nil, map[reconcile.Provider]reconcile.RailFetcher{reconcile.ProviderNMI: report}, nil)
			reader.Now = w.clock.Now
			reader.Writer = &reconcile.PGLocalWriter{DB: d, Now: w.clock.Now}
			sync := func() error {
				return d.RunInMerchantConn(ctx, func(ctx context.Context) error {
					result, err := reader.Run(ctx, reconcile.RunParams{Mode: reconcile.ModeEnforce, Mutations: &reconcile.LocalMutationPolicy{Insert: true}, Since: w.clock.Now().Add(-day), Until: w.clock.Now(), PSPs: map[reconcile.Provider]reconcile.PSPBinding{reconcile.ProviderNMI: {ID: psp, Rail: "nmi", AccountID: nmiAcct}}})
					if err != nil {
						return err
					}
					for _, provider := range result.Summary.Providers {
						if provider.Error != "" || len(provider.ApplyErrors) > 0 {
							return fmt.Errorf("reconciliation failed: %s %v", provider.Error, provider.ApplyErrors)
						}
					}
					return nil
				})
			}
			importReceipts := func() error {
				res, err := client.ImportBilling(t.Context(), book)
				if err == nil {
					require.Empty(t, res.Blocked)
				}
				return err
			}
			switch order {
			case "import_first":
				require.NoError(t, importReceipts())
				require.NoError(t, sync())
			case "sync_first":
				require.NoError(t, sync())
				require.Equal(t, 3, w.countRows(`SELECT count(*) FROM billing.payments`))
				require.NoError(t, importReceipts())
			case "concurrent":
				done := make(chan error, 1)
				go func() { done <- sync() }()
				require.NoError(t, importReceipts())
				require.NoError(t, <-done)
			}
			require.NoError(t, sync())
			require.Equal(t, 3, w.countRows(`SELECT count(*) FROM billing.payments`))
			require.Equal(t, 1, w.countRows(`SELECT count(*) FROM billing.orders`))
			svc := payments.NewPaymentService(d, w.clock)
			scoped := db.WithPSPID(ctx, psp)
			original, err := svc.GetByPSPTransactionID(scoped, models.RailNMI, p.TransactionID)
			require.NoError(t, err)
			for _, r := range refunds {
				receipt, err := svc.GetByPSPTransactionID(scoped, models.RailNMI, r.TransactionID)
				require.NoError(t, err)
				require.Equal(t, original.ID, *receipt.RefundedPaymentID)
				require.Equal(t, -r.Amount, receipt.Amount)
				require.True(t, r.RefundedAt.Equal(receipt.PurchasedAt))
			}
			writer := &reconcile.PGLocalWriter{DB: d, Now: w.clock.Now}
			action := reconcile.RecordRefundAction{PspID: &psp, Rail: "nmi", TransactionID: refunds[0].TransactionID, AmountCents: 101, Currency: "USD", PurchasedAt: refunds[0].RefundedAt.Add(time.Minute), RefundedPaymentID: &original.ID, CustomerID: original.CustomerID}
			_, err = writer.RecordRefund(ctx, action)
			require.Error(t, err, "a receipt inserted after the snapshot cannot silently absorb a different date")
			// The next refund completes the payment. An unresolved local refund
			// must first be settled, not counted again as a provider observation.
			third := reconcile.RemoteTransaction{TransactionID: "12682590253", OriginalTransactionID: p.TransactionID, Type: reconcile.TransactionTypeRefund, Success: true, AmountCents: -221, Currency: "USD", OccurredAt: w.clock.Now()}
			report.transactions = append(report.transactions, third)
			reader.Fetchers[reconcile.ProviderNMI] = report
			if order == "sync_first" {
				reserved, err := svc.ReserveRefund(scoped, original.ID, "pending-refund", 2_210_000, nil)
				require.NoError(t, err)
				require.ErrorContains(t, sync(), "in flight")
				require.Equal(t, 4, w.countRows(`SELECT count(*) FROM billing.payments`))
				_, err = svc.CompleteRefundReservation(scoped, reserved.ID, third.TransactionID, nil)
				require.NoError(t, err)
			}
			require.NoError(t, sync())
			require.NoError(t, sync())
			require.Equal(t, 4, w.countRows(`SELECT count(*) FROM billing.payments`))
			total, err := svc.GetRefundTotalByPaymentID(scoped, original.ID)
			require.NoError(t, err)
			require.Equal(t, p.Amount, total)
			if order == "sync_first" {
				loader := &reconcile.PGLocalStateLoader{DB: d}
				ids := []string{p.TransactionID, refunds[0].TransactionID}
				own, err := loader.PaymentsByTransactionIDs(ctx, reconcile.ProviderNMI, psp, ids)
				require.NoError(t, err)
				require.Len(t, own, 2)
				otherPSP, err := loader.PaymentsByTransactionIDs(ctx, reconcile.ProviderNMI, w.psp["stripe"].UUID(), ids)
				require.NoError(t, err)
				require.Empty(t, otherPSP)
				rival := w.rival()
				otherMerchant, err := loader.PaymentsByTransactionIDs(merchant.WithID(t.Context(), rival.client.MerchantID()), reconcile.ProviderNMI, psp, ids)
				require.NoError(t, err)
				require.Empty(t, otherMerchant)
			}
			for _, table := range []string{"subscriptions", "grants", "provider_intents"} {
				require.Zero(t, w.countRows("SELECT count(*) FROM billing."+table), table)
			}
		})
	}
}
