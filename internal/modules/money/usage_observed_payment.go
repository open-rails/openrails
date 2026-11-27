package money

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/invoices/usage"
	"github.com/open-rails/openrails/internal/modules/money/books"
	"github.com/open-rails/openrails/internal/modules/money/credits"
	"github.com/open-rails/openrails/internal/modules/money/ledger"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

var ErrUsageRecoveryHeld = errors.New("observed usage invoice payment requires reconciliation")

// UsageRecoveryOperationOwned routes an existing operation to its own
// verifier, including terminal contradictions that require operator repair.
type UsageRecoveryOperationOwned struct{ OperationID uuid.UUID }

func (e *UsageRecoveryOperationOwned) Error() string {
	return "the usage invoice's receipt belongs to accepted operation " + e.OperationID.String()
}

// ObservedNMIUsageReceipt is positive provider evidence, not reconstructed
// authorization. Its fields are private and only an account-bound read creates it.
type ObservedNMIUsageReceipt struct {
	merchant, psp uuid.UUID
	facts         nmi.InvoiceSaleEvidence
}

func ReadObservedNMIUsageReceipt(ctx context.Context, resolver railresolve.NMIClientResolver, mid, psp uuid.UUID, transactionID string) (ObservedNMIUsageReceipt, error) {
	client, err := usageRecoveryClient(ctx, resolver, mid, psp)
	if err != nil {
		return ObservedNMIUsageReceipt{}, err
	}
	facts, err := client.ReadInvoiceSaleEvidence(ctx, transactionID)
	if err != nil {
		return ObservedNMIUsageReceipt{}, err
	}
	return ObservedNMIUsageReceipt{merchant: mid, psp: psp, facts: facts}, nil
}

func usageRecoveryClient(ctx context.Context, resolver railresolve.NMIClientResolver, mid, psp uuid.UUID) (*nmi.NMIClient, error) {
	if resolver == nil || mid == uuid.Nil || psp == uuid.Nil {
		return nil, fmt.Errorf("%w: account reader unavailable", ErrUsageRecoveryHeld)
	}
	client, ok, err := resolver.ResolveNMIClient(ctx, mid, &psp)
	if err != nil {
		return nil, err
	}
	if !ok || client == nil {
		return nil, fmt.Errorf("%w: account cannot be read", ErrUsageRecoveryHeld)
	}
	owner, account := client.AccountIdentity()
	if owner != mid || account != psp {
		return nil, fmt.Errorf("%w: reader belongs to another account", ErrUsageRecoveryHeld)
	}
	return client, nil
}

// SetUsageRecoveryResolver wires account-bound readback before serving work.
func (s *MoneyService) SetUsageRecoveryResolver(resolver railresolve.NMIClientResolver) {
	s.usageRecoveryResolver = resolver
}

// findObservedUsagePayment checks the current history of every retained NMI
// account in this environment before another account can charge, including
// accounts whose card was added after the retained backup.
// A bulk refresh deliberately ending before now cannot replace this read.
func (s *MoneyService) findObservedUsagePayment(ctx context.Context, q *gen.Queries, mid, invoice, routedPSP uuid.UUID) (ObservedNMIUsageReceipt, bool, error) {
	accounts, err := q.UsageRecoveryAccounts(ctx, gen.UsageRecoveryAccountsParams{MerchantID: mid, RoutedPsp: routedPSP})
	if err != nil {
		return ObservedNMIUsageReceipt{}, false, err
	}
	var receipt ObservedNMIUsageReceipt
	found := false
	for _, account := range accounts {
		if account.Rail != "nmi" {
			continue
		}
		client, err := usageRecoveryClient(ctx, s.usageRecoveryResolver, mid, account.ID)
		if err != nil {
			return ObservedNMIUsageReceipt{}, false, err
		}
		facts, exists, err := client.FindInvoiceSaleEvidence(ctx, invoice)
		if err != nil {
			return ObservedNMIUsageReceipt{}, false, fmt.Errorf("%w: account %s invoice history: %v", ErrUsageRecoveryHeld, account.ID, err)
		}
		if !exists {
			continue
		}
		if found {
			return ObservedNMIUsageReceipt{}, false, fmt.Errorf("%w: invoice paid on multiple accounts", ErrUsageRecoveryHeld)
		}
		receipt, found = ObservedNMIUsageReceipt{merchant: mid, psp: account.ID, facts: facts}, true
	}
	return receipt, found, nil
}

// recoverBeforeDispatch handles an accepted operation restored without a local
// submission fence. Its own provider order remains canonical. Another invoice is
// never adopted as its charge: only atomic, still-unsent completion can release
// this operation before the observed receipt is recorded.
func (h *InvoiceCollectionHandler) recoverBeforeDispatch(ctx context.Context, intent gen.BillingProviderIntent, p intents.InvoiceCollectionPayload) (intents.Outcome, bool) {
	if p.Rail != string(models.RailNMI) {
		return intents.Outcome{}, false
	}
	if h.Verifier == nil {
		return intents.Parked("invoice history reader unavailable"), true
	}
	service := NewMoneyService(h.DB, h.Clock)
	service.SetUsageRecoveryResolver(h.Verifier)
	receipt, found, err := service.findObservedUsagePayment(ctx, h.DB.Gen(ctx), intent.MerchantID, p.InvoiceID, *intent.PspID)
	if err != nil {
		return intents.Parked("read current invoice history: " + err.Error()), true
	}
	if !found {
		return intents.Outcome{}, false
	}
	if receipt.psp == *intent.PspID && receipt.facts.Sale.OrderReference == intent.ID.String() {
		return h.qualifyAndSettle(ctx, intent, receipt.facts.Sale.TransactionID), true
	}
	var outcome intents.Outcome
	err = h.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		local := *h
		local.DB = h.DB.NewWithPgxTx(tx)
		outcome = local.finalizeNotExecuted(ctx, intent, p, "invoice_paid_elsewhere", "the invoice already has a verified payment under another operation")
		if outcome.Class != intents.OutcomeTerminal {
			return errors.New(outcome.Reason)
		}
		// CompleteInvoiceCollection locks and rereads the intent. If another
		// executor fenced submission, its nonexecution check fails and all
		// attempt/invoice changes roll back. Terminal rows cannot gain a fence.
		_, err := NewMoneyService(local.DB, h.Clock).RecoverObservedUsagePayment(ctx, receipt)
		return err
	})
	if err != nil {
		return intents.Ambiguous("the invoice's receipt could not safely replace the unsent collection: " + err.Error()), true
	}
	return outcome, true
}

// RecoverObservedUsagePayment settles an unpaid usage invoice from a newly
// verified provider receipt of its full remaining amount. It never creates an
// accepted charge, infers its initiator, establishes a stored-card agreement,
// or calls the provider. A smaller payment does not authorize collection of
// a possibly lost remainder. Canonical writers share the customer/invoice locks
// below. Restore and trusted manual imports must be coordinated with active
// writers; the cross-table receipt checks do not establish global database
// uniqueness.
func (s *MoneyService) RecoverObservedUsagePayment(ctx context.Context, receipt ObservedNMIUsageReceipt) (*gen.BillingInvoice, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	facts := receipt.facts
	if s == nil || s.db == nil || receipt.merchant != mid.UUID() || receipt.psp == uuid.Nil || facts.InvoiceID == uuid.Nil || facts.Sale.TransactionID == "" || !facts.Sale.Approved || facts.PaidAt.IsZero() {
		return nil, fmt.Errorf("%w: invalid verified receipt", ErrUsageRecoveryHeld)
	}
	charged, err := moneyutil.RailMinorToNative(facts.Sale.Currency, facts.Sale.Amount)
	if err != nil || charged <= 0 {
		return nil, fmt.Errorf("%w: unreadable provider amount", ErrUsageRecoveryHeld)
	}
	operation, err := uuid.Parse(facts.Sale.OrderReference)
	if err != nil {
		return nil, fmt.Errorf("%w: missing operation identity", ErrUsageRecoveryHeld)
	}
	// The sale's description names the invoice it paid.
	invoiceID := facts.InvoiceID
	var result *gen.BillingInvoice
	err = s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		account, err := q.GetPSP(ctx, gen.GetPSPParams{MerchantID: mid.UUID(), ID: receipt.psp})
		if err != nil {
			return err
		}
		if account.Rail != "nmi" {
			return fmt.Errorf("%w: account rail changed", ErrUsageRecoveryHeld)
		}
		invoice, err := lockUsageInvoice(ctx, q, mid.UUID(), invoiceID)
		if err != nil {
			return fmt.Errorf("%w: retained invoice unavailable: %v", ErrUsageRecoveryHeld, err)
		}
		existing, err := q.GetProviderIntent(ctx, gen.GetProviderIntentParams{MerchantID: mid.UUID(), ID: operation})
		if err == nil {
			accepted, decodeErr := intents.DecodeInvoiceCollectionPayload(existing)
			if decodeErr != nil || accepted.InvoiceID != invoiceID || existing.PspID == nil || *existing.PspID != receipt.psp {
				return fmt.Errorf("%w: provider order contradicts a retained operation", ErrUsageRecoveryHeld)
			}
			return &UsageRecoveryOperationOwned{OperationID: existing.ID}
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if invoice.Status == string(billing.InvoiceProcessing) && invoice.AttemptID != nil {
			return &UsageRecoveryOperationOwned{OperationID: *invoice.AttemptID}
		}
		if invoice.Currency != facts.Sale.Currency {
			return fmt.Errorf("%w: invoice currency contradicts provider", ErrUsageRecoveryHeld)
		}
		customers, err := q.UsageRecoveryVaultCustomers(ctx, gen.UsageRecoveryVaultCustomersParams{MerchantID: mid.UUID(), PspID: receipt.psp, Vault: facts.Sale.CustomerVaultID})
		if err != nil {
			return err
		}
		if len(customers) != 1 || customers[0] != invoice.CustomerID {
			return fmt.Errorf("%w: vault does not identify the invoice's customer", ErrUsageRecoveryHeld)
		}
		other, err := q.UsageRecoveryHasOtherPayment(ctx, gen.UsageRecoveryHasOtherPaymentParams{MerchantID: mid.UUID(), PspID: receipt.psp, TransactionID: facts.Sale.TransactionID})
		if err != nil {
			return err
		}
		if other {
			return fmt.Errorf("%w: transaction already belongs to another payment", ErrUsageRecoveryHeld)
		}
		allocations, err := q.UsageRecoveryAllocationTotal(ctx, gen.UsageRecoveryAllocationTotalParams{MerchantID: mid.UUID(), InvoiceID: invoiceID, Currency: invoice.Currency})
		if err != nil {
			return err
		}
		if !allocations.Consistent || allocations.Amount > derefInt64(invoice.AmountPaid) {
			return fmt.Errorf("%w: retained invoice allocations are inconsistent", ErrUsageRecoveryHeld)
		}
		prior, err := q.GetUsageRecoveryPayment(ctx, gen.GetUsageRecoveryPaymentParams{MerchantID: mid.UUID(), PspID: receipt.psp, TransactionID: facts.Sale.TransactionID})
		if err != nil {
			return err
		}
		if len(prior) > 0 {
			if invoice.Status != string(billing.InvoiceComplete) || len(prior) != 1 || prior[0].InvoiceID == nil || *prior[0].InvoiceID != invoiceID || prior[0].CustomerID != invoice.CustomerID || prior[0].Amount != charged || prior[0].Currency != invoice.Currency || prior[0].Status != "succeeded" || prior[0].Channel != string(models.ChannelRail) || prior[0].Rail == nil || *prior[0].Rail != "nmi" || prior[0].LedgerTransferID == nil {
				return fmt.Errorf("%w: transaction allocation conflicts", ErrUsageRecoveryHeld)
			}
			transfer, err := q.GetUsageRecoveryLedgerTransfer(ctx, gen.GetUsageRecoveryLedgerTransferParams{MerchantID: mid.UUID(), TransferID: *prior[0].LedgerTransferID})
			if err != nil {
				return err
			}
			minor, convertErr := moneyutil.NativeToRailMinor(invoice.Currency, transfer.Amount)
			if transfer.InvoiceID == nil || *transfer.InvoiceID != invoiceID || transfer.CustomerID == nil || *transfer.CustomerID != invoice.CustomerID || transfer.Currency != invoice.Currency || transfer.TransferType != "owed_payment" || transfer.Operation != string(ledger.OpInvoicePayment) || transfer.Amount <= 0 || convertErr != nil || minor != facts.Sale.Amount {
				return fmt.Errorf("%w: the invoice's receipt ledger binding conflicts", ErrUsageRecoveryHeld)
			}
			result = &invoice
			return nil
		}
		due := usage.Due(invoice)
		if (invoice.Status != string(billing.InvoiceOpen) && invoice.Status != string(billing.InvoiceUncollectible)) || due <= 0 {
			return fmt.Errorf("%w: the invoice is not an unchanged unpaid usage invoice", ErrUsageRecoveryHeld)
		}
		if facts.PaidAt.Before(invoice.CreatedAt.Add(-intents.ClockMargin)) || facts.PaidAt.After(s.now().Add(intents.ClockMargin)) {
			return fmt.Errorf("%w: provider payment time contradicts the retained invoice", ErrUsageRecoveryHeld)
		}
		expected, err := moneyutil.NativeToRailMinor(invoice.Currency, due)
		if err != nil || expected != facts.Sale.Amount {
			return fmt.Errorf("%w: provider amount does not cover the exact remaining invoice balance", ErrUsageRecoveryHeld)
		}
		key := receipt.psp.String() + ":" + facts.Sale.TransactionID
		// The money arrives before it settles what is owed.
		paymentID := uuidutil.NewV7()
		if err := books.New(q, mid.UUID()).Receive(ctx, books.Receipt{Payment: paymentID, Customer: invoice.CustomerID, Invoice: &invoiceID,
			PSP: &receipt.psp, Amount: charged, Currency: invoice.Currency, At: facts.PaidAt}); err != nil {
			return err
		}
		if err := books.Started(ctx, q, mid.UUID(), invoice.Currency, s.now()); err != nil {
			return err
		}
		transfer, err := ledger.New(q, mid.UUID()).PayOwed(ctx, invoice.CustomerID, invoice.Currency, due, ledger.Coord{Operation: ledger.OpInvoicePayment, Source: "observed_invoice_payment", SourceID: key}, &invoiceID)
		if err != nil {
			return err
		}
		if transfer.InvoiceID == nil || *transfer.InvoiceID != invoiceID || transfer.Amount != due {
			return fmt.Errorf("%w: recovered ledger identity conflicts", ErrUsageRecoveryHeld)
		}
		if err := books.New(q, mid.UUID()).CollectUsage(ctx, books.UsageCollection{Payment: paymentID, Customer: invoice.CustomerID, Invoice: &invoiceID,
			Currency: invoice.Currency, Amount: due, At: facts.PaidAt}); err != nil {
			return err
		}
		var change *models.MoneyTransaction
		if surplus := charged - due; surplus > 0 {
			customer := identity.CustomerID(invoice.CustomerID)
			roundingKey := "invoice-observed-rounding:" + key
			description := "Rounding credit from recovered invoice " + billing.InvoiceID(invoiceID).String()
			if change, err = s.depositTx(ctx, s.db.NewWithPgxTx(tx), DepositParams{CustomerID: &customer, Invoker: customer.String(), Currency: invoice.Currency, Amount: surplus,
				Source: "invoice_rounding_purchase", SourceID: &roundingKey, Description: &description, Funding: credits.Purchase}); err != nil {
				return err
			}
		}
		if err = q.InsertInvoicePayment(ctx, gen.InsertInvoicePaymentParams{ID: paymentID, MerchantID: mid.UUID(), CustomerID: invoice.CustomerID, InvoiceID: invoiceID, LedgerTransferID: transfer.ID, Currency: invoice.Currency, Amount: charged, Channel: string(models.ChannelRail), Rail: new("nmi"), TransactionID: facts.Sale.TransactionID, PspID: &receipt.psp, PaidAt: facts.PaidAt, Now: s.now(), TokenType: new(charge.TokenTypePSPToken)}); err != nil {
			return err
		}
		if err := s.fundChange(ctx, q, mid.UUID(), change, paymentID); err != nil {
			return err
		}
		settled, err := usage.Pay(ctx, s.db.NewWithPgxTx(tx), mid.UUID(), invoiceID, due, &paymentID, nil, facts.PaidAt)
		if err != nil {
			return fmt.Errorf("%w: invoice changed under recovery lock: %v", ErrUsageRecoveryHeld, err)
		}
		result = &settled
		return nil
	})
	return result, err
}

func derefInt64(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}
