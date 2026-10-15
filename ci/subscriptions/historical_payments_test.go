//go:build e2e && integration

package subscriptions_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingimport"
	"github.com/open-rails/openrails/internal/failpoint"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/nmimock"
	"github.com/stretchr/testify/require"
)

func TestHistoricalPaymentsWithoutSubscription(t *testing.T) {
	t.Parallel()
	for _, tp := range []topology{embedded, remote} {
		t.Run(string(tp), func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			ctx, client := t.Context(), w.client[tp]
			buyer, other := w.newCustomer(), w.newCustomer()
			paid := w.clock.Now().Add(-90 * day)
			sale := w.nmi.AddSale(nmimock.Sale{Vault: w.nmi.AddVault(visa), Amount: "4.99", At: paid})
			calls := len(w.nmi.Calls())
			p := billing.DeclaredHistoricalPayment{SourceID: "old-payment", Customer: buyer.cid(), Rail: "nmi", TransactionID: sale.TransactionID, Amount: 4_990_000, Currency: "USD", PaidAt: paid}
			book := func(p billing.DeclaredHistoricalPayment) billing.DeclaredBilling {
				return billing.DeclaredBilling{AsOf: w.clock.Now(), DefaultPSP: billing.PSPRef{Key: "nmi"}, HistoricalPayments: []billing.DeclaredHistoricalPayment{p}}
			}
			if tp == embedded {
				lost := errors.New("connection lost after the book committed")
				hit := false
				remove := failpoint.Set(func(ctx context.Context, site failpoint.Site) error {
					mid, _ := merchant.FromContext(ctx)
					if mid == client.MerchantID() && site.Kind == billingimport.FailpointKind && site.Point == failpoint.Committed {
						hit = true
						return lost
					}
					return nil
				})
				t.Cleanup(remove)
				_, err := client.ImportBilling(ctx, book(p))
				remove()
				require.ErrorContains(t, err, "billing import failed")
				require.True(t, hit, "the failure occurred after the book transaction committed")
				require.Equal(t, 0, w.countRows(`SELECT count(*) FROM billing.payments`))
			}
			// Simultaneous imports represent two jobs importing the same receipt.
			type result struct {
				value *billing.BillingImportResult
				err   error
			}
			done := make(chan result, 4)
			for range 4 {
				go func() { v, err := client.ImportBilling(ctx, book(p)); done <- result{v, err} }()
			}
			created, skipped := 0, 0
			for range 4 {
				r := <-done
				require.NoError(t, r.err)
				require.Empty(t, r.value.Blocked)
				created += len(r.value.Imported)
				skipped += len(r.value.Skipped)
			}
			require.Equal(t, 1, created)
			require.Equal(t, 3, skipped)
			page, err := client.ListPayments(ctx, billing.PaymentListParams{CustomerID: buyer.cid()})
			require.NoError(t, err)
			require.Len(t, page.Items, 1)
			payment := page.Items[0]
			require.Equal(t, sale.TransactionID, payment.TransactionID)
			require.Equal(t, p.Amount, payment.Amount)
			require.Equal(t, p.Currency, payment.Currency)
			require.True(t, paid.Equal(payment.CreatedAt), "customer history shows the payment date, not the import date")
			require.Nil(t, payment.SubscriptionID)
			require.NotNil(t, payment.OrderID)
			require.Equal(t, 1, w.countRows(`SELECT count(*) FROM billing.orders WHERE customer_id = $1 AND status = 'complete' AND subscription_id IS NULL`, buyer.id))
			require.Equal(t, 1, w.countRows(`SELECT count(*) FROM billing.payments WHERE customer_id = $1 AND purchased_at = $2`, buyer.id, paid))
			require.Equal(t, 0, w.countRows(`SELECT count(*) FROM billing.order_lines WHERE customer_id = $1 AND (product_id IS NOT NULL OR price_id IS NOT NULL OR subscription_id IS NOT NULL)`, buyer.id))
			for _, table := range []string{"subscriptions", "grants", "provider_intents"} {
				require.Equal(t, 0, w.countRows("SELECT count(*) FROM billing."+table), table)
			}
			// A new job's record ID is not a new payment, including after the
			// first caller lost its response and therefore retries the import.
			p.SourceID = "another-import-record"
			replayed, err := client.ImportBilling(ctx, book(p))
			require.NoError(t, err)
			require.Equal(t, []string{p.SourceID}, replayed.Skipped)
			for _, tc := range []struct {
				name   string
				change func(*billing.DeclaredHistoricalPayment)
			}{
				{"wrong customer", func(p *billing.DeclaredHistoricalPayment) { p.Customer = other.cid() }},
				{"changed amount", func(p *billing.DeclaredHistoricalPayment) { p.Amount += 1_000_000 }},
				{"changed currency", func(p *billing.DeclaredHistoricalPayment) { p.Currency = "EUR" }},
				{"changed date", func(p *billing.DeclaredHistoricalPayment) { p.PaidAt = p.PaidAt.Add(time.Hour) }},
			} {
				t.Run(tc.name, func(t *testing.T) {
					changed := p
					tc.change(&changed)
					res, err := client.ImportBilling(ctx, book(changed))
					require.NoError(t, err)
					require.Equal(t, []string{p.SourceID}, res.Blocked)
					require.Contains(t, res.Reasons[p.SourceID], "different customer or payment facts")
				})
			}
			require.Equal(t, 1, w.countRows(`SELECT count(*) FROM billing.payments`))
			require.Equal(t, 1, w.countRows(`SELECT count(*) FROM billing.orders`))
			require.Len(t, w.nmi.Calls(), calls, "import never writes to the provider")
		})
	}
}

func TestHistoricalPaymentsStayWithinMerchantAndPSP(t *testing.T) {
	t.Parallel()
	w := prepareWorld(t, 12)
	w.declare = declareB("nmi")
	w.start()
	ctx := t.Context()
	buyer := w.newCustomer()
	sale := w.nmi.AddSale(nmimock.Sale{Vault: w.nmi.AddVault(visa), Amount: "4.99", At: w.clock.Now().Add(-day)})
	p := billing.DeclaredHistoricalPayment{SourceID: "history", Customer: buyer.cid(), Rail: "nmi", TransactionID: sale.TransactionID, Amount: 4_990_000, Currency: "USD", PaidAt: sale.At}
	book := billing.DeclaredBilling{AsOf: w.clock.Now(), DefaultPSP: billing.PSPRef{Key: "nmi"}, HistoricalPayments: []billing.DeclaredHistoricalPayment{p}}
	client := w.client[remote]
	res, err := client.ImportBilling(ctx, book)
	require.NoError(t, err)
	require.Equal(t, []string{p.SourceID}, res.Imported)
	book.DefaultPSP = billing.PSPRef{Key: "mobius"}
	res, err = client.ImportBilling(ctx, book)
	require.NoError(t, err)
	require.Equal(t, []string{p.SourceID}, res.Imported, "a different provider account has its own transaction IDs")
	page, err := client.ListPayments(ctx, billing.PaymentListParams{CustomerID: buyer.cid()})
	require.NoError(t, err)
	require.Len(t, page.Items, 2)
	require.NotEqual(t, page.Items[0].PSPID, page.Items[1].PSPID)

	rival := w.rival()
	book.DefaultPSP = billing.PSPRef{ID: new(w.psp["nmi"])}
	_, err = rival.client.ImportBilling(ctx, book)
	require.Error(t, err, "another merchant cannot name our provider account")
	book.DefaultPSP = billing.PSPRef{Key: "nmi"}
	res, err = rival.client.ImportBilling(ctx, book)
	require.NoError(t, err)
	require.Equal(t, []string{p.SourceID}, res.Imported)
	other, err := rival.client.ListPayments(ctx, billing.PaymentListParams{CustomerID: buyer.cid()})
	require.NoError(t, err)
	require.Len(t, other.Items, 1)
	for _, own := range page.Items {
		require.NotEqual(t, own.ID, other.Items[0].ID)
	}
	replay, err := rival.client.ImportBilling(ctx, book)
	require.NoError(t, err)
	require.Equal(t, []string{p.SourceID}, replay.Skipped)
}
