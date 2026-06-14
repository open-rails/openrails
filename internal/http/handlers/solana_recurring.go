package handlers

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/db/repo"
	dbrepo "github.com/open-rails/openrails/internal/db/repo"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/checkout"
	solanamodule "github.com/open-rails/openrails/internal/modules/solana"
	"github.com/open-rails/openrails/internal/modules/solana/recurring"
	"github.com/open-rails/openrails/pkg/api"
	"github.com/open-rails/openrails/pkg/merchant"
)

// publishSolanaPlanRequest is the admin body for publishing an on-chain recurring
// plan (#254). The plan terms are immutable on-chain, so this is a create-only
// surface; editing core terms means sunsetting the plan and publishing a new one.
type publishSolanaPlanRequest struct {
	PlanID          uint64 `json:"plan_id" binding:"required"`
	TokenSymbol     string `json:"token_symbol" binding:"required"`
	AmountBaseUnits uint64 `json:"amount_base_units" binding:"required"`
	PeriodHours     uint64 `json:"period_hours" binding:"required"`
	ReceivingWallet string `json:"receiving_wallet"`
	MetadataURI     string `json:"metadata_uri"`
	EndTs           int64  `json:"end_ts"`
	// PriceID, when set, attaches the published plan to that price's Solana
	// processor config so the enroll flow can read the canonical terms back.
	PriceID string `json:"price_id"`
}

// AdminPublishSolanaPlan signs + submits create_plan from the tenant's merchant
// (cranker) key and returns the durable plan handle (#254). Admin-gated.
func AdminPublishSolanaPlan(r *httprequest.Request) {
	svc := r.State.SolanaPlanService
	if svc == nil {
		r.ErrorJSON(http.StatusServiceUnavailable, "Solana recurring billing is not configured")
		return
	}
	var req publishSolanaPlanRequest
	if !r.BindJSON(&req) {
		return
	}

	tenantID, err := merchant.Require(r.Request.Context())
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "no tenant resolved on request")
		return
	}
	handle, err := svc.PublishPlan(r.Request.Context(), recurring.PublishPlanInput{
		MerchantID:      tenantID,
		PlanID:          req.PlanID,
		TokenSymbol:     req.TokenSymbol,
		AmountBaseUnits: req.AmountBaseUnits,
		PeriodHours:     req.PeriodHours,
		ReceivingWallet: req.ReceivingWallet,
		MetadataURI:     req.MetadataURI,
		EndTs:           req.EndTs,
	})
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}

	// Optionally bind the plan to a price so enroll can read canonical terms.
	if req.PriceID != "" && r.State.PriceService != nil {
		priceID, perr := uuid.Parse(req.PriceID)
		if perr != nil {
			r.ErrorJSON(http.StatusBadRequest, "invalid price_id")
			return
		}
		price, gerr := r.State.PriceService.GetByID(r.Request.Context(), priceID)
		if gerr != nil || price == nil {
			r.ErrorJSON(http.StatusNotFound, "price not found")
			return
		}
		price.SetProcessorConfig(models.ProcessorSolana, handle.ToProcessorConfig())
		if uerr := r.State.PriceService.UpdateProcessors(r.Request.Context(), priceID, price.Processors); uerr != nil {
			r.ErrorJSON(http.StatusInternalServerError, "failed to attach plan to price")
			return
		}
	}

	r.SuccessJSON(handle)
}

// confirmSolanaEnrollmentRequest is the user body for activating a recurring
// subscription after the wallet has signed init_subscription_authority +
// subscribe (#255). The financial terms are NOT taken from the client — they are
// read server-side from the price's published plan config.
type confirmSolanaEnrollmentRequest struct {
	PriceID          string `json:"price_id" binding:"required"`
	SubscriberWallet string `json:"subscriber_wallet" binding:"required"`
	// Signature is the confirmed atomic subscribe-bundle tx signature the wallet
	// submitted (#286). Optional; recorded on the membership/row when present.
	Signature string `json:"signature,omitempty"`
}

// ConfirmSolanaEnrollment verifies the on-chain subscription, charges the first
// cycle, and creates the local membership (#255). User-authenticated.
func ConfirmSolanaEnrollment(r *httprequest.Request) {
	svc := r.State.SolanaEnrollService
	if svc == nil {
		r.ErrorJSON(http.StatusServiceUnavailable, "Solana recurring billing is not configured")
		return
	}
	user := r.GetUser()
	if user == nil || user.ID == "" {
		r.ErrorJSON(http.StatusUnauthorized, "authentication required")
		return
	}
	var req confirmSolanaEnrollmentRequest
	if !r.BindJSON(&req) {
		return
	}
	priceID, err := uuid.Parse(req.PriceID)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid price_id")
		return
	}
	if r.State.PriceService == nil {
		r.ErrorJSON(http.StatusServiceUnavailable, "catalog is not configured")
		return
	}
	price, err := r.State.PriceService.GetByID(r.Request.Context(), priceID)
	if err != nil || price == nil {
		r.ErrorJSON(http.StatusNotFound, "price not found")
		return
	}

	// Read the canonical, immutable plan terms from the price's Solana config —
	// these were stamped at publish time and must not be client-supplied.
	cfg := price.GetProcessorConfig(models.ProcessorSolana)
	if cfg == nil {
		r.ErrorJSON(http.StatusBadRequest, "price is not configured for Solana recurring billing")
		return
	}
	planID, perr := strconv.ParseUint(cfg["plan_id"], 10, 64)
	amount, aerr := strconv.ParseUint(cfg["amount_base_units"], 10, 64)
	period, herr := strconv.ParseUint(cfg["period_hours"], 10, 64)
	createdAt, _ := strconv.ParseInt(cfg["created_at"], 10, 64)
	if perr != nil || aerr != nil || herr != nil {
		r.ErrorJSON(http.StatusInternalServerError, "price Solana plan config is malformed")
		return
	}

	var email string
	if user.Email != nil {
		email = *user.Email
	}
	tenantID, err := merchant.Require(r.Request.Context())
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "no tenant resolved on request")
		return
	}
	sub, err := svc.ConfirmEnrollment(r.Request.Context(), recurring.EnrollInput{
		MerchantID:       tenantID,
		UserID:           user.ID,
		UserEmail:        email,
		PriceID:          priceID,
		SubscriberWallet: req.SubscriberWallet,
		PlanID:           planID,
		MintSymbol:       cfg["mint_symbol"],
		AmountBaseUnits:  amount,
		PeriodHours:      period,
		PlanCreatedAt:    createdAt,
		FiatAmount:       price.Amount,
		Currency:         price.Currency,
		Signature:        strings.TrimSpace(req.Signature),
	})
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	r.SuccessJSON(sub)
}

// PrepareSolanaCancelTx builds the UNSIGNED on-chain cancel_subscription
// transaction the subscriber's wallet signs to TRUSTLESSLY revoke a recurring
// Solana subscription (#266/#271). This is the PREPARE step of the on-chain
// cancel loop: prepare -> wallet signs+sends -> confirm (ConfirmSolanaCancel) ->
// mirror. Solana is the source of truth; OpenRails never DB-only "soft cancels" —
// it only mirrors after observing the confirmed on-chain cancel. The caller must
// own the subscription; the response is `{ "transaction": "<base64>",
// "subscription_pda": "<pda>" }` for the wallet to deserialize, sign, and send.
func PrepareSolanaCancelTx(r *httprequest.Request) {
	svc := r.State.SolanaPrepareCancelService
	if svc == nil {
		r.ErrorJSON(http.StatusServiceUnavailable, "Solana recurring billing is not configured")
		return
	}

	uc, ok := r.UserContext()
	if !ok || uc.UserID == "" {
		r.ErrorJSON(http.StatusUnauthorized, "User authentication required")
		return
	}

	subscriptionIDStr := r.Param("id")
	if subscriptionIDStr == "" {
		r.ErrorJSON(http.StatusBadRequest, "subscription ID required")
		return
	}
	subscriptionID, err := api.ParseSubscriptionID(subscriptionIDStr)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "Invalid subscription ID format")
		return
	}

	// Authorize: the acting user must own the lifecycle subscription before we
	// reveal its on-chain identifiers.
	if r.State.SubscriptionService == nil {
		r.ErrorJSON(http.StatusServiceUnavailable, "subscriptions are not configured")
		return
	}
	sub, err := r.State.SubscriptionService.GetByID(r.Request.Context(), subscriptionID)
	if err != nil {
		if repo.IsNotFound(err) {
			r.ErrorJSON(http.StatusNotFound, "subscription not found")
			return
		}
		r.ErrorJSON(http.StatusInternalServerError, "failed to retrieve subscription")
		return
	}
	if sub.CustomerID.String() != uc.UserID {
		r.ErrorJSON(http.StatusNotFound, "subscription not found")
		return
	}

	res, err := svc.Prepare(r.Request.Context(), subscriptionID)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}

	r.SuccessJSON(map[string]any{
		"transaction":      res.Transaction,
		"subscription_pda": res.SubscriptionPDA,
	})
}

// confirmSolanaCancelRequest carries the signature of the cancel_subscription
// transaction the wallet signed + sent (#271). OpenRails confirms it landed
// on-chain before mirroring the cancel into the DB.
type confirmSolanaCancelRequest struct {
	Signature string `json:"signature" binding:"required"`
}

// ConfirmSolanaCancel is the CONFIRM step of the on-chain cancel loop (#271):
// after the wallet signs + sends the unsigned tx from PrepareSolanaCancelTx, it
// posts the resulting signature here. OpenRails verifies the cancel LANDED and
// SUCCEEDED on-chain (Solana is the source of truth) and only then MIRRORS it by
// cancelling the membership immediately — the lifecycle cascade flips the linked
// solana_subscriptions row to cancelled so the cranker stops. There is no DB-only
// "soft cancel": a signature that never confirms or reverted does NOT cancel. The
// acting user must own the subscription.
func ConfirmSolanaCancel(r *httprequest.Request) {
	if r.State.SolanaRPC == nil {
		r.ErrorJSON(http.StatusServiceUnavailable, "Solana recurring billing is not configured")
		return
	}
	if r.State.SubscriptionLifecycleService == nil {
		r.ErrorJSON(http.StatusServiceUnavailable, "subscriptions are not configured")
		return
	}

	uc, ok := r.UserContext()
	if !ok || uc.UserID == "" {
		r.ErrorJSON(http.StatusUnauthorized, "User authentication required")
		return
	}

	subscriptionIDStr := r.Param("id")
	if subscriptionIDStr == "" {
		r.ErrorJSON(http.StatusBadRequest, "subscription ID required")
		return
	}
	subscriptionID, err := api.ParseSubscriptionID(subscriptionIDStr)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "Invalid subscription ID format")
		return
	}

	var req confirmSolanaCancelRequest
	if !r.BindJSON(&req) {
		return
	}

	// Authorize: the acting user must own the lifecycle subscription before we
	// confirm + mirror a cancel against it.
	if r.State.SubscriptionService == nil {
		r.ErrorJSON(http.StatusServiceUnavailable, "subscriptions are not configured")
		return
	}
	sub, err := r.State.SubscriptionService.GetByID(r.Request.Context(), subscriptionID)
	if err != nil {
		if repo.IsNotFound(err) {
			r.ErrorJSON(http.StatusNotFound, "subscription not found")
			return
		}
		r.ErrorJSON(http.StatusInternalServerError, "failed to retrieve subscription")
		return
	}
	if sub.CustomerID.String() != uc.UserID {
		r.ErrorJSON(http.StatusNotFound, "subscription not found")
		return
	}

	svc := recurring.NewConfirmCancelService(r.State.SolanaRPC, r.State.SubscriptionLifecycleService)
	if err := svc.Confirm(r.Request.Context(), subscriptionID, req.Signature); err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}

	r.SuccessJSON(map[string]any{
		"subscription_id": subscriptionID.String(),
		"status":          "cancelled",
	})
}

// solanaTierChangeRequest is the body for the prepare endpoint: the target price
// to change TO. The acting user must own the path subscription.
type solanaTierChangeRequest struct {
	NewPriceID string `json:"new_price_id" binding:"required"`
}

// solanaTierChangeConfirmRequest is the body for the confirm endpoint: the
// signature of the atomic tier-change tx the wallet signed + sent, plus the same
// target price (so confirm resolves the identical canonical terms as prepare).
type solanaTierChangeConfirmRequest struct {
	Signature  string `json:"signature" binding:"required"`
	NewPriceID string `json:"new_price_id" binding:"required"`
}

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
	// firstChargeBaseUnits is the Model-B prorated first pull (token base units)
	// for an upgrade; 0 for a downgrade.
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
		if repo.IsNotFound(err) {
			return nil, http.StatusNotFound, "subscription not found"
		}
		return nil, http.StatusInternalServerError, "failed to retrieve subscription"
	}
	if oldSub.CustomerID.String() != uc.UserID {
		return nil, http.StatusNotFound, "subscription not found"
	}
	if oldSub.Processor != models.ProcessorSolana {
		return nil, http.StatusBadRequest, "subscription is not a Solana subscription"
	}

	// Load the OLD on-chain row (subscriber/merchant identifiers for the atomic tx).
	oldRow, err := dbrepo.NewSolanaSubscriptionRepo(r.State.DB).GetBySubscriptionID(r.Request.Context(), subscriptionID)
	if err != nil || oldRow == nil {
		return nil, http.StatusBadRequest, "no on-chain record for this subscription"
	}

	// Resolve the NEW price + its published plan terms.
	newPriceID, err := api.ParsePriceID(newPriceIDStr)
	if err != nil {
		return nil, http.StatusBadRequest, "invalid new_price_id"
	}
	newPrice, err := r.State.PriceService.GetByID(r.Request.Context(), newPriceID)
	if err != nil || newPrice == nil {
		return nil, http.StatusNotFound, "target price not found"
	}
	if !newPrice.IsPurchasable() {
		return nil, http.StatusBadRequest, "target price is not available"
	}
	newCfg := newPrice.GetProcessorConfig(models.ProcessorSolana)
	newTerms, ok := parseResolvedPlanTerms(newCfg)
	if !ok {
		return nil, http.StatusBadRequest, "target price is not configured for Solana recurring billing"
	}

	// Load OLD + NEW products to decide direction and enforce the tier group.
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
	if oldProduct.ID == newProduct.ID {
		return nil, http.StatusBadRequest, "already subscribed to this product"
	}
	if oldProduct.TierGroup != nil && newProduct.TierGroup != nil &&
		strings.TrimSpace(*oldProduct.TierGroup) != strings.TrimSpace(*newProduct.TierGroup) {
		return nil, http.StatusBadRequest, "tier change must stay within the same tier group"
	}

	// Direction: higher TierRank => upgrade (charge prorated now), lower =>
	// downgrade (deferred, no charge) — same rule as CheckoutService.TierChange.
	isUpgrade := newProduct.TierRank >= oldProduct.TierRank

	out := &resolvedTierChange{
		oldSub:    oldSub,
		oldRow:    oldRow,
		newPrice:  newPrice,
		newTerms:  newTerms,
		isUpgrade: isUpgrade,
	}

	if isUpgrade {
		// Model-B prorated first charge (new_full - old_unused) in cents, then
		// cents -> stablecoin base units at the $1 peg (depeg failsafe inside).
		firstChargeCents, _ := checkout.CalculateModelBUpgradeCharge(
			oldPrice.Amount,
			newPrice.Amount,
			oldSub.CurrentPeriodEndsAt,
			newPrice.BillingCycleDays,
			nowOrDefault(r),
		)
		firstChargeBaseUnits := solanamodule.FiatCentsToStablecoinBaseUnits(
			r.Request.Context(), firstChargeCents, newTerms.mintSymbol, r.State.SolanaPriceProvider,
		)
		// A genuine upgrade can round to 0 base units only when the unused old credit
		// fully covers the new price; we still need a non-zero pull to activate the
		// new on-chain subscription, so charge the smallest unit (a fraction of a
		// cent) in that degenerate case.
		if firstChargeBaseUnits == 0 {
			firstChargeBaseUnits = 1
		}
		out.firstChargeBaseUnits = firstChargeBaseUnits
	}

	return out, 0, ""
}

// parseResolvedPlanTerms parses a price's Solana processor config into the
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

// nowOrDefault reads the clock the checkout session service uses (so tests can
// pin time), falling back to wall-clock.
func nowOrDefault(r *httprequest.Request) time.Time {
	if r.State != nil && r.State.CheckoutSessionService != nil {
		if c := r.State.CheckoutSessionService.Clock(); c != nil {
			return c.Now()
		}
	}
	return time.Now()
}

// PrepareSolanaTierChange is the PREPARE step of the on-chain tier-change loop
// (#272). It authorizes ownership, resolves the OLD on-chain identifiers + the
// NEW price's canonical plan terms, decides upgrade vs downgrade, computes the
// Model-B prorated first charge for an upgrade, and returns the SINGLE ATOMIC
// transaction for the wallet to sign + send:
//
//	{ "transaction": "<base64>", "kind": "upgrade|downgrade",
//	  "new_subscription_pda": "<pda>" }
//
// For an upgrade the tx is PARTIALLY signed (the cranker co-signed the prorated
// transfer slot); for a downgrade it is fully UNSIGNED. After sending, the wallet
// posts the signature to the confirm endpoint, which mirrors the switch into the
// DB. Solana is the source of truth — nothing is mirrored until confirm.
func PrepareSolanaTierChange(r *httprequest.Request) {
	svc := r.State.SolanaPrepareTierChangeService
	if svc == nil {
		r.ErrorJSON(http.StatusServiceUnavailable, "Solana recurring billing is not configured")
		return
	}
	subscriptionID, ok := parseSubscriptionIDParam(r)
	if !ok {
		return
	}
	var req solanaTierChangeRequest
	if !r.BindJSON(&req) {
		return
	}

	resolved, status, msg := resolveSolanaTierChange(r, subscriptionID, req.NewPriceID)
	if status != 0 {
		r.ErrorJSON(status, msg)
		return
	}

	tenantID, err := merchant.Require(r.Request.Context())
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "no tenant resolved on request")
		return
	}
	res, err := svc.Prepare(r.Request.Context(), recurring.PrepareTierChangeInput{
		MerchantID:           tenantID,
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
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}

	r.SuccessJSON(map[string]any{
		"transaction":          res.Transaction,
		"kind":                 res.Kind,
		"new_subscription_pda": res.NewSubscriptionPDA,
	})
}

// ConfirmSolanaTierChange is the CONFIRM step of the on-chain tier-change loop
// (#272). After the wallet signs + sends the atomic tx from
// PrepareSolanaTierChange, it posts the resulting signature here. OpenRails
// confirms the tx LANDED + SUCCEEDED on-chain (the source of truth) and only then
// MIRRORS the switch into the DB: cancel the OLD membership + on-chain row, create
// the NEW membership + on-chain row, and set the new row's next_pull_at per kind
// (upgrade => now + new period; downgrade => the old period end). Idempotent: a
// re-confirm after a committed mirror returns the existing new subscription.
func ConfirmSolanaTierChange(r *httprequest.Request) {
	if r.State.SolanaRPC == nil || r.State.SubscriptionLifecycleService == nil || r.State.DB == nil {
		r.ErrorJSON(http.StatusServiceUnavailable, "Solana recurring billing is not configured")
		return
	}
	subscriptionID, ok := parseSubscriptionIDParam(r)
	if !ok {
		return
	}
	var req solanaTierChangeConfirmRequest
	if !r.BindJSON(&req) {
		return
	}

	resolved, status, msg := resolveSolanaTierChange(r, subscriptionID, req.NewPriceID)
	if status != 0 {
		r.ErrorJSON(status, msg)
		return
	}

	// Re-derive the NEW subscription PDA the same way prepare did, so confirm does
	// not trust a client-supplied PDA. PrepareTierChangeService returns it, but the
	// confirm body only carries the signature + price; deriving it server-side from
	// the canonical terms keeps the mirror authoritative.
	tenantID, err := merchant.Require(r.Request.Context())
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "no tenant resolved on request")
		return
	}
	prep, err := r.State.SolanaPrepareTierChangeService.Prepare(r.Request.Context(), recurring.PrepareTierChangeInput{
		MerchantID:           tenantID,
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
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}

	var email string
	user := r.GetUser()
	if user != nil && user.Email != nil {
		email = *user.Email
	}
	network := ""
	var tokens map[string]config.SolanaToken
	if pc := r.State.Config.GetSolanaProcessor(); pc != nil {
		network = pc.Network
		tokens = pc.Tokens
	}

	svc := recurring.NewConfirmTierChangeService(
		r.State.SolanaRPC,
		r.State.SubscriptionLifecycleService,
		dbrepo.NewSolanaSubscriptionRepo(r.State.DB),
		network,
		tokens,
	)
	result, err := svc.Confirm(r.Request.Context(), recurring.ConfirmTierChangeInput{
		Signature:            req.Signature,
		OldSubscriptionID:    subscriptionID,
		UserID:               resolved.oldSub.CustomerID.String(),
		UserEmail:            email,
		NewPriceID:           resolved.newPrice.ID,
		NewSubscriptionPDA:   prep.NewSubscriptionPDA,
		NewPlanID:            resolved.newTerms.planID,
		NewMintSymbol:        resolved.newTerms.mintSymbol,
		NewAmountBaseUnits:   resolved.newTerms.amount,
		NewPeriodHours:       resolved.newTerms.period,
		NewPlanCreatedAt:     resolved.newTerms.createdAt,
		NewFiatAmount:        resolved.newPrice.Amount,
		NewCurrency:          resolved.newPrice.Currency,
		IsUpgrade:            resolved.isUpgrade,
		FirstChargeBaseUnits: resolved.firstChargeBaseUnits,
		OldPeriodEndsAt:      resolved.oldSub.CurrentPeriodEndsAt,
	})
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}

	kind := "downgrade"
	if resolved.isUpgrade {
		kind = "upgrade"
	}
	r.SuccessJSON(map[string]any{
		"subscription_id":     result.NewSubscription.ID.String(),
		"new_subscription_id": result.NewSubscription.ID.String(),
		"kind":                kind,
		"status":              "active",
		"already_confirmed":   result.AlreadyConfirmed,
	})
}

// parseSubscriptionIDParam reads + validates the :id path param, writing the
// error response on failure.
func parseSubscriptionIDParam(r *httprequest.Request) (uuid.UUID, bool) {
	idStr := r.Param("id")
	if idStr == "" {
		r.ErrorJSON(http.StatusBadRequest, "subscription ID required")
		return uuid.Nil, false
	}
	id, err := api.ParseSubscriptionID(idStr)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "Invalid subscription ID format")
		return uuid.Nil, false
	}
	return id, true
}
