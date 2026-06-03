package handlers

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/models"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/solana/recurring"
	"github.com/open-rails/openrails/pkg/api"
	"github.com/open-rails/openrails/pkg/authprovider"
	"github.com/open-rails/openrails/pkg/tenant"
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

	tenantID := tenant.FromContextOrDefault(r.Request.Context())
	handle, err := svc.PublishPlan(r.Request.Context(), recurring.PublishPlanInput{
		TenantID:        tenantID,
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
	tenantID := tenant.FromContextOrDefault(r.Request.Context())
	sub, err := svc.ConfirmEnrollment(r.Request.Context(), recurring.EnrollInput{
		TenantID:         tenantID,
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

	uc, ok := authprovider.UserContextFromGin(r.GinCtx)
	if !ok || uc.UserID == "" {
		r.ErrorJSON(http.StatusUnauthorized, "User authentication required")
		return
	}

	subscriptionIDStr := r.GinCtx.Param("id")
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
		if errors.Is(err, sql.ErrNoRows) {
			r.ErrorJSON(http.StatusNotFound, "subscription not found")
			return
		}
		r.ErrorJSON(http.StatusInternalServerError, "failed to retrieve subscription")
		return
	}
	if sub.UserID != uc.UserID {
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

	uc, ok := authprovider.UserContextFromGin(r.GinCtx)
	if !ok || uc.UserID == "" {
		r.ErrorJSON(http.StatusUnauthorized, "User authentication required")
		return
	}

	subscriptionIDStr := r.GinCtx.Param("id")
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
		if errors.Is(err, sql.ErrNoRows) {
			r.ErrorJSON(http.StatusNotFound, "subscription not found")
			return
		}
		r.ErrorJSON(http.StatusInternalServerError, "failed to retrieve subscription")
		return
	}
	if sub.UserID != uc.UserID {
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
