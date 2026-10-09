package money

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/money/ledger"
	"github.com/open-rails/openrails/internal/modules/money/statement"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

var ErrInvoiceRecoveryHeld = errors.New("observed invoice payment requires reconciliation")
var ErrNotInvoiceReceipt = nmi.ErrNotInvoiceReceipt

// InvoiceRecoveryOperationOwned routes an existing operation to its own
// verifier, including terminal contradictions that require operator repair.
type InvoiceRecoveryOperationOwned struct{ OperationID uuid.UUID }

func (e *InvoiceRecoveryOperationOwned) Error() string {
	return "invoice receipt belongs to accepted operation " + e.OperationID.String()
}

// ObservedNMIInvoiceReceipt is positive provider evidence, not reconstructed
// authorization. Its fields are private and only an account-bound read creates it.
type ObservedNMIInvoiceReceipt struct {
	merchant, psp uuid.UUID
	facts         nmi.InvoiceSaleEvidence
}

func ReadObservedNMIInvoiceReceipt(ctx context.Context, resolver railresolve.NMIClientResolver, mid, psp uuid.UUID, transactionID string) (ObservedNMIInvoiceReceipt, error) {
	client, err := invoiceRecoveryClient(ctx, resolver, mid, psp)
	if err != nil {
		return ObservedNMIInvoiceReceipt{}, err
	}
	facts, err := client.ReadInvoiceSaleEvidence(ctx, transactionID)
	if err != nil {
		return ObservedNMIInvoiceReceipt{}, err
	}
	return ObservedNMIInvoiceReceipt{merchant: mid, psp: psp, facts: facts}, nil
}

func invoiceRecoveryClient(ctx context.Context, resolver railresolve.NMIClientResolver, mid, psp uuid.UUID) (*nmi.NMIClient, error) {
	if resolver == nil || mid == uuid.Nil || psp == uuid.Nil {
		return nil, fmt.Errorf("%w: account reader unavailable", ErrInvoiceRecoveryHeld)
	}
	client, ok, err := resolver.ResolveNMIClient(ctx, mid, &psp)
	if err != nil {
		return nil, err
	}
	if !ok || client == nil {
		return nil, fmt.Errorf("%w: account cannot be read", ErrInvoiceRecoveryHeld)
	}
	owner, account := client.AccountIdentity()
	if owner != mid || account != psp {
		return nil, fmt.Errorf("%w: reader belongs to another account", ErrInvoiceRecoveryHeld)
	}
	return client, nil
}

// SetInvoiceRecoveryResolver wires account-bound readback before serving work.
func (s *MoneyService) SetInvoiceRecoveryResolver(resolver NMIClientResolver) {
	s.invoiceRecoveryResolver = resolver
}

// findObservedInvoicePayment checks the current history of every retained NMI
// account in this environment before another account can charge, including
// accounts whose card was added after the retained backup.
// A bulk refresh deliberately ending before now cannot replace this read.
func (s *MoneyService) findObservedInvoicePayment(ctx context.Context, q *gen.Queries, mid, invoice, routedPSP uuid.UUID) (ObservedNMIInvoiceReceipt, bool, error) {
	accounts, err := q.InvoiceRecoveryAccounts(ctx, gen.InvoiceRecoveryAccountsParams{MerchantID: mid, RoutedPsp: routedPSP})
	if err != nil {
		return ObservedNMIInvoiceReceipt{}, false, err
	}
	var receipt ObservedNMIInvoiceReceipt
	found := false
	for _, account := range accounts {
		if account.Rail != "nmi" {
			continue
		}
		client, err := invoiceRecoveryClient(ctx, s.invoiceRecoveryResolver, mid, account.ID)
		if err != nil {
			return ObservedNMIInvoiceReceipt{}, false, err
		}
		facts, exists, err := client.FindInvoiceSaleEvidence(ctx, invoice)
		if err != nil {
			return ObservedNMIInvoiceReceipt{}, false, fmt.Errorf("%w: account %s invoice history: %v", ErrInvoiceRecoveryHeld, account.ID, err)
		}
		if !exists {
			continue
		}
		if found {
			return ObservedNMIInvoiceReceipt{}, false, fmt.Errorf("%w: invoice paid on multiple accounts", ErrInvoiceRecoveryHeld)
		}
		receipt, found = ObservedNMIInvoiceReceipt{merchant: mid, psp: account.ID, facts: facts}, true
	}
	return receipt, found, nil
}

// recoverBeforeDispatch handles an accepted operation restored without a local
// submission fence. Its own provider order remains canonical. Another order is
// never adopted as its charge: only atomic, still-unsent completion can release
// this operation before the observed invoice receipt is recorded.
func (h *InvoiceCollectionHandler) recoverBeforeDispatch(ctx context.Context, intent gen.BillingProviderIntent, p intents.InvoiceCollectionPayload) (intents.Outcome, bool) {
	if p.Rail != string(models.RailNMI) {
		return intents.Outcome{}, false
	}
	resolver, ok := h.Verifier.(NMIClientResolver)
	if !ok {
		return intents.Parked("invoice history reader unavailable"), true
	}
	service := NewMoneyService(h.DB, h.Clock)
	service.SetInvoiceRecoveryResolver(resolver)
	receipt, found, err := service.findObservedInvoicePayment(ctx, h.DB.Gen(ctx), intent.MerchantID, p.InvoiceID, *intent.PspID)
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
		outcome = local.finalizeNotExecuted(ctx, intent, p, "invoice_paid_elsewhere", "invoice already has a verified payment under another operation")
		if outcome.Class != intents.OutcomeTerminal {
			return errors.New(outcome.Reason)
		}
		// CompleteInvoiceCollection locks and rereads the intent. If another
		// executor fenced submission, its nonexecution check fails and all
		// attempt/invoice changes roll back. Terminal rows cannot gain a fence.
		_, err := NewMoneyService(local.DB, h.Clock).RecoverObservedInvoicePayment(ctx, receipt)
		return err
	})
	if err != nil {
		return intents.Ambiguous("invoice receipt could not safely replace unsent collection: " + err.Error()), true
	}
	return outcome, true
}

// RecoverObservedInvoicePayment settles an existing finalized invoice from a
// newly verified provider receipt. It never creates an accepted charge, infers
// its initiator, establishes a stored-card agreement, or calls the provider.
// A smaller payment does not authorize collection of a possibly lost remainder.
// Canonical automatic invoice writers share the payer/invoice locks below.
// Restore and trusted manual imports must be coordinated with active writers;
// the cross-table receipt checks do not establish global database uniqueness.
func (s *MoneyService) RecoverObservedInvoicePayment(ctx context.Context, receipt ObservedNMIInvoiceReceipt) (*models.Invoice, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	facts := receipt.facts
	if s == nil || s.db == nil || receipt.merchant != mid.UUID() || receipt.psp == uuid.Nil || facts.InvoiceID == uuid.Nil || facts.Sale.TransactionID == "" || !facts.Sale.Approved || facts.PaidAt.IsZero() {
		return nil, fmt.Errorf("%w: invalid verified receipt", ErrInvoiceRecoveryHeld)
	}
	charged, err := moneyutil.RailMinorToNative(facts.Sale.Currency, facts.Sale.Amount)
	if err != nil || charged <= 0 {
		return nil, fmt.Errorf("%w: unreadable provider amount", ErrInvoiceRecoveryHeld)
	}
	order, err := uuid.Parse(facts.Sale.OrderReference)
	if err != nil {
		return nil, fmt.Errorf("%w: missing operation identity", ErrInvoiceRecoveryHeld)
	}
	var result *models.Invoice
	err = s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		account, err := q.GetPSP(ctx, gen.GetPSPParams{MerchantID: mid.UUID(), ID: receipt.psp})
		if err != nil {
			return err
		}
		if account.Rail != "nmi" {
			return fmt.Errorf("%w: account rail changed", ErrInvoiceRecoveryHeld)
		}
		invoice, err := q.GetMerchantInvoice(ctx, gen.GetMerchantInvoiceParams{MerchantID: mid.UUID(), ID: facts.InvoiceID})
		if err != nil {
			return fmt.Errorf("%w: retained invoice unavailable: %v", ErrInvoiceRecoveryHeld, err)
		}
		if _, err = q.LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: mid.UUID(), ID: invoice.CustomerID}); err != nil {
			return err
		}
		invoice, err = q.GetInvoiceForPayerForUpdate(ctx, gen.GetInvoiceForPayerForUpdateParams{MerchantID: mid.UUID(), CustomerID: invoice.CustomerID, ID: invoice.ID})
		if err != nil {
			return err
		}
		existing, err := q.GetProviderIntent(ctx, gen.GetProviderIntentParams{MerchantID: mid.UUID(), ID: order})
		if err == nil {
			accepted, decodeErr := intents.DecodeInvoiceCollectionPayload(existing)
			if decodeErr != nil || accepted.InvoiceID != facts.InvoiceID || existing.PspID == nil || *existing.PspID != receipt.psp {
				return fmt.Errorf("%w: provider order contradicts a retained operation", ErrInvoiceRecoveryHeld)
			}
			return &InvoiceRecoveryOperationOwned{OperationID: existing.ID}
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if invoice.CollectionIntentID != nil {
			return &InvoiceRecoveryOperationOwned{OperationID: *invoice.CollectionIntentID}
		}
		if invoice.Currency != facts.Sale.Currency {
			return fmt.Errorf("%w: invoice currency contradicts provider", ErrInvoiceRecoveryHeld)
		}
		customers, err := q.InvoiceRecoveryVaultCustomers(ctx, gen.InvoiceRecoveryVaultCustomersParams{MerchantID: mid.UUID(), PspID: receipt.psp, Vault: facts.Sale.CustomerVaultID})
		if err != nil {
			return err
		}
		if len(customers) != 1 || customers[0] != invoice.CustomerID {
			return fmt.Errorf("%w: vault does not identify the invoice payer", ErrInvoiceRecoveryHeld)
		}
		other, err := q.InvoiceRecoveryHasOtherPayment(ctx, gen.InvoiceRecoveryHasOtherPaymentParams{MerchantID: mid.UUID(), PspID: receipt.psp, TransactionID: facts.Sale.TransactionID})
		if err != nil {
			return err
		}
		if other {
			return fmt.Errorf("%w: transaction already belongs to another payment", ErrInvoiceRecoveryHeld)
		}
		// A statement also states charges of its period other invoices bill,
		// and what they received; its receivable is the charges it bills.
		billed, err := q.SumInvoiceItemAmountBilledBy(ctx, gen.SumInvoiceItemAmountBilledByParams{MerchantID: mid.UUID(), InvoiceID: invoice.ID})
		if err != nil {
			return err
		}
		ownPaid := billed - invoice.AmountDue
		if received := invoice.AmountPaid - ownPaid; invoice.AmountPaid < 0 || invoice.AmountDue < 0 || ownPaid < 0 || received < 0 || received > invoice.TotalAmount-billed {
			return fmt.Errorf("%w: invoice monetary snapshot is inconsistent", ErrInvoiceRecoveryHeld)
		}
		allocations, err := q.InvoiceRecoveryAllocationTotal(ctx, gen.InvoiceRecoveryAllocationTotalParams{MerchantID: mid.UUID(), InvoiceID: invoice.ID, Currency: invoice.Currency})
		if err != nil {
			return err
		}
		if !allocations.Consistent || allocations.Amount != ownPaid {
			return fmt.Errorf("%w: retained invoice allocations are incomplete", ErrInvoiceRecoveryHeld)
		}
		prior, err := q.GetInvoiceRecoveryPayment(ctx, gen.GetInvoiceRecoveryPaymentParams{MerchantID: mid.UUID(), PspID: receipt.psp, TransactionID: facts.Sale.TransactionID})
		if err != nil {
			return err
		}
		if len(prior) > 0 {
			if (invoice.Status != "paid" && invoice.Status != "open") || invoice.AmountDue != 0 || len(prior) != 1 || prior[0].InvoiceID != invoice.ID || prior[0].CustomerID != invoice.CustomerID || prior[0].Amount != charged || prior[0].Currency != invoice.Currency || prior[0].Status != "settled" || prior[0].Channel != string(models.ChannelRail) || prior[0].Rail == nil || *prior[0].Rail != "nmi" || prior[0].LedgerTransferID == nil {
				return fmt.Errorf("%w: transaction allocation conflicts", ErrInvoiceRecoveryHeld)
			}
			transfer, err := q.GetInvoiceRecoveryLedgerTransfer(ctx, gen.GetInvoiceRecoveryLedgerTransferParams{MerchantID: mid.UUID(), TransferID: *prior[0].LedgerTransferID})
			if err != nil {
				return err
			}
			minor, convertErr := moneyutil.NativeToRailMinor(invoice.Currency, transfer.Amount)
			if transfer.InvoiceID == nil || *transfer.InvoiceID != invoice.ID || transfer.CustomerID == nil || *transfer.CustomerID != invoice.CustomerID || transfer.Currency != invoice.Currency || transfer.TransferType != "owed_payment" || transfer.Operation != string(ledger.OpInvoicePayment) || transfer.Amount <= 0 || convertErr != nil || minor != facts.Sale.Amount {
				return fmt.Errorf("%w: invoice receipt ledger binding conflicts", ErrInvoiceRecoveryHeld)
			}
			result, err = invoiceFromGen(invoice)
			return err
		}
		if (invoice.Status != "open" && invoice.Status != "past_due" && invoice.Status != "uncollectible") || invoice.FinalizedAt == nil || invoice.AmountDue <= 0 {
			return fmt.Errorf("%w: invoice is not an unchanged finalized receivable", ErrInvoiceRecoveryHeld)
		}
		if facts.PaidAt.Before(invoice.FinalizedAt.Add(-intents.ClockMargin)) || facts.PaidAt.After(s.now().Add(intents.ClockMargin)) {
			return fmt.Errorf("%w: provider payment time contradicts the retained invoice", ErrInvoiceRecoveryHeld)
		}
		expected, err := moneyutil.NativeToRailMinor(invoice.Currency, invoice.AmountDue)
		if err != nil || expected != facts.Sale.Amount {
			return fmt.Errorf("%w: provider amount does not cover the exact remaining invoice balance", ErrInvoiceRecoveryHeld)
		}
		if err = q.ReopenObservedInvoiceForSettlement(ctx, gen.ReopenObservedInvoiceForSettlementParams{MerchantID: mid.UUID(), InvoiceID: invoice.ID}); err != nil {
			return err
		}
		applied, err := q.ApplyInvoicePaymentSnapshot(ctx, gen.ApplyInvoicePaymentSnapshotParams{MerchantID: mid.UUID(), CustomerID: invoice.CustomerID, InvoiceID: invoice.ID, Snapshot: invoice.AmountDue, Now: facts.PaidAt})
		if err != nil {
			return err
		}
		if applied != 1 {
			return fmt.Errorf("%w: invoice changed under recovery lock", ErrInvoiceRecoveryHeld)
		}
		if err := statement.Follow(ctx, q, mid.UUID(), invoice.CustomerID, invoice.Currency, invoice.ID, facts.PaidAt); err != nil {
			return err
		}
		key := receipt.psp.String() + ":" + facts.Sale.TransactionID
		transfer, err := ledger.New(q, mid.UUID()).PayOwed(ctx, invoice.CustomerID, invoice.Currency, invoice.AmountDue, ledger.Coord{Operation: ledger.OpInvoicePayment, Source: "observed_invoice_payment", SourceID: key}, &invoice.ID)
		if err != nil {
			return err
		}
		if transfer.InvoiceID == nil || *transfer.InvoiceID != invoice.ID || transfer.Amount != invoice.AmountDue {
			return fmt.Errorf("%w: recovered ledger identity conflicts", ErrInvoiceRecoveryHeld)
		}
		if surplus := charged - invoice.AmountDue; surplus > 0 {
			payer := identity.CustomerID(invoice.CustomerID)
			roundingKey := "invoice-observed-rounding:" + key
			description := "Rounding credit from recovered invoice " + invoice.ID.String()
			if _, err := s.depositTx(ctx, q, DepositParams{CustomerID: &payer, Currency: invoice.Currency, Amount: surplus, Source: "invoice_rounding_purchase", SourceID: &roundingKey, Description: &description}); err != nil {
				return err
			}
		}
		if err = q.InsertInvoicePayment(ctx, gen.InsertInvoicePaymentParams{ID: uuidutil.NewV7(), MerchantID: mid.UUID(), CustomerID: invoice.CustomerID, InvoiceID: invoice.ID, LedgerTransferID: &transfer.ID, Currency: invoice.Currency, Amount: charged, Status: "settled", Channel: string(models.ChannelRail), Rail: new("nmi"), RailPaymentID: &facts.Sale.TransactionID, PspID: &receipt.psp, AttemptedAt: facts.PaidAt, SettledAt: &facts.PaidAt, CreatedAt: s.now(), UpdatedAt: s.now()}); err != nil {
			return err
		}
		settled, err := q.GetInvoiceForPayer(ctx, gen.GetInvoiceForPayerParams{MerchantID: mid.UUID(), CustomerID: invoice.CustomerID, ID: invoice.ID})
		if err != nil {
			return err
		}
		result, err = invoiceFromGen(settled)
		return err
	})
	return result, err
}
