package handlers

import (
	"net/http"
	"strings"

	authgin "github.com/PaulFidika/authkit/adapters/gin"
	"github.com/doujins-org/doujins-billing/internal/db/models"
	"github.com/doujins-org/doujins-billing/internal/services"
	"github.com/doujins-org/doujins-billing/pkg/api"
)

type changeSubscriptionRequest struct {
	PriceID string `json:"price_id" binding:"required"`
}

type changeSubscriptionResponse struct {
	Status         string  `json:"status"`
	Action         string  `json:"action"`
	Message        string  `json:"message,omitempty"`
	SubscriptionID *string `json:"subscription_id,omitempty"`
}

// ChangeSubscription upgrades/downgrades the active Stripe subscription.
// POST /v1/me/subscriptions/change
func ChangeSubscription(r *Request) {
	var req changeSubscriptionRequest
	if !r.BindJSON(&req) {
		return
	}
	cl, ok := authgin.ClaimsFromGin(r.GinCtx)
	if !ok || cl.UserID == "" {
		r.ErrorJSON(http.StatusUnauthorized, "User authentication required")
		return
	}
	if r.State == nil || r.State.SubscriptionService == nil || r.State.PriceService == nil || r.State.ProductService == nil {
		r.ErrorJSON(http.StatusInternalServerError, "subscription service unavailable")
		return
	}

	priceID, err := api.ParsePriceID(req.PriceID)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid price_id")
		return
	}
	newPrice, err := r.State.PriceService.GetByID(r.Request.Context(), priceID)
	if err != nil {
		r.ErrorJSON(http.StatusNotFound, "price not found")
		return
	}
	newProduct, err := r.State.ProductService.GetByID(r.Request.Context(), newPrice.ProductID)
	if err != nil {
		r.ErrorJSON(http.StatusNotFound, "product not found")
		return
	}

	sub, err := r.State.SubscriptionService.GetActiveSubscription(r.Request.Context(), cl.UserID)
	if err != nil {
		r.ErrorJSON(http.StatusNotFound, "active subscription not found")
		return
	}
	if sub.Processor != models.ProcessorStripe {
		r.ErrorJSON(http.StatusBadRequest, "unsupported processor for plan change")
		return
	}
	currentPrice, err := r.State.PriceService.GetByID(r.Request.Context(), sub.PriceID)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "current price not found")
		return
	}
	currentProduct, err := r.State.ProductService.GetByID(r.Request.Context(), currentPrice.ProductID)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "current product not found")
		return
	}
	if currentProduct.ID == newProduct.ID {
		r.ErrorJSON(http.StatusConflict, "already on this plan")
		return
	}
	if currentProduct.TierGroup != nil && newProduct.TierGroup != nil {
		if strings.TrimSpace(*currentProduct.TierGroup) != strings.TrimSpace(*newProduct.TierGroup) {
			r.ErrorJSON(http.StatusBadRequest, "cannot change to a different tier group")
			return
		}
	}

	stripePriceID, ok := newPrice.GetStripeConfig()
	if !ok || strings.TrimSpace(stripePriceID) == "" {
		r.ErrorJSON(http.StatusBadRequest, "stripe price not configured")
		return
	}
	if strings.TrimSpace(sub.ProcessorSubscriptionID) == "" {
		r.ErrorJSON(http.StatusBadRequest, "stripe subscription id missing")
		return
	}

	action := "upgrade"
	proration := "create_prorations"
	billingAnchor := "now"
	if newProduct.TierRank < currentProduct.TierRank {
		action = "downgrade"
		proration = "none"
		billingAnchor = "unchanged"
	}

	stripeService := &services.StripeSubscriptionService{Config: r.State.Config}
	itemID, err := stripeService.GetSubscriptionItemID(r.Request.Context(), sub.ProcessorSubscriptionID)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	if err := stripeService.UpdateSubscriptionPrice(r.Request.Context(), sub.ProcessorSubscriptionID, itemID, stripePriceID, proration, billingAnchor); err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}

	sub.PriceID = newPrice.ID
	sub.ProductID = newPrice.ProductID
	sub.ScheduledPriceID = nil
	if err := r.State.SubscriptionService.Update(r.Request.Context(), sub); err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to update subscription")
		return
	}

	subID := api.FormatSubscriptionID(sub.ID)
	msg := "Plan updated"
	if action == "downgrade" {
		msg = "Plan downgraded"
	}
	r.SuccessJSON(changeSubscriptionResponse{
		Status:         "success",
		Action:         action,
		Message:        msg,
		SubscriptionID: &subID,
	})
}
