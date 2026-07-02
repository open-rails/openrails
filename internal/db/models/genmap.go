package models

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/gen"
)

// Mapping helpers between sqlc-generated row types (internal/db/gen) and the
// domain models in this package (which double as JSON API types). Moved here
// from internal/db/repo (#688): modules call gen directly and convert with
// these; gen types never leak above the module layer.

// FromJSONB unmarshals a jsonb column ([]byte) into dst; empty/NULL is a
// no-op, leaving dst's zero value (matching bun's nullzero scan behavior).
func FromJSONB[T any](b []byte, dst *T, col string) error {
	if len(b) == 0 {
		return nil
	}
	if err := json.Unmarshal(b, dst); err != nil {
		return fmt.Errorf("models: decode %s: %w", col, err)
	}
	return nil
}

// ToJSONB marshals v for a jsonb column; nil maps/zero-len values become SQL
// NULL (nil slice), matching bun's nullzero insert behavior.
func ToJSONB[M ~map[string]V, V any](m M) ([]byte, error) {
	if m == nil {
		return nil, nil
	}
	return json.Marshal(m)
}

// UpdateTimestamp keeps a zero UpdatedAt from clobbering the column with
// 0001-01-01 on full-row updates.
func UpdateTimestamp(t time.Time) time.Time {
	if t.IsZero() {
		return time.Now()
	}
	return t
}

func DerefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// DerefIntPtr converts a generated *int32 to the models' *int.
func DerefIntPtr(v *int32) *int {
	if v == nil {
		return nil
	}
	i := int(*v)
	return &i
}

func DerefUUID(u *uuid.UUID) uuid.UUID {
	if u == nil {
		return uuid.Nil
	}
	return *u
}

// IntPtrTo32 converts a models *int to a generated *int32.
func IntPtrTo32(v *int) *int32 {
	if v == nil {
		return nil
	}
	i := int32(*v)
	return &i
}

func RevokeReasonPtr(r *EntitlementRevokeReason) *string {
	if r == nil {
		return nil
	}
	s := string(*r)
	return &s
}

func PaymentFromGen(p gen.OpenrailsPayment) (*Payment, error) {
	m := &Payment{
		ID:                    p.ID,
		CustomerID:            p.CustomerID,
		PriceID:               p.PriceID,
		SubscriptionID:        p.SubscriptionID,
		RefundedPaymentID:     p.RefundedPaymentID,
		Rail:                  Rail(p.Rail),
		TransactionID:         p.TransactionID,
		Amount:                p.Amount,
		ListAmount:            p.ListAmount,
		Currency:              p.Currency,
		Status:                string(p.Status),
		RailMerchantAccountID: p.RailMerchantAccountID,
		CardBrand:             p.CardBrand,
		CardLast4:             p.CardLast4,
		DiscountCode:          p.DiscountCode,
		DiscountReason:        p.DiscountReason,
		PurchasedAt:           p.PurchasedAt,
		CreatedAt:             p.CreatedAt,
	}
	if err := FromJSONB(p.DiscountMetadata, &m.DiscountMetadata, "payments.discount_metadata"); err != nil {
		return nil, err
	}
	if err := FromJSONB(p.Metadata, &m.Metadata, "payments.metadata"); err != nil {
		return nil, err
	}
	if err := FromJSONB(p.EntitlementsSpecSnapshot, &m.EntitlementsSpecSnapshot, "payments.entitlements_spec_snapshot"); err != nil {
		return nil, err
	}
	if err := FromJSONB(p.CreditsSpecSnapshot, &m.CreditsSpecSnapshot, "payments.credits_spec_snapshot"); err != nil {
		return nil, err
	}
	return m, nil
}

func PaymentsFromGen(rows []gen.OpenrailsPayment) ([]*Payment, error) {
	out := make([]*Payment, 0, len(rows))
	for _, r := range rows {
		m, err := PaymentFromGen(r)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

func PriceFromGen(p gen.OpenrailsPrice) (*Price, error) {
	m := &Price{
		ID:                  p.ID,
		MerchantID:          p.MerchantID,
		ProductID:           p.ProductID,
		Status:              CatalogStatus(p.Status),
		Amount:              p.Amount,
		Currency:            p.Currency,
		AccessDurationHours: DerefIntPtr(p.AccessDurationHours),
		AutoRenew:           p.AutoRenew,
		TrialUnitAmount:     p.TrialUnitAmount,
		TrialDurationHours:  DerefIntPtr(p.TrialDurationHours),
		CreatedAt:           p.CreatedAt,
		UpdatedAt:           p.UpdatedAt,
	}
	if err := FromJSONB(p.Rails, &m.Rails, "prices.rails"); err != nil {
		return nil, err
	}
	return m, nil
}

func ProductFromGen(p gen.OpenrailsProduct) (*Product, error) {
	m := &Product{
		ID:          p.ID,
		MerchantID:  p.MerchantID,
		Key:         p.Key,
		DisplayName: p.DisplayName,
		Description: DerefStr(p.Description),
		TierGroup:   p.TierGroup,
		TierRank:    int(p.TierRank),
		Status:      CatalogStatus(p.Status),
		CreatedAt:   p.CreatedAt,
		UpdatedAt:   p.UpdatedAt,
	}
	if err := FromJSONB(p.EntitlementsSpec, &m.EntitlementsSpec, "products.entitlements_spec"); err != nil {
		return nil, err
	}
	if err := FromJSONB(p.CreditsSpec, &m.CreditsSpec, "products.credits_spec"); err != nil {
		return nil, err
	}
	return m, nil
}

func SubscriptionFromGen(s gen.OpenrailsSubscription) (*Subscription, error) {
	m := &Subscription{
		ID:                    s.ID,
		MerchantID:            s.MerchantID,
		CustomerID:            s.CustomerID,
		ProductID:             s.ProductID,
		PriceID:               DerefUUID(s.PriceID),
		ScheduledPriceID:      s.ScheduledPriceID,
		Status:                SubscriptionStatus(s.Status),
		StartedAt:             s.StartedAt,
		EndedAt:               s.EndedAt,
		CurrentPeriodStartsAt: s.CurrentPeriodStartsAt,
		CurrentPeriodEndsAt:   s.CurrentPeriodEndsAt,
		Rail:                  Rail(s.Rail),
		RailSubscriptionID:    s.RailSubscriptionID,
		RailMerchantAccountID: s.RailMerchantAccountID,
		UserEmail:             s.UserEmail,
		PaymentMethodID:       s.PaymentMethodID,
		LastRetryAt:           s.LastRetryAt,
		RetryAttempts:         DerefIntPtr(s.RetryAttempts),
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
		ct := CancelType(*s.CancelType)
		m.CancelType = &ct
	}
	if err := FromJSONB(s.EntitlementsSpecSnapshot, &m.EntitlementsSpecSnapshot, "subscriptions.entitlements_spec_snapshot"); err != nil {
		return nil, err
	}
	if err := FromJSONB(s.CreditsSpecSnapshot, &m.CreditsSpecSnapshot, "subscriptions.credits_spec_snapshot"); err != nil {
		return nil, err
	}
	return m, nil
}

func SubscriptionsFromGen(rows []gen.OpenrailsSubscription) ([]*Subscription, error) {
	out := make([]*Subscription, 0, len(rows))
	for _, r := range rows {
		m, err := SubscriptionFromGen(r)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

func PaymentMethodFromGen(p gen.OpenrailsPaymentMethod) (*PaymentMethod, error) {
	m := &PaymentMethod{
		ID:                    p.ID,
		CustomerID:            p.CustomerID,
		Rail:                  Rail(p.Rail),
		RailMerchantAccountID: p.RailMerchantAccountID,
		RailCustomerRef:       p.RailCustomerRef,
		RailMethodRef:         p.RailMethodRef,
		RebillDriver:          p.RebillDriver,
		InitialTransactionID:  p.InitialTransactionID,
		LastFour:              p.LastFour,
		CardType:              p.CardType,
		ExpiryDate:            p.ExpiryDate,
		CreatedAt:             p.CreatedAt,
		UpdatedAt:             p.UpdatedAt,
	}
	if err := FromJSONB(p.Metadata, &m.Metadata, "payment_methods.metadata"); err != nil {
		return nil, err
	}
	return m, nil
}

func PaymentMethodsFromGen(rows []gen.OpenrailsPaymentMethod) ([]*PaymentMethod, error) {
	out := make([]*PaymentMethod, 0, len(rows))
	for _, r := range rows {
		m, err := PaymentMethodFromGen(r)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

func CheckoutSessionFromGen(c gen.OpenrailsCheckoutSession) (*CheckoutSession, error) {
	m := &CheckoutSession{
		ID:                    c.ID,
		CustomerID:            c.CustomerID,
		PriceID:               c.PriceID,
		Mode:                  CheckoutSessionMode(c.Mode),
		Rail:                  Rail(c.Rail),
		Status:                CheckoutSessionStatus(c.Status),
		Amount:                c.Amount,
		Currency:              c.Currency,
		ExpiresAt:             c.ExpiresAt,
		Reference:             c.Reference,
		TransactionID:         c.TransactionID,
		PaymentID:             c.PaymentID,
		SubscriptionID:        c.SubscriptionID,
		IdempotencyKey:        c.IdempotencyKey,
		RailMerchantAccountID: c.RailMerchantAccountID,
		CreatedAt:             c.CreatedAt,
		UpdatedAt:             c.UpdatedAt,
	}
	if err := FromJSONB(c.Metadata, &m.Metadata, "checkout_sessions.metadata"); err != nil {
		return nil, err
	}
	if err := FromJSONB(c.RailFields, &m.RailFields, "checkout_sessions.rail_fields"); err != nil {
		return nil, err
	}
	if err := FromJSONB(c.RailState, &m.RailState, "checkout_sessions.rail_state"); err != nil {
		return nil, err
	}
	return m, nil
}

func EntitlementFromGen(e gen.OpenrailsEntitlement) *Entitlement {
	sourceID := e.SourceID
	m := &Entitlement{
		ID:          e.ID,
		MerchantID:  e.MerchantID,
		CustomerID:  e.CustomerID,
		Entitlement: e.Entitlement,
		StartAt:     e.StartAt,
		EndAt:       e.EndAt,
		SourceID:    &sourceID,
		SourceType:  EntitlementSourceType(e.SourceType),
		RevokedAt:   e.RevokedAt,
		CreatedAt:   e.CreatedAt,
		UpdatedAt:   e.UpdatedAt,
		DeletedAt:   e.DeletedAt,
	}
	if e.RevokeReason != nil {
		rr := EntitlementRevokeReason(*e.RevokeReason)
		m.RevokeReason = &rr
	}
	return m
}

func EntitlementsFromGen(rows []gen.OpenrailsEntitlement) []Entitlement {
	out := make([]Entitlement, 0, len(rows))
	for _, r := range rows {
		out = append(out, *EntitlementFromGen(r))
	}
	return out
}
