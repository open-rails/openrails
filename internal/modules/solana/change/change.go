// Package change records Solana plan changes from the chain. Preparing one
// stores its agreement as a solana_tier_change operation before the wallet is
// handed the transaction; the operation then reads the chain for a transaction
// naming its reference, qualifies it against that agreement and settles the
// change. The wallet's confirm only makes it look sooner.
package change

import (
	"context"
	"errors"
	"fmt"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/alerting"
	"github.com/open-rails/openrails/internal/modules/solana/recurring"
	"github.com/open-rails/openrails/internal/modules/solana/solanasubs"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/timeutil"
)

const (
	// firstLook is how long after preparing the chain is first read; lookFloor
	// and lookCeiling bound the wait between later reads, half the change's age.
	firstLook   = 5 * time.Second
	lookFloor   = 5 * time.Second
	lookCeiling = time.Minute
	// neverLanded marks an operation whose transaction provably cannot land.
	neverLanded = "never_landed"
)

// ErrNoPendingChange: no prepared change of the subscription takes the
// signature.
var ErrNoPendingChange = errors.New("solana: no pending plan change takes this transaction")

// Chain reads a landed transaction and a reference's finalized history
// (satisfied by *solana.MerchantChainReader).
type Chain interface {
	GetTransaction(ctx context.Context, signature solanago.Signature) (*rpc.GetTransactionResult, error)
	ReferenceHistory(ctx context.Context, reference string, terminal solanaint.ChainTerminal) ([]solanaint.SignatureInfo, bool, error)
}

// Lifecycle is the membership surface a settled change drives (satisfied by
// *subscriptions.SubscriptionLifecycleService).
type Lifecycle interface {
	CancelMembershipTx(ctx context.Context, txDB *db.DB, params *subscriptions.CancelMembershipParams) (*subscriptions.CancelMembershipTxResult, error)
	CreateMembershipTx(ctx context.Context, txDB *db.DB, params *subscriptions.CreateMembershipParams) (*models.Subscription, []*models.NotificationEvent, error)
}

// Handler is the solana_tier_change operation and the surface the change
// route records and hints through. A nil Chain or Lifecycle parks every look.
type Handler struct {
	DB        *db.DB
	Clock     clockwork.Clock
	Chain     Chain
	Lifecycle Lifecycle
	Tokens    recurring.Tokens
	Store     *intents.Store
}

func (h *Handler) now() time.Time { return timeutil.FirstClock(h.Clock).Now().UTC() }

func (h *Handler) Type() string { return solanasubs.ChangeType }

// A plan change sends nothing: a read that failed is simply made again.
func (h *Handler) Backoff(attempts int32) time.Duration {
	return min(time.Duration(max(attempts, 1))*lookFloor, lookCeiling)
}

// Record stores a prepared change before its transaction reaches the wallet.
func (h *Handler) Record(ctx context.Context, merchantID, pspID uuid.UUID, c solanasubs.Change) error {
	if h.Store == nil {
		return errors.New("solana plan change: no operation store")
	}
	_, err := h.Store.Enqueue(ctx, intents.EnqueueParams{
		MerchantID: merchantID, Provider: string(models.RailSolana), IntentType: solanasubs.ChangeType,
		SubscriptionID: &c.OldSubscriptionID, PriceID: &c.PriceID, PspID: pspID, Payload: c,
		IdempotencyKey: solanasubs.ChangeKey(c.Reference), NextAttemptAt: c.PreparedAt.Add(firstLook),
		Origin: intents.OriginUser, OriginReason: "solana plan change",
	})
	return err
}

// Hint settles the subscription's pending change to priceID that signature
// lands, now rather than at its next look. The signature only names a
// transaction to read: the change is qualified from the chain all the same.
func (h *Handler) Hint(ctx context.Context, subscriptionID, priceID uuid.UUID, signature string) (*recurring.ConfirmTierChangeResult, error) {
	pending, err := solanasubs.PendingChanges(ctx, h.DB, subscriptionID)
	if err != nil {
		return nil, err
	}
	refused := ErrNoPendingChange
	for _, row := range pending {
		c, err := solanasubs.DecodeChange(row)
		if err != nil || c.PriceID != priceID {
			continue
		}
		result, err := h.settle(db.WithPSPID(ctx, pspOf(row)), c, signature)
		if !errors.Is(err, recurring.ErrPaymentUnverified) {
			return result, err
		}
		refused = err
	}
	return nil, refused
}

func pspOf(row gen.BillingProviderIntent) uuid.UUID {
	if row.PspID == nil {
		return uuid.Nil
	}
	return *row.PspID
}

// settle qualifies signature against the frozen agreement and records the
// change with its bill, payment and access period in one transaction.
func (h *Handler) settle(ctx context.Context, c solanasubs.Change, signature string) (*recurring.ConfirmTierChangeResult, error) {
	if h.Chain == nil || h.Lifecycle == nil {
		return nil, errors.New("solana plan change: chain reader or lifecycle not wired")
	}
	svc := recurring.NewConfirmTierChangeService(h.Chain, h.Lifecycle, solanasubs.NewSolanaSubscriptionRepo(h.DB), subscriptions.NewSubscriptionRepo(h.DB), h.DB, h.Tokens)
	return svc.Confirm(ctx, recurring.ConfirmTierChangeInput{
		Signature: signature, Reference: c.Reference,
		OldSubscriptionID: c.OldSubscriptionID, UserID: c.CustomerID.String(),
		NewPriceID: c.PriceID, NewProductID: c.ProductID, NewSubscriptionPDA: c.NewSubscriptionPDA,
		NewPlanOwner: c.PlanOwner, NewTreasury: c.Treasury, NewPlanID: c.PlanID, NewMintSymbol: c.MintSymbol,
		NewAmountBaseUnits: c.AmountBaseUnits, NewPeriodHours: c.PeriodHours, NewPlanCreatedAt: c.PlanCreatedAt,
		NewFiatAmount: c.Amount, NewCurrency: c.Currency,
		IsUpgrade: c.Upgrade, FirstChargeBaseUnits: c.FirstChargeBaseUnits, FirstChargeMicros: c.FirstChargeMicros,
		OldPeriodEndsAt: c.OldPeriodEndsAt,
	})
}

// CheckRelevance ends a change whose transaction was proven never to land.
func (h *Handler) CheckRelevance(_ context.Context, intent gen.BillingProviderIntent) (intents.Relevance, error) {
	if _, err := solanasubs.DecodeChange(intent); err != nil {
		return intents.SupersededBy("unusable Solana plan change: " + err.Error()), nil
	}
	if intents.EvidenceString(intent, neverLanded) != "" {
		return intents.SupersededBy("the prepared transaction expired without landing"), nil
	}
	return intents.StillRelevant(), nil
}

// Execute reads the chain for the change's transaction. Found and qualified,
// the change is settled; proven never to have landed, it is released; anything
// else waits for the next look.
func (h *Handler) Execute(ctx context.Context, intent gen.BillingProviderIntent) intents.Outcome {
	if h.Chain == nil || h.Lifecycle == nil || h.Store == nil {
		return intents.Parked("solana plan change not fully wired (no chain reader/lifecycle)")
	}
	c, err := solanasubs.DecodeChange(intent)
	if err != nil {
		return intents.Terminal(err.Error())
	}
	history, closed, err := h.Chain.ReferenceHistory(ctx, c.Reference, solanaint.ChainTerminal{LastValidBlockHeight: c.LastValidBlockHeight})
	if err != nil {
		return intents.Retryable("read the change's reference: " + err.Error())
	}
	// Oldest first: the first transaction that qualifies is the change.
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].HasError {
			continue
		}
		signature := history[i].Signature
		_, err := h.settle(ctx, c, signature)
		switch {
		case err == nil:
			return intents.Succeeded(map[string]any{"transaction_id": signature})
		case errors.Is(err, recurring.ErrPaymentUnverified):
			// Anyone may name the reference: not this change's transaction.
		case errors.Is(err, recurring.ErrPaymentNotLanded):
			return intents.Deferred("the change's transaction is not readable yet", h.now().Add(lookFloor))
		case errors.Is(err, recurring.ErrTierChangeConflict):
			if rerr := h.review(ctx, c, signature, err); rerr != nil {
				return intents.Retryable("record the unsettled plan change: " + rerr.Error())
			}
			return intents.TerminalWithEvidence(err.Error(), map[string]any{"transaction_id": signature})
		default:
			return intents.Retryable("settle the plan change: " + err.Error())
		}
	}
	if closed {
		// Every block that could hold the transaction is finalized and none
		// does. The next run ends the operation on this recorded proof.
		if err := h.Store.RecordProgress(ctx, intent.ID, map[string]any{neverLanded: h.now().Format(time.RFC3339)}); err != nil {
			return intents.Retryable("record that the change never landed: " + err.Error())
		}
		return intents.Deferred("the prepared transaction expired without landing", h.now())
	}
	now := h.now()
	return intents.Deferred("awaiting the wallet's transaction", now.Add(min(max(now.Sub(c.PreparedAt)/2, lookFloor), lookCeiling)))
}

// Verify is the same read: a change sends nothing, so there is nothing to
// resolve before looking again.
func (h *Handler) Verify(ctx context.Context, intent gen.BillingProviderIntent) intents.Outcome {
	outcome := h.Execute(ctx, intent)
	if outcome.Class == intents.OutcomeParked {
		return intents.Retryable(outcome.Reason)
	}
	return outcome
}

// review surfaces a change that landed and could not be recorded: the wallet
// canceled, subscribed and (an upgrade) paid on chain.
func (h *Handler) review(ctx context.Context, c solanasubs.Change, signature string, cause error) error {
	kind := "downgrade"
	if c.Upgrade {
		kind = "upgrade"
	}
	return alerting.RecordLedgerRepair(ctx, h.DB, h.now(), alerting.LedgerRepair{
		Provider:       string(models.RailSolana),
		Operation:      "solana_tier_change_unrecorded",
		TransactionID:  signature,
		CustomerID:     c.CustomerID.String(),
		IdempotencyKey: "solana-tier-change-unrecorded:" + signature,
		SubscriptionID: &c.OldSubscriptionID,
		Err:            fmt.Errorf("plan change %s landed but was not recorded: %w", signature, cause),
		Metadata: map[string]any{
			"reference": c.Reference, "tier_change": kind, "price_id": c.PriceID.String(),
			"new_subscription_pda": c.NewSubscriptionPDA, "first_charge_base_units": c.FirstChargeBaseUnits,
		},
	})
}
