package payment

import (
	"context"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/orders"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// A PSP declaring fallback_for stands in for the PSPs it names: an order
// offers it only while none of them can take the order (unarmed, disarmed,
// archived, or not selling its lines), or once the order has
// fallback_after_failures failed attempts through them. Every other PSP is a
// peer, offered whenever it takes the order.

// ErrFallbackWithheld refuses paying through a fallback PSP whose primaries
// still take the order.
var ErrFallbackWithheld = apperr.New(http.StatusConflict, billing.CodePaymentOptionWithheld,
	"This PSP is a fallback: it takes the order once the order's primary PSPs are unavailable or have failed on it.")

// withFallbacks marks each fallback option, and withholds the ones whose
// primaries still take the order.
func (s *OrderAttemptService) withFallbacks(ctx context.Context, order *orders.Order, options []OrderOption) ([]OrderOption, error) {
	purchase, ok := s.purchaseService.(*PurchaseService)
	if !ok {
		return options, nil
	}
	keys := make(map[string]uuid.UUID, len(options))
	fallbacks := make([]*config.PSPFallback, len(options))
	for i, o := range options {
		target, err := purchase.resolveRailTargetForPSP(ctx, o.Rail, o.PSPID)
		if err != nil {
			return nil, err
		}
		if target.Scope == nil {
			continue
		}
		keys[strings.ToLower(target.Scope.Key)] = o.PSPID
		fallback, declared, err := config.ReadPSPFallback(target.Scope.Settings)
		if err != nil {
			return nil, err
		}
		if declared {
			fallbacks[i] = &fallback
		}
	}
	for i, fallback := range fallbacks {
		if fallback == nil {
			continue
		}
		options[i].Fallback = true
		var primaries []uuid.UUID
		for _, key := range fallback.For {
			if id, ok := keys[key]; ok {
				primaries = append(primaries, id)
			}
		}
		if len(primaries) == 0 {
			continue
		}
		failed, err := s.failedThrough(ctx, order, primaries)
		if err != nil {
			return nil, err
		}
		options[i].Withheld = failed < fallback.AfterFailures
	}
	return options, nil
}

// failedThrough counts the order's failed attempts through psps; an order
// not yet created has none.
func (s *OrderAttemptService) failedThrough(ctx context.Context, order *orders.Order, psps []uuid.UUID) (int, error) {
	if order == nil {
		return 0, nil
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return 0, err
	}
	n, err := s.db.Gen(ctx).CountOrderFailedAttempts(ctx, gen.CountOrderFailedAttemptsParams{MerchantID: mid.UUID(), OrderID: order.ID, PspIds: psps})
	return int(n), err
}
