package models

import (
	"encoding/json"
	"fmt"
	"math"
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

// IntPtrTo32 converts a models *int to a generated *int32, clamping instead
// of wrapping if a caller ever hands it a value outside int32's range (none
// of today's callers — retry counts, duration hours — can, but the helper is
// shared and should never truncate silently).
func IntPtrTo32(v *int) *int32 {
	if v == nil {
		return nil
	}
	n := *v
	switch {
	case n > math.MaxInt32:
		n = math.MaxInt32
	case n < math.MinInt32:
		n = math.MinInt32
	}
	i := int32(n)
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
		ID:                p.ID,
		CustomerID:        p.CustomerID,
		PriceID:           p.PriceID,
		SubscriptionID:    p.SubscriptionID,
		RefundedPaymentID: p.RefundedPaymentID,
		Rail:              Rail(p.Rail),
		TransactionID:     p.TransactionID,
		Amount:            p.Amount,
		ListAmount:        p.ListAmount,
		Currency:          p.Currency,
		Status:            string(p.Status),
		PspID:             p.PspID,
		CardBrand:         p.CardBrand,
		CardLast4:         p.CardLast4,
		AttemptKind:       p.AttemptKind,
		FailureCode:       p.FailureCode,
		FailureReason:     p.FailureReason,
		ReversalKind:      p.ReversalKind,
		TokenType:         p.TokenType,
		DiscountCode:      p.DiscountCode,
		DiscountReason:    p.DiscountReason,
		PurchasedAt:       p.PurchasedAt,
		CreatedAt:         p.CreatedAt,
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
		Archived:            p.Archived,
		Amount:              p.Amount,
		Currency:            p.Currency,
		AccessDurationHours: DerefIntPtr(p.AccessDurationHours),
		AutoRenew:           p.AutoRenew,
		TrialUnitAmount:     p.TrialUnitAmount,
		TrialDurationHours:  DerefIntPtr(p.TrialDurationHours),
		Key:                 p.Key,
		CreatedAt:           p.CreatedAt,
		UpdatedAt:           p.UpdatedAt,
	}
	if err := FromJSONB(p.PspLinks, &m.PSPLinks, "prices.psp_links"); err != nil {
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
		Archived:    p.Archived,
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
		PspID:                 s.PspID,
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
		ID:                   p.ID,
		CustomerID:           p.CustomerID,
		Rail:                 Rail(p.Rail),
		PspID:                p.PspID,
		RailCustomerRef:      p.RailCustomerRef,
		RailMethodRef:        p.RailMethodRef,
		RebillDriver:         p.RebillDriver,
		InitialTransactionID: p.InitialTransactionID,
		LastFour:             p.LastFour,
		CardType:             p.CardType,
		ExpiryDate:           p.ExpiryDate,
		CreatedAt:            p.CreatedAt,
		UpdatedAt:            p.UpdatedAt,

		StoredCredentialRecurringRef:   p.StoredCredentialRecurringRef,
		StoredCredentialUnscheduledRef: p.StoredCredentialUnscheduledRef,

		Custodian:          p.Custodian,
		VaultFingerprint:   p.VaultFingerprint,
		NetworkTokenID:     p.NetworkTokenID,
		NetworkTokenStatus: p.NetworkTokenStatus,
		NetworkTokenPAR:    p.NetworkTokenPar,
		ChargeVia:          p.ChargeVia,
		ParkReason:         p.ParkReason,
		ParkedAt:           p.ParkedAt,
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
		ID:             c.ID,
		CustomerID:     c.CustomerID,
		PriceID:        c.PriceID,
		Mode:           CheckoutSessionMode(c.Mode),
		Rail:           Rail(c.Rail),
		Status:         CheckoutSessionStatus(c.Status),
		Amount:         c.Amount,
		Currency:       c.Currency,
		ExpiresAt:      c.ExpiresAt,
		Reference:      c.Reference,
		TransactionID:  c.TransactionID,
		PaymentID:      c.PaymentID,
		SubscriptionID: c.SubscriptionID,
		PspID:          c.PspID,
		CreatedAt:      c.CreatedAt,
		UpdatedAt:      c.UpdatedAt,
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

// NotificationFromGen maps a generated notification_queue row onto the model.
func NotificationFromGen(n gen.OpenrailsNotificationQueue) (*NotificationQueue, error) {
	m := &NotificationQueue{
		ID:         n.ID,
		CustomerID: n.CustomerID,
		EventType:  NotificationEventType(n.EventType),
		Seen:       n.Seen,
		CreatedAt:  n.CreatedAt,
	}
	if err := FromJSONB(n.Data, &m.Data, "notification_queue.data"); err != nil {
		return nil, err
	}
	return m, nil
}

// PriceKeyMovementFromGen maps a generated price_key_movements row (#774).
func PriceKeyMovementFromGen(r gen.OpenrailsPriceKeyMovement) *PriceKeyMovement {
	return &PriceKeyMovement{
		ID:          r.ID,
		MerchantID:  r.MerchantID,
		Key:         r.Key,
		PriceID:     r.PriceID,
		EffectiveAt: r.EffectiveAt,
		CreatedAt:   r.CreatedAt,
	}
}

func PriceKeyMovementsFromGen(rows []gen.OpenrailsPriceKeyMovement) []*PriceKeyMovement {
	out := make([]*PriceKeyMovement, 0, len(rows))
	for _, r := range rows {
		out = append(out, PriceKeyMovementFromGen(r))
	}
	return out
}

// SubscriptionRepriceFromGen maps a generated subscription_reprices row (#773).
func SubscriptionRepriceFromGen(r gen.OpenrailsSubscriptionReprice) *SubscriptionReprice {
	return &SubscriptionReprice{
		ID:                      r.ID,
		MerchantID:              r.MerchantID,
		SubscriptionID:          r.SubscriptionID,
		FromPriceID:             r.FromPriceID,
		ToPriceID:               r.ToPriceID,
		EffectiveAt:             r.EffectiveAt,
		Status:                  RepriceStatus(r.Status),
		RepriceBatchID:          r.RepriceBatchID,
		CreatedAt:               r.CreatedAt,
		AppliedAt:               r.AppliedAt,
		CanceledAt:              r.CanceledAt,
		AcknowledgedShortNotice: r.AcknowledgedShortNotice,
		Kind:                    RepriceKind(r.Kind),
		BlockedReason:           r.BlockedReason,
	}
}

func SubscriptionRepricesFromGen(rows []gen.OpenrailsSubscriptionReprice) []*SubscriptionReprice {
	out := make([]*SubscriptionReprice, 0, len(rows))
	for _, r := range rows {
		out = append(out, SubscriptionRepriceFromGen(r))
	}
	return out
}

// RepriceBatchFromGen maps a generated reprice_batches row (#773). The
// int32->int widenings are always exact (Go's int is 64-bit on every
// platform this project targets).
func RepriceBatchFromGen(r gen.OpenrailsRepriceBatch) *RepriceBatch {
	return &RepriceBatch{
		ID:                     r.ID,
		MerchantID:             r.MerchantID,
		PriceKey:               r.PriceKey,
		ToPriceID:              r.ToPriceID,
		EffectiveAt:            r.EffectiveAt,
		SubscriptionsMatched:   int(r.SubscriptionsMatched),
		SubscriptionsScheduled: int(r.SubscriptionsScheduled),
		SubscriptionsSkipped:   int(r.SubscriptionsSkipped),
		CreatedAt:              r.CreatedAt,
		Kind:                   RepriceKind(r.Kind),
		SourcePriceID:          r.SourcePriceID,
		FallbackPolicy:         r.FallbackPolicy,
		SubscriptionsBlocked:   int(r.SubscriptionsBlocked),
	}
}
