//go:build e2e && integration

package subscriptions_test

import (
	"bytes"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchantarchive"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/nmimock"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"
)

func observedInvoice(t *testing.T, w *world, c *customer, amount int64) billing.InvoiceID {
	t.Helper()
	_, err := w.client[remote].SetCreditLimit(t.Context(), c.cid(), billing.SetCreditLimitParams{Currency: "USD", Amount: amount * 2})
	require.NoError(t, err)
	_, err = recordUsage(t.Context(), w.client[remote], billing.RecordUsageParams{CustomerID: c.cid(), Invoker: c.id, Currency: "USD", EventType: "observed-invoice", Amount: amount, Source: "test", SourceID: uuid.NewString()})
	require.NoError(t, err)
	w.advance(time.Minute)
	job, err := w.jobs.Insert(t.Context(), invoicePass{}, &river.InsertOpts{Queue: "billing"})
	require.NoError(t, err)
	w.waitJob(job.Job.ID)
	invoices, err := w.client[remote].ListInvoices(t.Context(), billing.InvoiceListParams{CustomerID: c.cid()})
	require.NoError(t, err)
	for _, invoice := range invoices.Items {
		if invoice.AmountDue == amount {
			return invoice.ID
		}
	}
	t.Fatal("normal invoice worker did not finalize the expected receivable")
	return billing.InvoiceID{}
}

// These are real provider receipts which the restored local facts cannot safely
// allocate. Holding them must leave the receivable, ledger and provider alone.
func TestObservedInvoiceRecoveryRefusesContradictoryFacts(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"wrong_payer", "unknown_payer", "wrong_account", "wrong_currency", "partial_amount", "overpayment", "missing_invoice", "before_invoice", "refund", "multiple_sales", "void_invoice", "already_paid"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			w.waive("recorded", "this provider receipt is deliberately unallocatable and must remain held")
			c := w.newCustomer()
			method := c.saveCard("nmi", visa)
			invoice := observedInvoice(t, w, c, 50_000_000)
			mid := w.client[embedded].MerchantID()
			ctx := merchant.WithID(t.Context(), mid)
			rt := engine.Graph(w.rt).Runtime
			sale := nmimock.Sale{OrderID: uuid.NewString(), OrderDescription: "invoice " + invoice.UUID().String(), Vault: w.vaultOf(method), Currency: "USD", Amount: "50.00", At: w.clock.Now()}
			switch name {
			case "wrong_payer":
				other := w.newCustomer()
				sale.Vault = w.vaultOf(other.saveCard("nmi", visa))
			case "unknown_payer":
				sale.Vault = w.nmi.AddVault(visa)
			case "wrong_currency":
				sale.Currency = "EUR"
			case "partial_amount":
				sale.Amount = "25.00"
			case "overpayment":
				sale.Amount = "50.01"
			case "missing_invoice":
				sale.OrderDescription = "invoice " + uuid.NewString()
			case "before_invoice":
				sale.At = w.clock.Now().Add(-24 * time.Hour)
			case "void_invoice":
				_, err := w.client[remote].VoidInvoice(t.Context(), invoice)
				require.NoError(t, err)
			case "already_paid":
				_, err := w.client[remote].CreateInvoicePayment(t.Context(), invoice, billing.CreateInvoicePaymentParams{Amount: 50_000_000, Reference: "already-remitted"})
				require.NoError(t, err)
			}
			sale = w.nmi.AddSale(sale)
			if name == "refund" {
				w.nmi.Refund(sale.TransactionID, 100)
			}
			if name == "multiple_sales" {
				other := sale
				other.TransactionID, other.OrderID = "", uuid.NewString()
				w.nmi.AddSale(other)
			}
			before, err := w.client[remote].GetInvoice(t.Context(), invoice)
			require.NoError(t, err)
			beforePayments, err := w.client[remote].ListInvoicePayments(t.Context(), invoice, billing.PageRequest{})
			require.NoError(t, err)
			beforeWrites := len(w.nmi.Attempts())
			psp := w.psp["nmi"].UUID()
			if name == "wrong_account" {
				psp = w.psp["stripe"].UUID()
			}
			receipt, err := money.ReadObservedNMIInvoiceReceipt(ctx, rt.CollectionResolver, mid.UUID(), psp, sale.TransactionID)
			if err == nil {
				_, err = rt.MoneyService.RecoverObservedInvoicePayment(ctx, receipt)
			}
			require.Error(t, err, "contradictory history must not create a receipt")
			after, err := w.client[remote].GetInvoice(t.Context(), invoice)
			require.NoError(t, err)
			require.Equal(t, before, after)
			afterPayments, err := w.client[remote].ListInvoicePayments(t.Context(), invoice, billing.PageRequest{})
			require.NoError(t, err)
			require.Equal(t, beforePayments, afterPayments)
			var allocations int
			require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT count(*) FROM billing.ledger_transfers WHERE operation='invoice_payment' AND invoice_id=$1`), invoice.UUID()).Scan(&allocations))
			require.Zero(t, allocations)
			require.Len(t, w.nmi.Attempts(), beforeWrites)
		})
	}
}

func TestObservedInvoiceRecoveryRefusesExistingSubscriptionAllocation(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	c := w.newCustomer()
	method := c.saveCard("nmi", visa)
	invoice := observedInvoice(t, w, c, 50_000_000)
	vault := w.vaultOf(method)
	sale := w.nmi.AddSale(nmimock.Sale{OrderID: uuid.NewString(), OrderDescription: "invoice " + invoice.UUID().String(), Vault: vault, Amount: "50.00"})
	// A trusted import already allocated this provider transaction elsewhere.
	// Recovery must refuse the known conflict, not allocate the money twice.
	tier := w.bookTier("allocated-receipt", 5000, 30)
	paidThrough := w.clock.Now().Add(30 * 24 * time.Hour)
	schedule := w.nmi.AddSchedule(nmimock.Schedule{Vault: vault, Plan: tier.plan, Amount: "50.00", NextBilling: paidThrough})
	_, err := w.client[remote].ImportBilling(t.Context(), billing.DeclaredBilling{
		AsOf: w.clock.Now(), DefaultPSP: billing.PSPRef{Key: "nmi"},
		Subscriptions: []billing.DeclaredSubscription{{SourceID: "already-allocated", Customer: c.cid(), Price: tier.price.ID, Rail: "nmi", RailSubscriptionID: schedule, StartedAt: w.clock.Now(), PaidThrough: &paidThrough, PaymentMethod: &billing.PaymentMethodRef{Rail: "nmi", RailCustomerRef: vault, RailMethodRef: w.nmi.Vault(vault).BillingID}}},
		Transactions:  []billing.DeclaredTransaction{{RailSubscriptionID: schedule, TransactionID: sale.TransactionID, Success: true, Amount: 50_000_000, Currency: "USD", OccurredAt: w.clock.Now()}},
	})
	require.NoError(t, err)
	mid := w.client[embedded].MerchantID()
	ctx := merchant.WithID(t.Context(), mid)
	rt := engine.Graph(w.rt).Runtime
	receipt, err := money.ReadObservedNMIInvoiceReceipt(ctx, rt.CollectionResolver, mid.UUID(), w.psp["nmi"].UUID(), sale.TransactionID)
	require.NoError(t, err)
	_, err = rt.MoneyService.RecoverObservedInvoicePayment(ctx, receipt)
	require.ErrorIs(t, err, money.ErrInvoiceRecoveryHeld)
	require.ErrorContains(t, err, "already belongs to another payment")
	stillDue, err := w.client[remote].GetInvoice(t.Context(), invoice)
	require.NoError(t, err)
	require.Equal(t, int64(50_000_000), stillDue.AmountDue)
	payments, err := w.client[remote].ListInvoicePayments(t.Context(), invoice, billing.PageRequest{})
	require.NoError(t, err)
	require.Empty(t, payments.Items)
	require.Empty(t, w.nmi.Attempts())
}

func TestObservedInvoiceRecoveryReplayChecksActualLedgerBinding(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	c := w.newCustomer()
	method := c.saveCard("nmi", visa)
	invoice := observedInvoice(t, w, c, 50_000_000)
	sale := w.nmi.AddSale(nmimock.Sale{OrderID: uuid.NewString(), OrderDescription: "invoice " + invoice.UUID().String(), Vault: w.vaultOf(method), Amount: "50.00"})
	mid := w.client[embedded].MerchantID()
	ctx := merchant.WithID(t.Context(), mid)
	rt := engine.Graph(w.rt).Runtime
	receipt, err := money.ReadObservedNMIInvoiceReceipt(ctx, rt.CollectionResolver, mid.UUID(), w.psp["nmi"].UUID(), sale.TransactionID)
	require.NoError(t, err)
	_, err = rt.MoneyService.RecoverObservedInvoicePayment(ctx, receipt)
	require.NoError(t, err)

	otherInvoice := observedInvoice(t, w, c, 50_000_000)
	_, err = w.client[remote].CreateInvoicePayment(t.Context(), otherInvoice, billing.CreateInvoicePaymentParams{Amount: 50_000_000, Reference: "other-invoice-bank-payment"})
	require.NoError(t, err)
	// Deliberately corrupt the restored linkage without changing the amount or
	// receipt ID. A nonnil transfer pointer alone must not bless this archive.
	var wrongTransfer uuid.UUID
	require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT ledger_transfer_id FROM billing.invoice_payments WHERE invoice_id=$1`), otherInvoice.UUID()).Scan(&wrongTransfer))
	_, err = w.pool.Exec(t.Context(), w.q(`UPDATE billing.invoice_payments SET ledger_transfer_id=NULL WHERE invoice_id=$1`), otherInvoice.UUID())
	require.NoError(t, err)
	_, err = w.pool.Exec(t.Context(), w.q(`UPDATE billing.invoice_payments SET ledger_transfer_id=$1 WHERE invoice_id=$2`), wrongTransfer, invoice.UUID())
	require.NoError(t, err)
	_, err = rt.MoneyService.RecoverObservedInvoicePayment(ctx, receipt)
	require.ErrorIs(t, err, money.ErrInvoiceRecoveryHeld)
	var allocations int
	require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT count(*) FROM billing.ledger_transfers WHERE operation='invoice_payment' AND invoice_id=$1`), invoice.UUID()).Scan(&allocations))
	require.Equal(t, 1, allocations)
	require.Empty(t, w.nmi.Attempts())
}

// This is an actual old archive: the invoice and card existed at backup time,
// then the source collected and stopped. Only the provider remembers that later
// payment in the destination; no accepted operation is synthesized there.
func TestObservedInvoiceRecoveryFromOldBackup(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name           string
		amount, manual int64
		uncollectible  bool
	}{
		{"full", 50_000_000, 0, false},
		{"rounding", 50_000_001, 0, false},
		{"retained_manual_partial", 75_000_001, 25_000_000, false},
		{"uncollectible_paid_by_customer", 50_000_000, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			source := newWorld(t)
			c := source.newCustomer()
			method := c.saveCard("nmi", visa)
			invoice := observedInvoice(t, source, c, tc.amount)
			if tc.manual > 0 {
				_, err := source.client[remote].CreateInvoicePayment(t.Context(), invoice, billing.CreateInvoicePaymentParams{Amount: tc.manual, Reference: "retained-bank-payment"})
				require.NoError(t, err)
			}
			if tc.uncollectible {
				_, err := source.client[remote].MarkInvoiceUncollectible(t.Context(), invoice)
				require.NoError(t, err)
			}
			mid := source.client[embedded].MerchantID()
			source.refreshProviders()
			source.settleCollectionScans()
			source.settle()
			source.stop()
			sourceDB, err := db.NewWithPGXPool(source.pool, source.schema)
			require.NoError(t, err)
			var archive bytes.Buffer
			require.NoError(t, merchantarchive.Export(t.Context(), sourceDB, mid, &archive))
			source.start()
			status, body := c.call(http.MethodPost, "/invoices/"+invoice.String()+"/pay-now", "after-backup", map[string]string{"payment_method_id": method})
			require.Equal(t, http.StatusOK, status, body)
			source.settle()
			require.Len(t, source.nmi.ledger(""), 1)
			transaction := source.nmi.ledger("")[0].ID
			sourceRuntime := engine.Graph(source.rt).Runtime
			sourceContext := merchant.WithID(t.Context(), mid)
			sourceReceipt, err := money.ReadObservedNMIInvoiceReceipt(sourceContext, sourceRuntime.CollectionResolver, mid.UUID(), source.psp["nmi"].UUID(), transaction)
			require.NoError(t, err)
			_, err = sourceRuntime.MoneyService.RecoverObservedInvoicePayment(sourceContext, sourceReceipt)
			var owned *money.InvoiceRecoveryOperationOwned
			require.ErrorAs(t, err, &owned, "retained accepted operation keeps its canonical authority")
			source.stop()

			target := prepareWorldAtDSN(t, 12, providerCopyDatabase(t, dsn(t)))
			target.slug, target.auth, target.nmi, target.stripe = source.slug, source.auth, source.nmi, source.stripe
			target.clock = source.clock
			target.cfg = func(cfg *config.Config) { cfg.ProviderWriteMode = config.ProviderWriteModeReadOnly }
			targetDB, err := db.NewWithPGXPool(target.pool, target.schema)
			require.NoError(t, err)
			directory, err := merchants.NewDirectoryService(targetDB.DataPool())
			require.NoError(t, err)
			_, _, err = directory.RegisterForRestore(t.Context(), mid, target.slug)
			require.NoError(t, err)
			_, err = merchantarchive.Restore(t.Context(), targetDB, mid, bytes.NewReader(archive.Bytes()))
			require.NoError(t, err)
			target.advance(5 * 24 * time.Hour)
			target.start()
			ctx := merchant.WithID(t.Context(), mid)
			rt := engine.Graph(target.rt).Runtime
			var operations int
			require.NoError(t, target.pool.QueryRow(t.Context(), target.q(`SELECT count(*) FROM billing.provider_intents WHERE intent_type='invoice_collection'`)).Scan(&operations))
			require.Zero(t, operations, "backup predates operation admission")
			invoiceBefore, err := target.client[remote].GetInvoice(t.Context(), invoice)
			require.NoError(t, err)
			require.Equal(t, tc.amount-tc.manual, invoiceBefore.AmountDue)
			beforeWrites := len(target.nmi.Attempts())
			receipt, err := money.ReadObservedNMIInvoiceReceipt(ctx, rt.CollectionResolver, mid.UUID(), target.psp["nmi"].UUID(), transaction)
			require.NoError(t, err)
			var wg sync.WaitGroup
			errors := make([]error, 4)
			for i := range errors {
				wg.Go(func() { _, errors[i] = rt.MoneyService.RecoverObservedInvoicePayment(ctx, receipt) })
			}
			wg.Wait()
			for _, err := range errors {
				require.NoError(t, err, "same verified provider receipt replays across concurrent recovery calls")
			}
			paid, err := target.client[remote].GetInvoice(t.Context(), invoice)
			require.NoError(t, err)
			require.Equal(t, billing.InvoicePaid, paid.Status)
			require.Zero(t, paid.AmountDue)
			require.Equal(t, tc.amount, paid.AmountPaid)
			payments, err := target.client[remote].ListInvoicePayments(t.Context(), invoice, billing.PageRequest{})
			require.NoError(t, err)
			wantPayments := 1
			if tc.manual > 0 {
				wantPayments++
			}
			require.Len(t, payments.Items, wantPayments)
			var providerPayments int
			for _, payment := range payments.Items {
				if payment.TransactionID != nil && *payment.TransactionID == transaction {
					providerPayments++
					require.NotNil(t, payment.Rail)
					require.Equal(t, "nmi", *payment.Rail)
					require.Nil(t, payment.PaymentMethodID, "lost method/initiator is not invented")
				}
			}
			require.Equal(t, 1, providerPayments)
			var allocations int
			var settled, credited int64
			require.NoError(t, target.pool.QueryRow(t.Context(), target.q(`SELECT count(*),COALESCE(sum(amount),0) FROM billing.ledger_transfers WHERE operation='invoice_payment' AND invoice_id=$1`), invoice.UUID()).Scan(&allocations, &settled))
			require.Equal(t, 1, allocations)
			require.Equal(t, tc.amount-tc.manual, settled)
			require.NoError(t, target.pool.QueryRow(t.Context(), target.q(`SELECT COALESCE(sum(l.amount),0) FROM billing.ledger_transfers l JOIN billing.grants g ON g.id=l.grant_id AND g.merchant_id=l.merchant_id WHERE g.source_id LIKE 'invoice-observed-rounding:%' AND l.transfer_type='deposit' AND l.customer_id=$1`), c.cid().UUID()).Scan(&credited))
			if tc.amount%10_000 != 0 {
				require.Equal(t, int64(9_999), credited)
			} else {
				require.Zero(t, credited)
			}
			require.NoError(t, target.pool.QueryRow(t.Context(), target.q(`SELECT count(*) FROM billing.provider_intents WHERE intent_type='invoice_collection'`)).Scan(&operations))
			require.Zero(t, operations)
			require.Len(t, target.nmi.Attempts(), beforeWrites, "observed recovery made no outward charge")
			require.Equal(t, config.ProviderWriteModeReadOnly, rt.Config.ProviderWriteMode, "read-only mode is unchanged")
			// Restart and reread: the durable PSP+transaction receipt, rather than an
			// in-memory proof, prevents another allocation or rounding credit.
			target.restart()
			rt = engine.Graph(target.rt).Runtime
			again, err := money.ReadObservedNMIInvoiceReceipt(ctx, rt.CollectionResolver, mid.UUID(), target.psp["nmi"].UUID(), transaction)
			require.NoError(t, err)
			_, err = rt.MoneyService.RecoverObservedInvoicePayment(ctx, again)
			require.NoError(t, err)
			require.Len(t, target.nmi.Attempts(), beforeWrites)
			var afterCredit int64
			require.NoError(t, target.pool.QueryRow(t.Context(), target.q(`SELECT COALESCE(sum(l.amount),0) FROM billing.ledger_transfers l JOIN billing.grants g ON g.id=l.grant_id AND g.merchant_id=l.merchant_id WHERE g.source_id LIKE 'invoice-observed-rounding:%' AND l.transfer_type='deposit' AND l.customer_id=$1`), c.cid().UUID()).Scan(&afterCredit))
			require.Equal(t, credited, afterCredit, "restart does not create a second rounding credit")
			require.NoError(t, target.pool.QueryRow(t.Context(), target.q(`SELECT count(*) FROM billing.ledger_transfers WHERE operation='invoice_payment' AND invoice_id=$1`), invoice.UUID()).Scan(&allocations))
			require.Equal(t, 1, allocations, "restart does not allocate the receipt twice")
		})
	}
}
