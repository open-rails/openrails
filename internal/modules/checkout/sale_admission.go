package checkout

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/productaccess"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/merchant"
)

func (s *CheckoutPurchaseService) transactionBound(d *db.DB) *CheckoutPurchaseService {
	out := NewCheckoutPurchaseService(catalog.NewPriceService(d), catalog.NewProductService(d), payments.NewPaymentService(d, s.clock), entitlements.NewEntitlementService(d, s.clock), nil, s.clock)
	if s.SubscriptionService != nil {
		out.SubscriptionService = subscriptions.NewSubscriptionService(d, out.PriceService, out.ProductService, nil, s.clock)
	}
	out.ProductAccessService = productaccess.NewService(d, s.clock)
	return out
}

func saleRequestFingerprint(req *CheckoutRequest, user *UserIdentity, price uuid.UUID, target railTarget) string {
	raw, _ := json.Marshal(struct {
		Customer string
		Price    uuid.UUID
		PSP      string
		Request  *CheckoutRequest
	}{user.ID, price, target.PSP, req})
	digest := sha256.Sum256(raw)
	return fmt.Sprintf("%x", digest)
}

func ownsSaleRequest(in gen.OpenrailsRailIntent, customer string, price uuid.UUID, fingerprint string) error {
	p, err := payments.DecodeNMISalePayload(in)
	if err != nil {
		return err
	}
	if p.UserID != customer || p.PriceID != price || p.RequestFingerprint != fingerprint {
		return apperr.Conflictf("checkout idempotency key belongs to another accepted purchase")
	}
	return nil
}

func saleReplayParams(in gen.OpenrailsRailIntent) intents.EnqueueParams {
	return intents.EnqueueParams{MerchantID: in.MerchantID, Provider: in.Rail, PspID: *in.PspID, IntentType: payments.TypeNMISale, PriceID: in.PriceID, Payload: json.RawMessage(in.Payload), IdempotencyKey: in.IdempotencyKey, NextAttemptAt: in.NextAttemptAt, Origin: intents.Origin(in.Origin)}
}

func (s *CheckoutNMISaleService) prepareAcceptedSale(ctx context.Context, d *db.DB, req *CheckoutRequest, user *UserIdentity, priceID, methodID uuid.UUID, target railTarget, fingerprint string) (payments.NMISalePayload, error) {
	var out payments.NMISalePayload
	purchase := s.PurchaseService.transactionBound(d)
	price, err := purchase.PriceService.GetByID(ctx, priceID)
	if err != nil {
		return out, err
	}
	product, err := purchase.ProductService.GetByID(ctx, price.ProductID)
	if err != nil {
		return out, err
	}
	if price.AutoRenew {
		return out, errors.New("sale requires a one-time price")
	}
	eligibility, err := purchase.CheckPurchaseEligibility(ctx, user.ID, price.ID)
	if err != nil {
		return out, err
	}
	if eligibility.Status != EligibilityAllowed {
		return out, apperr.Conflictf("purchase is not eligible: %s", eligibility.Reason)
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return out, err
	}
	method, err := d.Gen(ctx).GetPaymentMethodForShare(ctx, gen.GetPaymentMethodForShareParams{MerchantID: mid.UUID(), ID: methodID})
	if err != nil {
		return out, err
	}
	if target.Scope == nil || method.PspID != target.Scope.ID || method.CustomerID.String() != user.ID || method.Custodian != models.CustodianPSP || method.ParkReason != "" {
		return out, errors.New("sale instrument does not match customer and provider")
	}
	now := purchase.now().UTC()
	start := now
	if eligibility.Coverage != nil && eligibility.Coverage.EndDate != nil && eligibility.Coverage.EndDate.After(start) {
		start = eligibility.Coverage.EndDate.UTC()
	}
	var end *time.Time
	if price.AccessDurationHours != nil && *price.AccessDurationHours > 0 {
		v := now.Add(time.Duration(*price.AccessDurationHours) * time.Hour)
		end = &v
	}
	entitlements := models.CloneEntitlementsSpec(product.EntitlementsSpec)
	if entitlements == nil {
		entitlements = map[string]*int{}
	}
	out = payments.NMISalePayload{Provider: "nmi", PSP: target.PSP, Amount: price.Amount, Currency: price.Currency, Description: fmt.Sprintf("Purchase: %s", product.DisplayName), UserID: user.ID, PriceID: price.ID, E2ERunID: strings.TrimSpace(req.Metadata["e2e_run_id"]), PaymentMethodID: method.ID, Instrument: charge.FreezeInstrument(method), PaymentID: uuidutil.NewV7(), ProductID: product.ID, ListAmount: price.Amount, AcceptedAt: now, Entitlements: entitlements, AccessDurationHours: price.AccessDurationHours, EntitlementStart: start, OwnershipStart: now, OwnershipEnd: end, Eligibility: string(eligibility.Status), RequestFingerprint: fingerprint}
	return out, nil
}
