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
	"github.com/open-rails/openrails/internal/modules/money/ledger"
	"github.com/open-rails/openrails/internal/modules/orders/usage"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

var ErrUsageRecoveryHeld = errors.New("observed usage order payment requires reconciliation")

// UsageRecoveryOperationOwned routes an existing operation to its own
// verifier, including terminal contradictions that require operator repair.
type UsageRecoveryOperationOwned struct{ OperationID uuid.UUID }

func (e *UsageRecoveryOperationOwned) Error() string {
	return "the usage order's receipt belongs to accepted operation " + e.OperationID.String()
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
func (s *MoneyService) SetUsageRecoveryResolver(resolver NMIClientResolver) {
	s.usageRecoveryResolver = resolver
}

// findObservedUsagePayment checks the current history of every retained NMI
// account in this environment before another account can charge, including
// accounts whose card was added after the retained backup.
// A bulk refresh deliberately ending before now cannot replace this read.
func (s *MoneyService) findObservedUsagePayment(ctx context.Context, q *gen.Queries, mid, order, routedPSP uuid.UUID) (ObservedNMIUsageReceipt, bool, error) {
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
		facts, exists, err := client.FindInvoiceSaleEvidence(ctx, order)
		if err != nil {
			return ObservedNMIUsageReceipt{}, false, fmt.Errorf("%w: account %s order history: %v", ErrUsageRecoveryHeld, account.ID, err)
		}
		if !exists {
			continue
		}
		if found {
			return ObservedNMIUsageReceipt{}, false, fmt.Errorf("%w: order paid on multiple accounts", ErrUsageRecoveryHeld)
		}
		receipt, found = ObservedNMIUsageReceipt{merchant: mid, psp: account.ID, facts: facts}, true
	}
	return receipt, found, nil
}

// recoverBeforeDispatch handles an accepted operation restored without a local
// submission fence. Its own provider order remains canonical. Another order is
// never adopted as its charge: only atomic, still-unsent completion can release
// this operation before the observed receipt is recorded.
func (h *OrderCollectionHandler) recoverBeforeDispatch(ctx context.Context, intent gen.BillingProviderIntent, p intents.OrderCollectionPayload) (intents.Outcome, bool) {
	if p.Rail != string(models.RailNMI) {
		return intents.Outcome{}, false
	}
	resolver, ok := h.Verifier.(NMIClientResolver)
	if !ok {
		return intents.Parked("order history reader unavailable"), true
	}
	service := NewMoneyService(h.DB, h.Clock)
	service.SetUsageRecoveryResolver(resolver)
	receipt, found, err := service.findObservedUsagePayment(ctx, h.DB.Gen(ctx), intent.MerchantID, p.OrderID, *intent.PspID)
	if err != nil {
		return intents.Parked("read current order history: " + err.Error()), true
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
		outcome = local.finalizeNotExecuted(ctx, intent, p, "order_paid_elsewhere", "the order already has a verified payment under another operation")
		if outcome.Class != intents.OutcomeTerminal {
			return errors.New(outcome.Reason)
		}
		// CompleteOrderCollection locks and rereads the intent. If another
		// executor fenced submission, its nonexecution check fails and all
		// attempt/order changes roll back. Terminal rows cannot gain a fence.
		_, err := NewMoneyService(local.DB, h.Clock).RecoverObservedUsagePayment(ctx, receipt)
		return err
	})
	if err != nil {
		return intents.Ambiguous("the order's receipt could not safely replace the unsent collection: " + err.Error()), true
	}
	return outcome, true
}

// RecoverObservedUsagePayment settles an unpaid usage order from a newly
// verified provider receipt of its full remaining amount. It never creates an
// accepted charge, infers its initiator, establishes a stored-card agreement,
// or calls the provider. A smaller payment does not authorize collection of
// a possibly lost remainder. Canonical writers share the payer/order locks
// below. Restore and trusted manual imports must be coordinated with active
// writers; the cross-table receipt checks do not establish global database
// uniqueness.
func (s *MoneyService) RecoverObservedUsagePayment(ctx context.Context, receipt ObservedNMIUsageReceipt) (*gen.BillingOrder, error) {
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
	// The sale's description names the order it paid.
	orderID := facts.InvoiceID
	var result *gen.BillingOrder
	err = s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		account, err := q.GetPSP(ctx, gen.GetPSPParams{MerchantID: mid.UUID(), ID: receipt.psp})
		if err != nil {
			return err
		}
		if account.Rail != "nmi" {
			return fmt.Errorf("%w: account rail changed", ErrUsageRecoveryHeld)
		}
		order, err := lockUsageOrder(ctx, q, mid.UUID(), orderID)
		if err != nil {
			return fmt.Errorf("%w: retained order unavailable: %v", ErrUsageRecoveryHeld, err)
		}
		existing, err := q.GetProviderIntent(ctx, gen.GetProviderIntentParams{MerchantID: mid.UUID(), ID: operation})
		if err == nil {
			accepted, decodeErr := intents.DecodeOrderCollectionPayload(existing)
			if decodeErr != nil || accepted.OrderID != orderID || existing.PspID == nil || *existing.PspID != receipt.psp {
				return fmt.Errorf("%w: provider order contradicts a retained operation", ErrUsageRecoveryHeld)
			}
			return &UsageRecoveryOperationOwned{OperationID: existing.ID}
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if order.Status == string(billing.OrderProcessing) && order.AttemptID != nil {
			return &UsageRecoveryOperationOwned{OperationID: *order.AttemptID}
		}
		if order.Currency != facts.Sale.Currency {
			return fmt.Errorf("%w: order currency contradicts provider", ErrUsageRecoveryHeld)
		}
		customers, err := q.UsageRecoveryVaultCustomers(ctx, gen.UsageRecoveryVaultCustomersParams{MerchantID: mid.UUID(), PspID: receipt.psp, Vault: facts.Sale.CustomerVaultID})
		if err != nil {
			return err
		}
		if len(customers) != 1 || customers[0] != order.CustomerID {
			return fmt.Errorf("%w: vault does not identify the order's payer", ErrUsageRecoveryHeld)
		}
		other, err := q.UsageRecoveryHasOtherPayment(ctx, gen.UsageRecoveryHasOtherPaymentParams{MerchantID: mid.UUID(), PspID: receipt.psp, TransactionID: facts.Sale.TransactionID})
		if err != nil {
			return err
		}
		if other {
			return fmt.Errorf("%w: transaction already belongs to another payment", ErrUsageRecoveryHeld)
		}
		allocations, err := q.UsageRecoveryAllocationTotal(ctx, gen.UsageRecoveryAllocationTotalParams{MerchantID: mid.UUID(), OrderID: orderID, Currency: order.Currency})
		if err != nil {
			return err
		}
		if !allocations.Consistent || allocations.Amount > derefInt64(order.AmountPaid) {
			return fmt.Errorf("%w: retained order allocations are inconsistent", ErrUsageRecoveryHeld)
		}
		prior, err := q.GetUsageRecoveryPayment(ctx, gen.GetUsageRecoveryPaymentParams{MerchantID: mid.UUID(), PspID: receipt.psp, TransactionID: facts.Sale.TransactionID})
		if err != nil {
			return err
		}
		if len(prior) > 0 {
			if order.Status != string(billing.OrderComplete) || len(prior) != 1 || prior[0].OrderID == nil || *prior[0].OrderID != orderID || prior[0].CustomerID != order.CustomerID || prior[0].Amount != charged || prior[0].Currency != order.Currency || prior[0].Status != "succeeded" || prior[0].Channel != string(models.ChannelRail) || prior[0].Rail == nil || *prior[0].Rail != "nmi" || prior[0].LedgerTransferID == nil {
				return fmt.Errorf("%w: transaction allocation conflicts", ErrUsageRecoveryHeld)
			}
			transfer, err := q.GetUsageRecoveryLedgerTransfer(ctx, gen.GetUsageRecoveryLedgerTransferParams{MerchantID: mid.UUID(), TransferID: *prior[0].LedgerTransferID})
			if err != nil {
				return err
			}
			minor, convertErr := moneyutil.NativeToRailMinor(order.Currency, transfer.Amount)
			if transfer.OrderID == nil || *transfer.OrderID != orderID || transfer.CustomerID == nil || *transfer.CustomerID != order.CustomerID || transfer.Currency != order.Currency || transfer.TransferType != "owed_payment" || transfer.Operation != string(ledger.OpOrderPayment) || transfer.Amount <= 0 || convertErr != nil || minor != facts.Sale.Amount {
				return fmt.Errorf("%w: the order's receipt ledger binding conflicts", ErrUsageRecoveryHeld)
			}
			result = &order
			return nil
		}
		due := usage.Due(order)
		if (order.Status != string(billing.OrderOpen) && order.Status != string(billing.OrderUncollectible)) || due <= 0 {
			return fmt.Errorf("%w: the order is not an unchanged unpaid usage order", ErrUsageRecoveryHeld)
		}
		if facts.PaidAt.Before(order.CreatedAt.Add(-intents.ClockMargin)) || facts.PaidAt.After(s.now().Add(intents.ClockMargin)) {
			return fmt.Errorf("%w: provider payment time contradicts the retained order", ErrUsageRecoveryHeld)
		}
		expected, err := moneyutil.NativeToRailMinor(order.Currency, due)
		if err != nil || expected != facts.Sale.Amount {
			return fmt.Errorf("%w: provider amount does not cover the exact remaining order balance", ErrUsageRecoveryHeld)
		}
		key := receipt.psp.String() + ":" + facts.Sale.TransactionID
		transfer, err := ledger.New(q, mid.UUID()).PayOwed(ctx, order.CustomerID, order.Currency, due, ledger.Coord{Operation: ledger.OpOrderPayment, Source: "observed_invoice_payment", SourceID: key}, &orderID)
		if err != nil {
			return err
		}
		if transfer.OrderID == nil || *transfer.OrderID != orderID || transfer.Amount != due {
			return fmt.Errorf("%w: recovered ledger identity conflicts", ErrUsageRecoveryHeld)
		}
		if surplus := charged - due; surplus > 0 {
			payer := identity.CustomerID(order.CustomerID)
			roundingKey := "invoice-observed-rounding:" + key
			description := "Rounding credit from recovered order " + billing.OrderID(orderID).String()
			if _, err := s.depositTx(ctx, s.db.NewWithPgxTx(tx), DepositParams{CustomerID: &payer, Currency: order.Currency, Amount: surplus, Source: "invoice_rounding_purchase", SourceID: &roundingKey, Description: &description}); err != nil {
				return err
			}
		}
		paymentID := uuidutil.NewV7()
		if err = q.InsertOrderPayment(ctx, gen.InsertOrderPaymentParams{ID: paymentID, MerchantID: mid.UUID(), CustomerID: order.CustomerID, OrderID: orderID, LedgerTransferID: transfer.ID, Currency: order.Currency, Amount: charged, Channel: string(models.ChannelRail), Rail: new("nmi"), TransactionID: facts.Sale.TransactionID, PspID: &receipt.psp, PaidAt: facts.PaidAt, Now: s.now(), TokenType: new(charge.TokenTypePSPToken)}); err != nil {
			return err
		}
		settled, err := usage.Pay(ctx, s.db.NewWithPgxTx(tx), mid.UUID(), orderID, due, &paymentID, nil, facts.PaidAt)
		if err != nil {
			return fmt.Errorf("%w: order changed under recovery lock: %v", ErrUsageRecoveryHeld, err)
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
