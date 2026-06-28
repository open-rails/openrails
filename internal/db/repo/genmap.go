package repo

import (
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
)

// Mapping helpers between sqlc-generated row types (internal/db/gen) and the
// domain models (internal/db/models) that double as JSON API types. The
// boundary rule (#334): repos accept and return models.*; gen types never
// leak above the repo layer.

// fromJSONB unmarshals a jsonb column ([]byte) into dst; empty/NULL is a
// no-op, leaving dst's zero value (matching bun's nullzero scan behavior).
func fromJSONB[T any](b []byte, dst *T, col string) error {
	if len(b) == 0 {
		return nil
	}
	if err := json.Unmarshal(b, dst); err != nil {
		return fmt.Errorf("repo: decode %s: %w", col, err)
	}
	return nil
}

// toJSONB marshals v for a jsonb column; nil maps/zero-len values become SQL
// NULL (nil slice), matching bun's nullzero insert behavior.
func toJSONB[M ~map[string]V, V any](m M) ([]byte, error) {
	if m == nil {
		return nil, nil
	}
	return json.Marshal(m)
}

func paymentFromGen(p gen.OpenrailsPayment) (*models.Payment, error) {
	m := &models.Payment{
		ID:                p.ID,
		CustomerID:        p.CustomerID,
		PriceID:           p.PriceID,
		SubscriptionID:    p.SubscriptionID,
		RefundedPaymentID: p.RefundedPaymentID,
		Rail:              models.Rail(p.Rail),
		TransactionID:     p.TransactionID,
		Amount:            p.Amount,
		ListAmount:        p.ListAmount,
		Currency:          p.Currency,
		Status:            string(p.Status),
		CardBrand:         p.CardBrand,
		CardLast4:         p.CardLast4,
		DiscountCode:      p.DiscountCode,
		DiscountReason:    p.DiscountReason,
		PurchasedAt:       p.PurchasedAt,
		CreatedAt:         p.CreatedAt,
	}
	if err := fromJSONB(p.DiscountMetadata, &m.DiscountMetadata, "payments.discount_metadata"); err != nil {
		return nil, err
	}
	if err := fromJSONB(p.Metadata, &m.Metadata, "payments.metadata"); err != nil {
		return nil, err
	}
	if err := fromJSONB(p.EntitlementsSpecSnapshot, &m.EntitlementsSpecSnapshot, "payments.entitlements_spec_snapshot"); err != nil {
		return nil, err
	}
	if err := fromJSONB(p.CreditsSpecSnapshot, &m.CreditsSpecSnapshot, "payments.credits_spec_snapshot"); err != nil {
		return nil, err
	}
	return m, nil
}

func paymentsFromGen(rows []gen.OpenrailsPayment) ([]*models.Payment, error) {
	out := make([]*models.Payment, 0, len(rows))
	for _, r := range rows {
		m, err := paymentFromGen(r)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

func priceFromGen(p gen.OpenrailsPrice) (*models.Price, error) {
	m := &models.Price{
		ID:               p.ID,
		MerchantID:       p.MerchantID,
		ProductID:        p.ProductID,
		Status:           models.CatalogStatus(p.Status),
		Amount:           p.Amount,
		Currency:         p.Currency,
		BillingCycleDays: derefIntPtr(p.BillingCycleDays),
		CreatedAt:        p.CreatedAt,
		UpdatedAt:        p.UpdatedAt,
	}
	if err := fromJSONB(p.Rails, &m.Rails, "prices.rails"); err != nil {
		return nil, err
	}
	return m, nil
}

func productFromGen(p gen.OpenrailsProduct) (*models.Product, error) {
	m := &models.Product{
		ID:          p.ID,
		MerchantID:  p.MerchantID,
		Slug:        p.Slug,
		DisplayName: p.DisplayName,
		Description: derefStr(p.Description),
		TierGroup:   p.TierGroup,
		TierRank:    int(p.TierRank),
		Status:      models.CatalogStatus(p.Status),
		CreatedAt:   p.CreatedAt,
		UpdatedAt:   p.UpdatedAt,
	}
	if err := fromJSONB(p.EntitlementsSpec, &m.EntitlementsSpec, "products.entitlements_spec"); err != nil {
		return nil, err
	}
	if err := fromJSONB(p.CreditsSpec, &m.CreditsSpec, "products.credits_spec"); err != nil {
		return nil, err
	}
	return m, nil
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// derefIntPtr converts a generated *int32 to the models' *int.
func derefIntPtr(v *int32) *int {
	if v == nil {
		return nil
	}
	i := int(*v)
	return &i
}

func derefUUID(u *uuid.UUID) uuid.UUID {
	if u == nil {
		return uuid.Nil
	}
	return *u
}

func subscriptionFromGen(s gen.OpenrailsSubscription) (*models.Subscription, error) {
	m := &models.Subscription{
		ID:                    s.ID,
		MerchantID:            s.MerchantID,
		CustomerID:            s.CustomerID,
		ProductID:             s.ProductID,
		PriceID:               derefUUID(s.PriceID),
		ScheduledPriceID:      s.ScheduledPriceID,
		Status:                models.SubscriptionStatus(s.Status),
		StartedAt:             s.StartedAt,
		EndedAt:               s.EndedAt,
		CurrentPeriodStartsAt: s.CurrentPeriodStartsAt,
		CurrentPeriodEndsAt:   s.CurrentPeriodEndsAt,
		Rail:                  models.Rail(s.Rail),
		RailSubscriptionID:    s.RailSubscriptionID,
		UserEmail:             s.UserEmail,
		PaymentMethodID:       s.PaymentMethodID,
		LastRetryAt:           s.LastRetryAt,
		RetryAttempts:         derefIntPtr(s.RetryAttempts),
		NextRetryAt:           s.NextRetryAt,
		GraceEndsAt:           s.GraceEndsAt,
		CancelFeedback:        s.CancelFeedback,
		CancelledAt:           s.CancelledAt,
		DeletionScheduledAt:   s.DeletionScheduledAt,
		Metadata:              s.GatewayResponse,
		CreatedAt:             s.CreatedAt,
		UpdatedAt:             s.UpdatedAt,
	}
	if s.CancelType != nil {
		ct := models.CancelType(*s.CancelType)
		m.CancelType = &ct
	}
	if err := fromJSONB(s.EntitlementsSpecSnapshot, &m.EntitlementsSpecSnapshot, "subscriptions.entitlements_spec_snapshot"); err != nil {
		return nil, err
	}
	if err := fromJSONB(s.CreditsSpecSnapshot, &m.CreditsSpecSnapshot, "subscriptions.credits_spec_snapshot"); err != nil {
		return nil, err
	}
	return m, nil
}

func subscriptionsFromGen(rows []gen.OpenrailsSubscription) ([]*models.Subscription, error) {
	out := make([]*models.Subscription, 0, len(rows))
	for _, r := range rows {
		m, err := subscriptionFromGen(r)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

func paymentMethodFromGen(p gen.OpenrailsPaymentMethod) (*models.PaymentMethod, error) {
	m := &models.PaymentMethod{
		ID:                   p.ID,
		CustomerID:           p.CustomerID,
		Rail:                 models.Rail(p.Rail),
		RailCustomerRef:      p.RailCustomerRef,
		RailMethodRef:        p.RailMethodRef,
		InitialTransactionID: p.InitialTransactionID,
		LastFour:             p.LastFour,
		CardType:             p.CardType,
		ExpiryDate:           p.ExpiryDate,
		CreatedAt:            p.CreatedAt,
		UpdatedAt:            p.UpdatedAt,
	}
	if err := fromJSONB(p.Metadata, &m.Metadata, "payment_methods.metadata"); err != nil {
		return nil, err
	}
	return m, nil
}

func paymentMethodsFromGen(rows []gen.OpenrailsPaymentMethod) ([]*models.PaymentMethod, error) {
	out := make([]*models.PaymentMethod, 0, len(rows))
	for _, r := range rows {
		m, err := paymentMethodFromGen(r)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

func checkoutSessionFromGen(c gen.OpenrailsCheckoutSession) (*models.CheckoutSession, error) {
	m := &models.CheckoutSession{
		ID:                c.ID,
		CustomerID:        c.CustomerID,
		PriceID:           c.PriceID,
		Mode:              models.CheckoutSessionMode(c.Mode),
		Rail:              models.Rail(c.Rail),
		Status:            models.CheckoutSessionStatus(c.Status),
		Amount:            c.Amount,
		Currency:          c.Currency,
		ExpiresAt:         c.ExpiresAt,
		Reference:         c.Reference,
		TransactionID:     c.TransactionID,
		PaymentID:         c.PaymentID,
		SubscriptionID:    c.SubscriptionID,
		IdempotencyKey:    c.IdempotencyKey,
		ProviderAccountID: c.ProviderAccountID,
		CreatedAt:         c.CreatedAt,
		UpdatedAt:         c.UpdatedAt,
	}
	if err := fromJSONB(c.Metadata, &m.Metadata, "checkout_sessions.metadata"); err != nil {
		return nil, err
	}
	if err := fromJSONB(c.RailFields, &m.RailFields, "checkout_sessions.rail_fields"); err != nil {
		return nil, err
	}
	if err := fromJSONB(c.RailState, &m.RailState, "checkout_sessions.rail_state"); err != nil {
		return nil, err
	}
	return m, nil
}

func entitlementFromGen(e gen.OpenrailsEntitlement) *models.Entitlement {
	sourceID := e.SourceID
	m := &models.Entitlement{
		ID:          e.ID,
		MerchantID:  e.MerchantID,
		CustomerID:  e.CustomerID,
		Entitlement: e.Entitlement,
		StartAt:     e.StartAt,
		EndAt:       e.EndAt,
		SourceID:    &sourceID,
		SourceType:  models.EntitlementSourceType(e.SourceType),
		RevokedAt:   e.RevokedAt,
		CreatedAt:   e.CreatedAt,
		UpdatedAt:   e.UpdatedAt,
		DeletedAt:   e.DeletedAt,
	}
	if e.RevokeReason != nil {
		rr := models.EntitlementRevokeReason(*e.RevokeReason)
		m.RevokeReason = &rr
	}
	return m
}

func entitlementsFromGen(rows []gen.OpenrailsEntitlement) []models.Entitlement {
	out := make([]models.Entitlement, 0, len(rows))
	for _, r := range rows {
		out = append(out, *entitlementFromGen(r))
	}
	return out
}

func revokeReasonPtr(r *models.EntitlementRevokeReason) *string {
	if r == nil {
		return nil
	}
	s := string(*r)
	return &s
}
