package handlers

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	safecast "github.com/ccoveille/go-safecast/v2"
	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/checkout"
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

type solanaResolvedPlanTerms struct {
	planID     uint64
	mintSymbol string
	amount     uint64
	period     uint64
	createdAt  int64
}

// resolveSolanaTierChange authorizes ownership and resolves everything the
// prepare/confirm endpoints share. It returns an HTTP status + message on
// failure (the caller writes the error response).
func resolveSolanaTierChange(r *httprequest.Request, subscriptionID uuid.UUID, newPriceIDStr string) (*resolvedTierChange, int, string) {
	if r.State.SubscriptionService == nil || r.State.PriceService == nil || r.State.ProductService == nil {
		return nil, http.StatusServiceUnavailable, "subscriptions are not configured"
	}
	uc, ok := r.UserContext()
	if !ok || uc.UserID == "" {
		return nil, http.StatusUnauthorized, "User authentication required"
	}

	// Authorize: the acting user must own the OLD lifecycle subscription.
	oldSub, err := r.State.SubscriptionService.GetByID(r.Request.Context(), subscriptionID)
	if err != nil {
		if db.IsNotFound(err) {
			return nil, http.StatusNotFound, "subscription not found"
		}
		return nil, http.StatusInternalServerError, "failed to retrieve subscription"
	}
	if oldSub.CustomerID.String() != uc.UserID {
		return nil, http.StatusNotFound, "subscription not found"
	}
	if oldSub.Rail != models.RailSolana {
		return nil, http.StatusBadRequest, "subscription is not a Solana subscription"
	}

	// Load the OLD on-chain row (subscriber/merchant identifiers for the atomic tx).
	oldRow, err := solanasubs.NewSolanaSubscriptionRepo(r.State.DB).GetBySubscriptionID(r.Request.Context(), subscriptionID)
	if err != nil || oldRow == nil {
		return nil, http.StatusBadRequest, "no on-chain record for this subscription"
	}

	// Resolve the NEW price + its published plan terms.
	typedNewPriceID, err := billing.ParsePriceID(newPriceIDStr)
	if err != nil || typedNewPriceID.IsZero() {
		return nil, http.StatusBadRequest, "invalid new_price_id"
	}
	newPriceID := typedNewPriceID.UUID()
	newPrice, err := r.State.PriceService.GetByID(r.Request.Context(), newPriceID)
	if err != nil || newPrice == nil {
		return nil, http.StatusNotFound, "target price not found"
	}
	if !newPrice.IsPurchasable() {
		return nil, http.StatusBadRequest, "target price is not available"
	}
	newCfg := newPrice.ForPSP(oldSub.PspID).PSPLinkForRail(models.RailSolana)
	newTerms, ok := parseResolvedPlanTerms(newCfg)
	if !ok {
		return nil, http.StatusBadRequest, "target price is not configured for Solana recurring billing"
	}

	// Load OLD + NEW products: SolanaTierChange decides the change.
	oldPrice, err := r.State.PriceService.GetByID(r.Request.Context(), oldSub.PriceID)
	if err != nil || oldPrice == nil {
		return nil, http.StatusInternalServerError, "current price not found"
	}
	newProduct, err := r.State.ProductService.GetByID(r.Request.Context(), newPrice.ProductID)
	if err != nil || newProduct == nil {
		return nil, http.StatusNotFound, "target product not found"
	}
	oldProduct, err := r.State.ProductService.GetByID(r.Request.Context(), oldPrice.ProductID)
	if err != nil || oldProduct == nil {
		return nil, http.StatusInternalServerError, "current product not found"
	}
	isUpgrade, err := checkout.SolanaTierChange(oldSub, oldProduct, newProduct, oldPrice, newPrice)
	if err != nil {
		status := http.StatusBadRequest
		var tierErr *checkout.TierChangeError
		if errors.As(err, &tierErr) {
			status = tierErr.HTTPStatus
		}
		return nil, status, err.Error()
	}

	out := &resolvedTierChange{
		oldSub:    oldSub,
		oldRow:    oldRow,
		newPrice:  newPrice,
		newTerms:  newTerms,
		isUpgrade: isUpgrade,
	}

	if isUpgrade {
		// Model-B prorated first charge (new_full - old_unused) in micros, then
		// micros -> base units at the token's CONFIGURED decimals (#817), $1 peg
		// (depeg failsafe inside).
		quote, err := checkout.QuoteModelBUpgrade(checkout.ModelBUpgrade{
			Old: checkout.PriceAmountOf(oldPrice), New: checkout.PriceAmountOf(newPrice),
			PeriodStart: oldSub.CurrentPeriodStartsAt, PeriodEnd: oldSub.CurrentPeriodEndsAt,
			NewCycleHours: newPrice.RecurringCycleHours(),
		}, nowOrDefault(r))
		if err != nil {
			status := http.StatusBadRequest
			var tierErr *checkout.TierChangeError
			if errors.As(err, &tierErr) {
				status = tierErr.HTTPStatus
			}
			return nil, status, err.Error()
		}
		firstChargeMicros := quote.ChargeNow
		decimals, err := solanamodule.RequireTokenDecimals(r.Request.Context(), r.State.RailConfigs, newTerms.mintSymbol, r.State.SolanaMintDecimals)
		if err != nil {
			status, msg := solanaClientError(err, http.StatusInternalServerError)
			return nil, status, msg
		}
		firstChargeBaseUnits, err := solanamodule.FiatMicrosToStablecoinBaseUnits(
			r.Request.Context(), moneyutil.Micros(firstChargeMicros), newTerms.mintSymbol, decimals, r.State.SolanaPriceProvider,
		)
		if err != nil {
			status, msg := solanaClientError(err, http.StatusInternalServerError)
			return nil, status, msg
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

	return out, 0, ""
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
	if t.mintSymbol == "" {
		return t, false
	}
	return t, true
}

// nowOrDefault reads the clock the checkout attempt service uses (so tests can
// pin time), falling back to wall-clock.
func nowOrDefault(r *httprequest.Request) time.Time {
	if r.State != nil && r.State.CheckoutAttemptService != nil {
		if c := r.State.CheckoutAttemptService.Clock(); c != nil {
			return c.Now()
		}
	}
	return time.Now()
}

// solanaTierChange is change-tier on the Solana rail: one atomic on-chain
// transaction the customer's wallet signs (cancel the old subscription,
// subscribe to the new plan, and for an upgrade the prorated pull the merchant
// co-signed). Without a signature it answers requires_action with the
// transaction; with the signature of the landed transaction it mirrors the
// switch and answers succeeded with the new subscription. Nothing is mirrored
// before the chain confirms it.
func solanaTierChange(r *httprequest.Request, subscriptionID uuid.UUID, priceID, signature string) {
	if r.State.SolanaPrepareTierChangeService == nil || r.State.SolanaRPCResolver == nil || r.State.SubscriptionLifecycleService == nil || r.State.DB == nil {
		r.ErrorCode(billing.CodeServiceUnavailable, "Solana recurring billing is not configured")
		return
	}
	resolved, status, msg := resolveSolanaTierChange(r, subscriptionID, priceID)
	if status != 0 {
		r.ErrorJSON(status, msg)
		return
	}
	ctx := r.Request.Context()
	merchantID, err := merchant.Require(ctx)
	if err != nil {
		r.ErrorCode(billing.CodeInternalError, "no merchant resolved on request")
		return
	}
	// Confirm derives the new subscription account from the same canonical
	// terms rather than trusting one the client names.
	prep, err := r.State.SolanaPrepareTierChangeService.Prepare(ctx, recurring.PrepareTierChangeInput{
		MerchantID:           merchantID,
		SubscriberWallet:     resolved.oldRow.SubscriberWallet,
		MintSymbol:           resolved.newTerms.mintSymbol,
		OldPlanPDA:           resolved.oldRow.PlanPDA,
		OldSubscriptionPDA:   resolved.oldRow.SubscriptionPDA,
		NewPlanID:            resolved.newTerms.planID,
		NewAmountBaseUnits:   resolved.newTerms.amount,
		NewPeriodHours:       resolved.newTerms.period,
		NewPlanCreatedAt:     resolved.newTerms.createdAt,
		IsUpgrade:            resolved.isUpgrade,
		FirstChargeBaseUnits: resolved.firstChargeBaseUnits,
	})
	if err != nil {
		r.ErrorJSON(solanaClientError(err, http.StatusBadRequest))
		return
	}

	out := billing.TierChange{
		Action:           "downgrade",
		Effective:        "period_end",
		PriceID:          billing.PriceID(resolved.newPrice.ID),
		Rail:             string(models.RailSolana),
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
		out.Action, out.Effective, out.AmountDueNow = "upgrade", "now", resolved.firstChargeMicros
		next := nowOrDefault(r).Add(time.Duration(periodHours) * time.Hour).UTC()
		out.NextChargeDate = &next
	}
	if signature == "" {
		old := billing.SubscriptionID(subscriptionID)
		out.Status, out.SubscriptionID = "requires_action", &old
		out.NextAction = &billing.NextAction{Type: "solana_sign_transactions", Transactions: []string{prep.Transaction}}
		r.SuccessJSON(out)
		return
	}

	var email string
	if user := r.GetUser(); user != nil && user.Email != nil {
		email = *user.Email
	}
	// The network and token set come from the merchant's armed Solana PSP.
	network := ""
	var tokens map[string]config.TokenConfig
	if r.State.RailConfigs != nil {
		if proc, cerr := r.State.RailConfigs.RailConfig(ctx, string(models.RailSolana), ""); cerr == nil && proc.Solana != nil {
			network = proc.Solana.Network
			tokens = proc.Solana.Tokens
		}
	}
	svc := recurring.NewConfirmTierChangeService(
		r.State.SolanaRPCResolver.ChainReader(),
		r.State.SubscriptionLifecycleService,
		solanasubs.NewSolanaSubscriptionRepo(r.State.DB),
		r.State.DB,
		network,
		tokens,
	)
	result, err := svc.Confirm(ctx, recurring.ConfirmTierChangeInput{
		Signature:          signature,
		OldSubscriptionID:  subscriptionID,
		UserID:             resolved.oldSub.CustomerID.String(),
		CustomerEmail:      email,
		NewPriceID:         resolved.newPrice.ID,
		NewSubscriptionPDA: prep.NewSubscriptionPDA,
		NewPlanID:          resolved.newTerms.planID,
		NewMintSymbol:      resolved.newTerms.mintSymbol,
		NewAmountBaseUnits: resolved.newTerms.amount,
		NewPeriodHours:     resolved.newTerms.period,
		NewPlanCreatedAt:   resolved.newTerms.createdAt,
		NewFiatAmount:      resolved.newPrice.Amount,
		NewCurrency:        resolved.newPrice.Currency,
		IsUpgrade:          resolved.isUpgrade,
		// The confirm re-quotes the proration and cannot reproduce the amount
		// the prepare quoted, so the landed, merchant-co-signed pull is the
		// charge.
		OldPeriodEndsAt: resolved.oldSub.CurrentPeriodEndsAt,
	})
	if err != nil {
		r.ErrorJSON(solanaClientError(err, http.StatusBadRequest))
		return
	}
	next := billing.SubscriptionID(result.NewSubscription.ID)
	out.Status, out.SubscriptionID = "succeeded", &next
	r.SuccessJSON(out)
}
