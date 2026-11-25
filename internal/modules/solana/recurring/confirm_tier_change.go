package recurring

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	safecast "github.com/ccoveille/go-safecast/v2"
	solanago "github.com/gagliardetto/solana-go"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
	"github.com/open-rails/openrails/internal/modules/orders/observed"
	"github.com/open-rails/openrails/internal/modules/solana/settlement"
	"github.com/open-rails/openrails/internal/modules/solana/solanasubs"
	submod "github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

type tierChangeTransactor interface {
	MerchantTx(ctx context.Context, fn func(context.Context, pgx.Tx) error) error
}

// ConfirmTierChangeInput describes a confirmed on-chain tier change to mirror.
// The OLD on-chain identifiers are loaded from the stored solana_subscriptions
// row; the NEW terms are the canonical server-resolved values from the new
// price's plan config (never client-supplied).
type ConfirmTierChangeInput struct {
	Signature string // the wallet's atomic tier-change tx signature

	// Reference is the address the pending change was prepared with: the
	// transaction must carry it. Required. OrderAttemptID, when set, also
	// claims the transaction for that purchase.
	OrderAttemptID uuid.UUID
	Reference      string

	// OldSubscriptionID is the lifecycle subscription being changed FROM (the
	// caller has already authorized ownership).
	OldSubscriptionID uuid.UUID

	// Acting user + new-plan identity (for the new membership).
	UserID       string
	NewPriceID   uuid.UUID
	NewProductID uuid.UUID

	// New on-chain subscription account the atomic tx created (from the prepare
	// step's result). This is the rail_subscription_id of the new membership +
	// the subscription_pda of the new row.
	NewSubscriptionPDA string

	// NewPlanOwner and NewTreasury are the new plan's owner and destination,
	// from the new price's Solana rail config.
	NewPlanOwner string
	NewTreasury  string

	// New plan terms (canonical, from the new price's Solana rail config).
	NewPlanID          uint64
	NewMintSymbol      string
	NewAmountBaseUnits uint64 // full per-cycle pull amount (token base units)
	NewPeriodHours     uint64
	NewPlanCreatedAt   int64

	// NewFiatAmount is the full price; the signed first-charge quote cannot exceed it.
	NewFiatAmount int64
	NewCurrency   string

	// IsUpgrade selects the first pull and recorded charge: an upgrade pulls
	// period 1 in the tx (next pull = now + new period, prorated charge
	// recorded); a downgrade charges nothing and defers the first pull to the
	// old period end.
	IsUpgrade bool

	// FirstChargeBaseUnits and FirstChargeMicros are the prorated first pull
	// and its fiat quote, frozen when the change was prepared: an upgrade must
	// carry exactly both. A downgrade pulls nothing and quotes zero.
	FirstChargeBaseUnits uint64
	FirstChargeMicros    int64

	// OldPeriodEndsAt is the OLD lifecycle subscription's CurrentPeriodEndsAt — the
	// downgrade's deferred first-pull time. Required for a downgrade.
	OldPeriodEndsAt *time.Time
}

// ConfirmTierChangeResult is the mirrored new subscription.
type ConfirmTierChangeResult struct {
	Receipt          *models.SolanaTierChangeReceipt
	AlreadyConfirmed bool
}

// ErrTierChangeConflict: the landed transaction is this change, but the
// subscription no longer takes it (it ended, or another change landed first).
// The chain moved and nothing was recorded: an operator settles it.
var ErrTierChangeConflict = errors.New("recurring: the landed plan change no longer applies to the subscription")

// ReplayTierChange accepts only the exact operation previously committed.
func ReplayTierChange(sub *models.Subscription, priceID uuid.UUID, signature string) (*ConfirmTierChangeResult, error) {
	receipt, err := sub.SolanaTierChangeReceipt()
	if err != nil {
		return nil, fmt.Errorf("read tier-change receipt: %w", err)
	}
	if receipt == nil {
		return nil, nil
	}
	if receipt.Signature != signature || receipt.PriceID != priceID {
		return nil, fmt.Errorf("%w: subscription already changed by a different transaction", ErrPaymentUnverified)
	}
	return &ConfirmTierChangeResult{Receipt: receipt, AlreadyConfirmed: true}, nil
}

// ConfirmTierChangeService confirms the subscriber's atomic tier-change tx
// (built by PrepareTierChangeService). It proves from the chain that the tx is
// subscriber-signed, cancels the old subscription, subscribes to the new plan's
// terms and, for an upgrade, pulls the co-signed prorated charge. Only then
// does it mirror in one DB transaction: cancel the old membership and row
// (releasing the tier-group slot), create the new membership and upsert the new
// active row. Retries return the receipt saved with the old membership.
type ConfirmTierChangeService struct {
	chain       solanaint.ChainReader
	lifecycle   *submod.SubscriptionLifecycleService
	store       *solanasubs.SolanaSubscriptionRepo
	memberships *submod.SubscriptionRepo
	transactor  tierChangeTransactor
	tokens      Tokens
	now         func() time.Time
	Log         *slog.Logger
}

// NewConfirmTierChangeService builds a ConfirmTierChangeService. network
// ("mainnet"/"devnet") resolves the recurring mint for the new row.
func NewConfirmTierChangeService(logger *slog.Logger, chain solanaint.ChainReader, lifecycle *submod.SubscriptionLifecycleService, store *solanasubs.SolanaSubscriptionRepo, memberships *submod.SubscriptionRepo, transactor tierChangeTransactor, tokens Tokens) *ConfirmTierChangeService {
	return &ConfirmTierChangeService{
		chain:       chain,
		lifecycle:   lifecycle,
		store:       store,
		memberships: memberships,
		transactor:  transactor,
		tokens:      tokens,
		now:         time.Now,
		Log:         logger,
	}
}

// Confirm verifies the landed transaction is this tier change and mirrors it.
// Anything short of a successful, confirmed transaction proving the switch
// returns an error and leaves the DB untouched.
func (s *ConfirmTierChangeService) Confirm(ctx context.Context, in ConfirmTierChangeInput) (*ConfirmTierChangeResult, error) {
	if in.OldSubscriptionID == uuid.Nil {
		return nil, fmt.Errorf("recurring: old subscription id is required")
	}
	if in.Signature == "" {
		return nil, fmt.Errorf("recurring: signature is required")
	}
	oldSub, err := s.memberships.GetByID(ctx, in.OldSubscriptionID)
	if err != nil {
		return nil, fmt.Errorf("load tier-change membership: %w", err)
	}
	if oldSub.Rail != models.RailSolana || oldSub.CustomerID.String() != in.UserID {
		return nil, fmt.Errorf("%w: old subscription does not belong to this subscriber", ErrPaymentUnverified)
	}
	// A receipt of another transaction is judged once this one is qualified.
	if result, err := ReplayTierChange(oldSub, in.NewPriceID, in.Signature); result != nil || (err != nil && !errors.Is(err, ErrPaymentUnverified)) {
		return result, err
	}
	if in.Reference == "" {
		return nil, fmt.Errorf("recurring: a plan change is qualified against its reference")
	}
	if in.NewSubscriptionPDA == "" {
		return nil, fmt.Errorf("recurring: new subscription pda is required")
	}
	if in.NewAmountBaseUnits == 0 || in.NewPeriodHours == 0 {
		return nil, fmt.Errorf("recurring: invalid new plan terms (amount/period)")
	}
	if in.IsUpgrade && (in.NewProductID == uuid.Nil || in.FirstChargeBaseUnits == 0) {
		return nil, fmt.Errorf("recurring: upgrade requires the new plan's product and its quoted first charge")
	}
	if !in.IsUpgrade && (in.OldPeriodEndsAt == nil || in.OldPeriodEndsAt.IsZero()) {
		return nil, fmt.Errorf("recurring: downgrade requires the old period end (deferred first pull)")
	}

	if s.transactor == nil {
		return nil, fmt.Errorf("recurring: tier-change transaction manager is required")
	}

	// The old row is canceled and carries its identity onto the new row.
	oldRow, err := s.store.GetBySubscriptionID(ctx, in.OldSubscriptionID)
	if err != nil {
		return nil, fmt.Errorf("recurring: load old solana subscription: %w", err)
	}
	if oldRow == nil {
		return nil, fmt.Errorf("recurring: no solana subscription for %s", in.OldSubscriptionID)
	}

	// The switch (cancel-old + subscribe-new [+ transfer]) is all-or-nothing
	// on-chain, so one landed transaction proving every part proves the change.
	payment, err := fetchLanded(ctx, s.chain, in.Signature)
	if err != nil {
		return nil, err
	}
	charged, err := s.verifyLanded(ctx, payment, oldRow, in)
	if err != nil {
		return nil, err
	}
	// The quote the cranker signed: the upgrade's charge, nothing for a downgrade.
	chargeAmount, err := payment.tierChangeCharge(in.NewCurrency, in.NewFiatAmount)
	if err != nil {
		return nil, err
	}
	if chargeAmount != in.FirstChargeMicros || (!in.IsUpgrade && chargeAmount != 0) {
		return nil, fmt.Errorf("%w: the signed quote is not the prepared one", ErrPaymentUnverified)
	}

	// Mirror in one DB transaction: a failure leaves no partial state for the
	// idempotency guard to mistake for a completed switch.

	now := s.now().UTC()
	if landedAt := payment.landedAt(); landedAt != nil {
		now = *landedAt
	}
	newPeriodStart := now
	var newPeriodEnd time.Time
	if in.IsUpgrade {
		// Period 1 was pulled atomically in the tx, so the membership's current
		// period is [now, now+new_period] and the cranker's first pull is period 2.
		newPeriodHoursI64, safeErr := safecast.Convert[int64](in.NewPeriodHours)
		if safeErr != nil {
			return nil, fmt.Errorf("recurring: confirm tier-change: new period hours overflow: %w", safeErr)
		}
		newPeriodEnd = now.Add(time.Duration(newPeriodHoursI64) * time.Hour)
	} else {
		// Downgrade: no charge now; the paid higher tier runs to the old
		// period end, then the lower tier rebills.
		newPeriodEnd = in.OldPeriodEndsAt.UTC()
	}

	newPDA := in.NewSubscriptionPDA

	// Recorded first charge: the prorated amount actually pulled on-chain for an
	// upgrade; nothing for a downgrade (deferred, no charge).
	var paymentMeta map[string]any
	if in.IsUpgrade {
		paymentMeta = map[string]any{
			"solana_tier_change":             "upgrade",
			"solana_first_charge_base_units": charged,
		}
	} else {
		paymentMeta = map[string]any{"solana_tier_change": "downgrade"}
	}

	createParams := &submod.CreateMembershipParams{
		Succeeds:              &in.OldSubscriptionID,
		UserID:                in.UserID,
		PriceID:               in.NewPriceID,
		Rail:                  models.RailSolana,
		RailSubscriptionID:    &newPDA,
		Amount:                chargeAmount,
		AmountProvided:        true,
		Currency:              in.NewCurrency,
		CurrentPeriodStartsAt: &newPeriodStart,
		CurrentPeriodEndsAt:   &newPeriodEnd,
		PaymentMetadata:       paymentMeta,
	}
	if in.IsUpgrade {
		createParams.TransactionID = in.Signature
		// The landed transfer is the verified bill: the new plan's prorated
		// first period, settled by this on-chain charge.
		hours, err := safecast.Convert[int](in.NewPeriodHours)
		if err != nil {
			return nil, fmt.Errorf("recurring: confirm tier-change: new period hours overflow: %w", err)
		}
		createParams.ProviderBill = &observed.Bill{PSPID: oldSub.PspID, CustomerID: oldSub.CustomerID, Owner: string(models.ProviderCollectionPolicy(string(models.RailSolana))),
			Kind: "initial", SourceKind: "solana_tier_change", SourceKey: in.Signature, ProviderReference: newPDA,
			Currency: moneyutil.NormalizeCurrency(in.NewCurrency), Total: chargeAmount, PeriodStart: newPeriodStart, PeriodEnd: newPeriodEnd, DueAt: newPeriodStart,
			Settled: true, SettlementSource: "cash",
			Lines: []observed.Line{{PriceID: &in.NewPriceID, ProductID: &in.NewProductID, Description: "Subscription upgrade", UnitAmount: chargeAmount, Amount: chargeAmount, BillingIntervalHours: &hours}}}
	}

	var (
		result        *ConfirmTierChangeResult
		notifications []*models.NotificationEvent
	)
	err = s.transactor.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		txDB := db.NewWithPgxTx(s.Log, tx)
		locked, err := s.memberships.LockTierChangeTx(ctx, txDB, in.OldSubscriptionID)
		if err != nil {
			return err
		}
		if locked.Rail != models.RailSolana || locked.CustomerID.String() != in.UserID {
			return fmt.Errorf("%w: old subscription does not belong to this subscriber", ErrPaymentUnverified)
		}
		result, err = ReplayTierChange(locked, in.NewPriceID, in.Signature)
		switch {
		case result != nil:
			return nil
		case errors.Is(err, ErrPaymentUnverified):
			return fmt.Errorf("%w: %v", ErrTierChangeConflict, err)
		case err != nil:
			return err
		case locked.Status != models.StatusActive:
			return fmt.Errorf("%w: it is %s", ErrTierChangeConflict, locked.Status)
		}
		if in.OrderAttemptID != uuid.Nil {
			if err := settlement.ClaimOrder(ctx, txDB, in.OrderAttemptID, in.Signature); err != nil {
				return err
			}
		}

		// Release the old row's live tier-group slot before inserting the new
		// membership. Both operations share this transaction, so any later error
		// restores the old membership and its access automatically.
		cancelType := models.CancelTypeMerchant
		cancelResult, cancelErr := s.lifecycle.CancelMembershipTx(ctx, txDB, &submod.CancelMembershipParams{
			SubscriptionID: &in.OldSubscriptionID,
			CancelType:     cancelType,
			RevokeAccess:   true,
		})
		if cancelErr != nil {
			return fmt.Errorf("recurring: mirror old-membership cancel: %w", cancelErr)
		}
		if oldRow.ID != uuid.Nil {
			if err := s.store.SetStatusTx(ctx, txDB, oldRow.ID, models.SolanaSubscriptionCanceled); err != nil {
				return fmt.Errorf("recurring: mark old solana subscription canceled: %w", err)
			}
		}

		newSub, createNotifications, err := s.lifecycle.CreateMembershipTx(ctx, txDB, createParams)
		if err != nil {
			return fmt.Errorf("recurring: create new membership: %w", err)
		}

		// Subscriber + merchant + authority are unchanged. The new plan and
		// subscription PDAs identify the newly mirrored on-chain membership.
		newRow, buildErr := s.buildNewRow(ctx, oldRow, newSub.ID, in, newPDA, newPeriodEnd)
		if buildErr != nil {
			return buildErr
		}
		if in.IsUpgrade {
			newRow.LastSignature = &in.Signature
			newRow.LastPulledPeriodStartsAt = &newPeriodStart
		}
		if err := s.store.UpsertTx(ctx, txDB, newRow); err != nil {
			return fmt.Errorf("recurring: persist new solana subscription: %w", err)
		}
		receipt := &models.SolanaTierChangeReceipt{
			Signature: in.Signature, PriceID: in.NewPriceID, SubscriptionID: newSub.ID,
			AmountDueNow: chargeAmount, Currency: in.NewCurrency, NextChargeAmount: in.NewFiatAmount,
			NextChargeDate: *newSub.CurrentPeriodEndsAt, IsUpgrade: in.IsUpgrade,
		}
		if err := s.memberships.SaveSolanaTierChangeTx(ctx, txDB, in.OldSubscriptionID, receipt); err != nil {
			return fmt.Errorf("persist tier-change receipt: %w", err)
		}
		result = &ConfirmTierChangeResult{Receipt: receipt}

		notifications = append(notifications, createNotifications...)
		if cancelResult != nil {
			notifications = append(notifications, cancelResult.Notifications...)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return result, nil
}

// verifyLanded proves the landed tier-change transaction
// switched this subscriber from the old subscription to the new plan's terms,
// returning the upgrade's pulled amount (0 for a downgrade).
func (s *ConfirmTierChangeService) verifyLanded(ctx context.Context, payment *landedTx, oldRow *models.SolanaSubscription, in ConfirmTierChangeInput) (uint64, error) {
	var err error
	if err := payment.references(in.Reference); err != nil {
		return 0, err
	}
	var keys [6]solanago.PublicKey
	for i, v := range []string{oldRow.SubscriberWallet, in.NewPlanOwner, s.newMint(ctx, oldRow, in.NewMintSymbol), oldRow.PlanPDA, oldRow.SubscriptionPDA, in.NewTreasury} {
		if keys[i], err = solanago.PublicKeyFromBase58(v); err != nil {
			return 0, fmt.Errorf("recurring: stored solana subscription %s is malformed: %w", oldRow.SubscriptionPDA, err)
		}
	}
	subscriber, owner, mint, oldPlan, oldSub, treasury := keys[0], keys[1], keys[2], keys[3], keys[4], keys[5]
	if err := payment.cancels(subscriber, oldPlan, oldSub); err != nil {
		return 0, err
	}
	terms := subscription{
		subscriber: subscriber, owner: owner, treasury: treasury, mint: mint, planID: in.NewPlanID,
		amount: in.NewAmountBaseUnits, periodHours: in.NewPeriodHours, createdAt: in.NewPlanCreatedAt,
	}
	_, newSub, _, err := terms.addresses()
	if err != nil {
		return 0, err
	}
	if newSub.String() != in.NewSubscriptionPDA {
		return 0, fmt.Errorf("%w: new subscription %s is not the subscriber's account for plan %d", ErrPaymentUnverified, in.NewSubscriptionPDA, in.NewPlanID)
	}
	if err := payment.subscribes(terms); err != nil {
		return 0, err
	}
	if !in.IsUpgrade {
		// The cranker co-signed the message, so the wallet could not alter it.
		if !payment.signedBy(owner) {
			return 0, fmt.Errorf("%w: not co-signed by the plan's cranker", ErrPaymentUnverified)
		}
		return 0, nil
	}
	return payment.pulls(terms, in.FirstChargeBaseUnits)
}

func (l *landedTx) tierChangeCharge(currency string, maximum int64) (int64, error) {
	var amount int64
	found := false
	for _, ix := range l.tx.Message.Instructions {
		program, err := l.tx.ResolveProgramIDIndex(ix.ProgramIDIndex)
		if err != nil || !program.Equals(solanago.MemoProgramID) {
			continue
		}
		quote, ok := strings.CutPrefix(string(ix.Data), tierChangeQuotePrefix)
		if !ok {
			continue
		}
		quotedCurrency, value, ok := strings.Cut(quote, ":")
		if found || !ok || quotedCurrency != moneyutil.NormalizeCurrency(currency) {
			return 0, fmt.Errorf("%w: invalid tier-change quote", ErrPaymentUnverified)
		}
		amount, err = strconv.ParseInt(value, 10, 64)
		if err != nil || amount < 0 || amount > maximum {
			return 0, fmt.Errorf("%w: invalid tier-change charge", ErrPaymentUnverified)
		}
		found = true
	}
	if !found {
		return 0, fmt.Errorf("%w: missing signed tier-change quote", ErrPaymentUnverified)
	}
	return amount, nil
}

// newMint resolves the new plan's mint, falling back to the old row's mint
// (the same for a same-group change).
func (s *ConfirmTierChangeService) newMint(ctx context.Context, oldRow *models.SolanaSubscription, symbol string) string {
	if resolved, err := ResolveRecurringMintFromTokens(symbol, s.tokens.of(ctx)); err == nil && resolved != "" {
		return resolved
	}
	return oldRow.Mint
}

// buildNewRow assembles the NEW solana_subscriptions row: the new plan, its
// owner and the treasury it pays.
func (s *ConfirmTierChangeService) buildNewRow(ctx context.Context, oldRow *models.SolanaSubscription, newSubID uuid.UUID, in ConfirmTierChangeInput, newPDA string, nextPullAt time.Time) (*models.SolanaSubscription, error) {
	mintStr := s.newMint(ctx, oldRow, in.NewMintSymbol)

	owner, err := parseKey("new plan owner", in.NewPlanOwner)
	if err != nil {
		return nil, err
	}
	newPlanPDA, _, err := subscriptions.DerivePlanPDA(owner, in.NewPlanID)
	if err != nil {
		return nil, err
	}

	return &models.SolanaSubscription{
		SubscriptionID:           newSubID,
		MerchantID:               oldRow.MerchantID,
		SubscriberWallet:         oldRow.SubscriberWallet,
		AuthorityPDA:             oldRow.AuthorityPDA,
		SubscriptionPDA:          newPDA,
		PlanPDA:                  newPlanPDA.String(),
		MerchantAddress:          in.NewPlanOwner,
		Treasury:                 in.NewTreasury,
		Mint:                     mintStr,
		PlanCreatedAtFingerprint: in.NewPlanCreatedAt,
		NextPullAt:               nextPullAt,
		Status:                   models.SolanaSubscriptionActive,
	}, nil
}
