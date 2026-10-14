package payment

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// SubscriptionChangeOptions lists the prices the customer's subscription can
// change to now: the other products of its tier group, each with the change's
// preview. A target the change would refuse is left out, so a page never
// re-derives the tier rules; a subscription that cannot change at all answers
// why.
func (s *PurchaseService) SubscriptionChangeOptions(ctx context.Context, subscriptionID uuid.UUID, user *UserIdentity) ([]billing.SubscriptionChangeOption, error) {
	if s.SubscriptionService == nil || s.ProductService == nil || s.PriceService == nil {
		return nil, errors.New("subscription change services unavailable")
	}
	sub, err := s.SubscriptionService.GetByID(ctx, subscriptionID)
	if payer := identity.CustomerIDFromString(user.ID); err != nil || payer.IsZero() || sub.CustomerID != payer.UUID() {
		return nil, &TierChangeError{Code: codeSubscriptionNotFound, Message: "subscription not found"}
	}
	if err := validateTierChangeSubscriptionStatus(sub); err != nil {
		return nil, err
	}
	if err := s.refuseTierChangeInFlight(ctx, sub); err != nil {
		return nil, err
	}
	current, err := s.ProductService.GetByID(ctx, sub.ProductID)
	if err != nil {
		return nil, err
	}
	if current.TierGroup == nil || strings.TrimSpace(*current.TierGroup) == "" {
		return []billing.SubscriptionChangeOption{}, nil
	}
	live := false
	products, err := s.ProductService.List(ctx, catalog.ProductFilter{TierGroup: *current.TierGroup, Archived: &live}, billing.PageRequest{Limit: 100})
	if err != nil {
		return nil, err
	}
	others := make([]uuid.UUID, 0, len(products.Items))
	for _, product := range products.Items {
		if product.ID != current.ID {
			others = append(others, product.ID)
		}
	}
	prices, err := s.PriceService.CurrentByProducts(ctx, others)
	if err != nil {
		return nil, err
	}
	out := []billing.SubscriptionChangeOption{}
	for _, product := range products.Items {
		for _, price := range prices[product.ID] {
			preview, err := s.PreviewSubscriptionChange(ctx, &SubscriptionChangeRequest{SubscriptionID: sub.ID, PriceID: price.PublicID.String()}, user)
			if isChangeRefusal(err) {
				continue
			}
			if err != nil {
				return nil, err
			}
			out = append(out, billing.SubscriptionChangeOption{Product: *product.Summary(), Price: *subscriptions.BuyerPrice(price), Preview: *preview})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Product.TierRank != out[j].Product.TierRank {
			return out[i].Product.TierRank < out[j].Product.TierRank
		}
		return out[i].Price.UnitAmount < out[j].Price.UnitAmount
	})
	return out, nil
}

// isChangeRefusal reports whether err refuses one change target rather than
// failing the request.
func isChangeRefusal(err error) bool {
	if err == nil {
		return false
	}
	var coded *TierChangeError
	var inFlight *TierChangeInFlightError
	var refusal *apperr.Error
	switch {
	case errors.As(err, &coded), errors.As(err, &inFlight):
		return true
	case errors.Is(err, ErrTierChangePending), errors.Is(err, ErrTierChangeSameProduct),
		errors.Is(err, ErrTierChangeDifferentGroup), errors.Is(err, subscriptions.ErrPriceCurrencyMismatch):
		return true
	case errors.As(err, &refusal):
		return refusal.Status < 500
	}
	return false
}
