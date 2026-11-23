package handlers

import (
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	safecast "github.com/ccoveille/go-safecast/v2"
	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	solanarpc "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/payment"
	solanamodule "github.com/open-rails/openrails/internal/modules/solana"
	"github.com/open-rails/openrails/internal/modules/solana/recurring"
	"github.com/open-rails/openrails/internal/modules/solana/solanasubs"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// resolvedTierChange holds the server-resolved facts both tier-change endpoints
// need: the OLD lifecycle subscription + its stored on-chain row, the NEW price +
// its canonical plan terms, the upgrade/downgrade direction, and (for an upgrade)
// the Model-B prorated first charge in token base units.
type resolvedTierChange struct {
	oldSub    *models.Subscription
	oldRow    *models.SolanaSubscription
	newPrice  *models.Price
	newTerms  solanaResolvedPlanTerms
	isUpgrade bool
	// firstChargeMicros and firstChargeBaseUnits are the Model-B prorated
	// first pull for an upgrade (fiat micros, token base units); 0 for a
	// downgrade.
	firstChargeMicros    int64
	firstChargeBaseUnits uint64
}

// newTreasury is where the new plan pays: the treasury it was published
// with, or the subscription's own.
func (r *resolvedTierChange) newTreasury() string {
	if r.newTerms.treasury != "" {
		return r.newTerms.treasury
	}
	return r.oldRow.Treasury
}

type solanaResolvedPlanTerms struct {
	planID     uint64
	mintSymbol string
	amount     uint64
	period     uint64
	createdAt  int64
	// owner is the plan's owner; treasury the destination it was published
	// with.
	owner    string
	treasury string
}

func authorizeSolanaTierChange(r *httprequest.Request, subscriptionID uuid.UUID) (*models.Subscription, error) {
	if r.State.SubscriptionService == nil || r.State.PriceService == nil || r.State.ProductService == nil {
		return nil, api.Coded(billing.CodeServiceUnavailable, "subscriptions are not configured")
	}
	scope, ok := r.CustomerScope()
	if !ok {
		return nil, api.Coded(billing.CodeAuthenticationRequired, "")
	}

	// Authorize: the acting user must own the OLD lifecycle subscription.
	oldSub, err := r.State.SubscriptionService.GetByID(r.Request.Context(), subscriptionID)
	if err != nil {
		if db.IsNotFound(err) {
			return nil, api.Coded(billing.CodeSubscriptionNotFound, "")
		}
		return nil, fmt.Errorf("load subscription: %w", err)
	}
	if oldSub.CustomerID != scope.Customer().UUID() {
		return nil, api.Coded(billing.CodeSubscriptionNotFound, "")
	}
	if oldSub.Rail != models.RailSolana {
		return nil, api.Coded(billing.CodeSubscriptionChangeUnsupportedOnRail, "subscription is not a Solana subscription")
	}
	return oldSub, nil
}

func resolveSolanaTierChange(r *httprequest.Request, oldSub *models.Subscription, newPriceIDStr string) (*resolvedTierChange, error) {
	subscriptionID := oldSub.ID

	// Load the OLD on-chain row (subscriber/merchant identifiers for the atomic tx).
	oldRow, err := solanasubs.NewSolanaSubscriptionRepo(r.State.DB).GetBySubscriptionID(r.Request.Context(), subscriptionID)
	if err != nil || oldRow == nil {
		return nil, api.Coded(billing.CodeSubscriptionChangeProviderConflict, "no on-chain record for this subscription")
	}

	// Resolve the NEW price + its published plan terms.
	typedNewPriceID, err := billing.ParsePriceID(newPriceIDStr)
	if err != nil || typedNewPriceID.IsZero() {
		return nil, api.Coded(billing.CodeInvalidParam, "invalid price_id").WithParam("price_id")
	}
	newPrice, err := r.State.PriceService.GetByPublicID(r.Request.Context(), typedNewPriceID)
	if err != nil || newPrice == nil {
		return nil, api.Coded(billing.CodePriceNotFound, "target price not found")
	}
	if !newPrice.IsPurchasable() {
		return nil, api.Coded(billing.CodeSubscriptionChangeTargetInactive, "target price is not available")
	}
	newCfg := newPrice.ForPSP(oldSub.PspID).PSPLinkForRail(models.RailSolana)
	newTerms, ok := parseResolvedPlanTerms(newCfg)
	if !ok {
		return nil, api.Coded(billing.CodeSubscriptionChangeRequiresLinkedPlan, "target price is not configured for Solana recurring billing")
	}

	// Load OLD + NEW products: SolanaTierChange decides the change.
	oldPrice, err := r.State.PriceService.GetByID(r.Request.Context(), oldSub.PriceID)
	if err != nil || oldPrice == nil {
		return nil, fmt.Errorf("load current price: %w", errors.Join(err, errNilRow))
	}
	newProduct, err := r.State.ProductService.GetByID(r.Request.Context(), newPrice.ProductID)
	if err != nil || newProduct == nil {
		return nil, api.Coded(billing.CodeProductNotFound, "target product not found")
	}
	oldProduct, err := r.State.ProductService.GetByID(r.Request.Context(), oldPrice.ProductID)
	if err != nil || oldProduct == nil {
		return nil, fmt.Errorf("load current product: %w", errors.Join(err, errNilRow))
	}
	isUpgrade, err := payment.SolanaTierChange(oldSub, oldProduct, newProduct, oldPrice, newPrice)
	if err != nil {
		return nil, err
	}

	out := &resolvedTierChange{
		oldSub:    oldSub,
		oldRow:    oldRow,
		newPrice:  newPrice,
		newTerms:  newTerms,
		isUpgrade: isUpgrade,
	}

	if isUpgrade {
		ctx := db.WithPSPID(r.Request.Context(), oldSub.PspID)
		quote, err := payment.QuoteSolanaUpgrade(ctx, r.State.DB, oldSub, oldPrice, newPrice, nowOrDefault(r))
		if err != nil {
			return nil, err
		}
		firstChargeMicros := quote.ChargeNow
		decimals, err := solanamodule.RequireTokenDecimals(ctx, r.State.RailConfigs, newTerms.mintSymbol, r.State.SolanaMintDecimals)
		if err != nil {
			return nil, solanaFailure(r.Log, err, "resolve token decimals")
		}
		firstChargeBaseUnits, err := solanamodule.FiatMicrosToStablecoinBaseUnits(
			ctx, r.Log, moneyutil.Micros(firstChargeMicros), newTerms.mintSymbol, decimals, r.State.SolanaPriceProvider,
		)
		if err != nil {
			return nil, solanaFailure(r.Log, err, "convert the first charge")
		}
		// A genuine upgrade can round to 0 base units only when the unused old credit
		// fully covers the new price; we still need a non-zero pull to activate the
		// new on-chain subscription, so charge the smallest unit (a fraction of a
		// cent) in that degenerate case.
		if firstChargeBaseUnits == 0 {
			firstChargeBaseUnits = 1
		}
		out.firstChargeMicros = firstChargeMicros
		out.firstChargeBaseUnits = firstChargeBaseUnits
	}

	return out, nil
}

var errNilRow = errors.New("no row")

// solanaFailure is a transient outage's refusal, or err as an internal
// failure.
func solanaFailure(logger *slog.Logger, err error, step string) error {
	if refusal := solanaUnavailable(logger, err); refusal != nil {
		return refusal
	}
	return fmt.Errorf("%s: %w", step, err)
}

// parseResolvedPlanTerms parses a price's Solana rail config into the
// canonical plan terms. Returns ok=false when the config is absent or malformed.
func parseResolvedPlanTerms(cfg map[string]string) (solanaResolvedPlanTerms, bool) {
	var t solanaResolvedPlanTerms
	if cfg == nil {
		return t, false
	}
	var err error
	if t.planID, err = strconv.ParseUint(cfg["plan_id"], 10, 64); err != nil {
		return t, false
	}
	if t.amount, err = strconv.ParseUint(cfg["amount_base_units"], 10, 64); err != nil || t.amount == 0 {
		return t, false
	}
	if t.period, err = strconv.ParseUint(cfg["period_hours"], 10, 64); err != nil || t.period == 0 {
		return t, false
	}
	t.createdAt, _ = strconv.ParseInt(cfg["created_at"], 10, 64)
	t.mintSymbol = strings.TrimSpace(cfg["mint_symbol"])
	t.owner, t.treasury = strings.TrimSpace(cfg["merchant_address"]), strings.TrimSpace(cfg["treasury"])
	if t.mintSymbol == "" || t.owner == "" {
		return t, false
	}
	return t, true
}

// nowOrDefault reads the clock the order attempt service uses (so tests can
// pin time), falling back to wall-clock.
func nowOrDefault(r *httprequest.Request) time.Time {
	if r.State != nil && r.State.OrderAttemptService != nil {
		if c := r.State.OrderAttemptService.Clock(); c != nil {
			return c.Now()
		}
	}
	return time.Now()
}

// solanaTierChange is change on the Solana rail: one atomic on-chain
// transaction the customer's wallet signs (cancel the old subscription,
// subscribe to the new plan, and for an upgrade the prorated pull), co-signed
// by the new plan's cranker. Without a signature it records the pending change
// and answers requires_action with the transaction. OpenRails then finds the
// landed transaction on chain itself and records the switch; a request
// repeated with the signature only makes it look now, and answers succeeded
// with the new subscription. Nothing is recorded before the chain confirms it.
func solanaTierChange(r *httprequest.Request, subscriptionID uuid.UUID, priceID, signature string) {
	if r.State.SolanaPrepareTierChangeService == nil || r.State.SolanaRPCResolver == nil || r.State.SubscriptionLifecycleService == nil || r.State.PaymentService == nil || r.State.DB == nil {
		r.ErrorCode(billing.CodeServiceUnavailable, "Solana recurring billing is not configured")
		return
	}
	oldSub, err := authorizeSolanaTierChange(r, subscriptionID)
	if err != nil {
		writeChangeTierError(r, err)
		return
	}
	changes := r.State.SolanaPlanChanges(r.State.Clock)
	ctx := db.WithPSPID(r.Request.Context(), oldSub.PspID)
	if signature != "" {
		target, err := billing.ParsePriceID(priceID)
		if err != nil || target.IsZero() {
			r.APIError(api.Coded(billing.CodeInvalidParam, "invalid price_id").WithParam("price_id"))
			return
		}
		targetPrice, err := r.State.PriceService.GetByPublicID(r.Request.Context(), target)
		if err != nil {
			writeChangeTierError(r, err)
			return
		}
		// The chain may have been read already: the recorded change answers.
		result, err := recurring.ReplayTierChange(oldSub, targetPrice.ID, signature)
		if err == nil && result == nil {
			result, err = changes.Hint(ctx, subscriptionID, targetPrice.ID, signature)
		}
		if err != nil {
			r.APIError(solanaClientError(r.Log, err))
			return
		}
		r.SuccessJSON(solanaTierChangeResponse(result.Receipt, target))
		return
	}
	resolved, err := resolveSolanaTierChange(r, oldSub, priceID)
	if err != nil {
		writeChangeTierError(r, err)
		return
	}
	merchantID, err := merchant.Require(ctx)
	if err != nil {
		r.ErrorCode(billing.CodeInternalError, "no merchant resolved on request")
		return
	}
	now := nowOrDefault(r).UTC()
	old := billing.SubscriptionID(subscriptionID)
	out := billing.SubscriptionChange{
		Status:           "requires_action",
		Effective:        "period_end",
		PriceID:          resolved.newPrice.PublicID,
		Rail:             billing.RailSolana,
		SubscriptionID:   &old,
		Currency:         resolved.newPrice.Currency,
		NextChargeAmount: resolved.newPrice.Amount,
		NextChargeDate:   resolved.oldSub.CurrentPeriodEndsAt,
	}
	if resolved.isUpgrade {
		periodHours, err := safecast.Convert[int64](resolved.newTerms.period)
		if err != nil {
			r.ErrorCode(billing.CodeInternalError, "the target plan's period is out of range")
			return
		}
		out.Effective, out.AmountDueNow = "now", resolved.firstChargeMicros
		next := now.Add(time.Duration(periodHours) * time.Hour)
		out.NextChargeDate = &next
	} else if resolved.oldSub.CurrentPeriodEndsAt == nil {
		// A downgrade takes over at the end of the paid period.
		r.APIError(api.Coded(billing.CodeSubscriptionChangeProviderConflict, "the subscription has no paid period to finish"))
		return
	}
	reference, err := solanarpc.GenerateReference()
	if err != nil {
		r.InternalError("generate the change's reference", err)
		return
	}
	prep, err := r.State.SolanaPrepareTierChangeService.Prepare(ctx, recurring.PrepareTierChangeInput{
		MerchantID:           merchantID,
		SubscriberWallet:     resolved.oldRow.SubscriberWallet,
		MintSymbol:           resolved.newTerms.mintSymbol,
		OldPlanPDA:           resolved.oldRow.PlanPDA,
		OldSubscriptionPDA:   resolved.oldRow.SubscriptionPDA,
		NewPlanOwner:         resolved.newTerms.owner,
		Treasury:             resolved.newTreasury(),
		NewPlanID:            resolved.newTerms.planID,
		NewAmountBaseUnits:   resolved.newTerms.amount,
		NewPeriodHours:       resolved.newTerms.period,
		NewPlanCreatedAt:     resolved.newTerms.createdAt,
		IsUpgrade:            resolved.isUpgrade,
		FirstChargeBaseUnits: resolved.firstChargeBaseUnits,
		FirstChargeMicros:    resolved.firstChargeMicros,
		Currency:             resolved.newPrice.Currency,
		Reference:            reference,
	})
	if err != nil {
		r.APIError(solanaClientError(r.Log, err))
		return
	}
	// The agreement is durable before the wallet can sign: whatever becomes of
	// this response, a transaction that lands is found by its reference.
	if err := changes.Record(ctx, merchantID.UUID(), resolved.oldSub.PspID, solanasubs.Change{
		Reference: reference, LastValidBlockHeight: prep.LastValidBlockHeight,
		OldSubscriptionID: subscriptionID, CustomerID: resolved.oldSub.CustomerID, OldPeriodEndsAt: resolved.oldSub.CurrentPeriodEndsAt,
		PriceID: resolved.newPrice.ID, ProductID: resolved.newPrice.ProductID, NewSubscriptionPDA: prep.NewSubscriptionPDA,
		PlanOwner: resolved.newTerms.owner, Treasury: resolved.newTreasury(), PlanID: resolved.newTerms.planID, MintSymbol: resolved.newTerms.mintSymbol,
		AmountBaseUnits: resolved.newTerms.amount, PeriodHours: resolved.newTerms.period, PlanCreatedAt: resolved.newTerms.createdAt,
		Amount: resolved.newPrice.Amount, Currency: resolved.newPrice.Currency,
		Upgrade: resolved.isUpgrade, FirstChargeBaseUnits: resolved.firstChargeBaseUnits, FirstChargeMicros: resolved.firstChargeMicros,
		PreparedAt: now,
	}); err != nil {
		r.InternalError("record the pending plan change", err)
		return
	}
	out.NextAction = &billing.NextAction{Type: "solana_sign_transactions", Transactions: []string{prep.Transaction}}
	r.SuccessJSON(out)
}

func solanaTierChangeResponse(receipt *models.SolanaTierChangeReceipt, priceID billing.PriceID) billing.SubscriptionChange {
	id := billing.SubscriptionID(receipt.SubscriptionID)
	out := billing.SubscriptionChange{
		Status: "succeeded", Effective: "period_end", PriceID: priceID,
		Rail: billing.RailSolana, SubscriptionID: &id, Currency: receipt.Currency,
		AmountDueNow: receipt.AmountDueNow, NextChargeAmount: receipt.NextChargeAmount,
		NextChargeDate: &receipt.NextChargeDate,
	}
	if receipt.IsUpgrade {
		out.Effective = "now"
	}
	return out
}
