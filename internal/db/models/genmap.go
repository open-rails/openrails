package models

import (
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/db/gen"
)

// Mapping helpers between sqlc row types (internal/db/gen) and these domain
// models; gen types never leak above the module layer.

// FromJSONB unmarshals a jsonb column into dst; empty/NULL is a no-op.
func FromJSONB[T any](b []byte, dst *T, col string) error {
	if len(b) == 0 {
		return nil
	}
	if err := json.Unmarshal(b, dst); err != nil {
		return fmt.Errorf("models: decode %s: %w", col, err)
	}
	return nil
}

// ToJSONB marshals m for a jsonb column; a nil or empty map becomes SQL NULL.
func ToJSONB[M ~map[string]V, V any](m M) ([]byte, error) {
	if m == nil {
		return nil, nil
	}
	return json.Marshal(m)
}

// PointerToJSONB preserves an absent optional object as SQL NULL.
func PointerToJSONB[T any](value *T) ([]byte, error) {
	if value == nil {
		return nil, nil
	}
	return json.Marshal(value)
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

// SeatsOf is a seat count a query answers as zero when it has none.
func SeatsOf(v int32) *int {
	if v <= 0 {
		return nil
	}
	n := int(v)
	return &n
}

// IntPtrTo32 converts a models *int to a generated *int32, clamping an
// out-of-range value instead of wrapping.
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

func PaymentFromGen(p gen.BillingPayment) (*Payment, error) {
	m := &Payment{
		ID:                p.ID,
		CustomerID:        p.CustomerID,
		PriceID:           DerefUUID(p.PriceID),
		SubscriptionID:    p.SubscriptionID,
		OrderID:           p.OrderID,
		InvoiceID:         p.InvoiceID,
		LedgerTransferID:  p.LedgerTransferID,
		RefundedPaymentID: p.RefundedPaymentID,
		Channel:           Channel(p.Channel),
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
		MoneyMovement:     MoneyMovement(p.MoneyMovement),
		Quantity:          DerefIntPtr(p.Quantity),
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
	if err := FromJSONB(p.CreditGrantSnapshot, &m.CreditGrantSnapshot, "payments.credit_grant_snapshot"); err != nil {
		return nil, err
	}
	if p.Rail != nil {
		m.Rail = Rail(*p.Rail)
	}
	return m, nil
}

func PaymentsFromGen(rows []gen.BillingPayment) ([]*Payment, error) {
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

func PriceFromGen(p gen.BillingPrice) (*Price, error) {
	m := &Price{Revision: p.Revision,
		ID:                   p.ID,
		MerchantID:           p.MerchantID,
		ProductID:            p.ProductID,
		Archived:             p.Archived,
		Amount:               p.Amount,
		Currency:             p.Currency,
		AccessDurationHours:  DerefIntPtr(p.AccessDurationHours),
		BillingIntervalHours: DerefIntPtr(p.BillingIntervalHours),
		TrialUnitAmount:      p.TrialUnitAmount,
		TrialDurationHours:   DerefIntPtr(p.TrialDurationHours),
		Key:                  p.Key,
		CreatedAt:            p.CreatedAt,
		UpdatedAt:            p.UpdatedAt,
	}
	if err := FromJSONB(p.CustomerAmount, &m.CustomerAmount, "prices.customer_amount"); err != nil {
		return nil, err
	}
	if err := FromJSONB(p.Quantity, &m.Quantity, "prices.quantity"); err != nil {
		return nil, err
	}
	return m, nil
}

func ProductFromGen(p gen.BillingProduct) (*Product, error) {
	m := &Product{Revision: p.Revision,
		ID:          p.ID,
		MerchantID:  p.MerchantID,
		Key:         p.Key,
		DisplayName: p.DisplayName,
		Description: DerefStr(p.Description),
		TierGroup:   p.TierGroup,
		TierRank:    int(p.TierRank),
		Ownership:   catalog.Ownership(DerefStr(p.Ownership)),
		Archived:    p.Archived,
		CreatedAt:   p.CreatedAt,
		UpdatedAt:   p.UpdatedAt,
	}
	if err := FromJSONB(p.CreditGrant, &m.CreditGrant, "products.credit_grant"); err != nil {
		return nil, err
	}
	return m, nil
}

func SubscriptionFromGen(s gen.BillingSubscription) (*Subscription, error) {
	m := &Subscription{
		AccessDurationHoursSnapshot: DerefIntPtr(s.AccessDurationHoursSnapshot),
		ID:                          s.ID,
		MerchantID:                  s.MerchantID,
		CustomerID:                  s.CustomerID,
		ProductID:                   s.ProductID,
		PriceID:                     DerefUUID(s.PriceID),
		Quantity:                    DerefIntPtr(s.Quantity),
		Status:                      SubscriptionStatus(s.Status),
		StartedAt:                   s.StartedAt,
		EndedAt:                     s.EndedAt,
		CurrentPeriodStartsAt:       s.CurrentPeriodStartsAt,
		CurrentPeriodEndsAt:         s.CurrentPeriodEndsAt,
		Rail:                        Rail(s.Rail),
		RailSubscriptionID:          DerefStr(s.RailSubscriptionID),
		CollectionPolicy:            CollectionPolicy(s.CollectionPolicy),
		PspID:                       s.PspID,
		PaymentMethodID:             s.PaymentMethodID,
		LastRetryAt:                 s.LastRetryAt,
		RetryAttempts:               DerefIntPtr(s.RetryAttempts),
		TransientRetries:            int(s.TransientRetries),
		NextRetryAt:                 s.NextRetryAt,
		GraceEndsAt:                 s.GraceEndsAt,
		CancelFeedback:              s.CancelFeedback,
		CanceledAt:                  s.CanceledAt,
		DeletionScheduledAt:         s.DeletionScheduledAt,
		Metadata:                    s.GatewayResponse,
		CreatedAt:                   s.CreatedAt,
		UpdatedAt:                   s.UpdatedAt,
		LifecycleRev:                s.LifecycleRev,
		RowVersion:                  s.RowVersion,
		DunningPolicy:               s.DunningPolicy,
	}
	if s.CancelType != nil {
		ct := CancelType(*s.CancelType)
		m.CancelType = &ct
	}
	m.RememberLifecycle()
	return m, nil
}

func SubscriptionsFromGen(rows []gen.BillingSubscription) ([]*Subscription, error) {
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

func PaymentMethodFromGen(p gen.BillingPaymentMethod) (*PaymentMethod, error) {
	m := &PaymentMethod{
		ID:              p.ID,
		CustomerID:      p.CustomerID,
		Rail:            Rail(p.Rail),
		PspID:           p.PspID,
		RailCustomerRef: DerefStr(p.RailCustomerRef),
		RailMethodRef:   DerefStr(p.RailMethodRef),

		Card:      CardFromColumns(p.CardBrand, p.CardLast4, p.CardExpMonth, p.CardExpYear),
		CreatedAt: p.CreatedAt,
		UpdatedAt: p.UpdatedAt,

		Custodian:          p.Custodian,
		CustodianID:        p.CustodianID,
		Fingerprint:        DerefStr(p.Fingerprint),
		NetworkTokenID:     DerefStr(p.NetworkTokenID),
		NetworkTokenStatus: DerefStr(p.NetworkTokenStatus),
		NetworkTokenPAR:    DerefStr(p.NetworkTokenPar),
		ChargeVia:          p.ChargeVia,
		ParkReason:         DerefStr(p.ParkReason),
		ParkedAt:           p.ParkedAt,

		Status:              p.Status,
		ReplacedByID:        p.ReplacedByID,
		SetupRef:            DerefStr(p.SetupRef),
		ContactCardholderAt: p.ContactCardholderAt,
	}
	if err := FromJSONB(p.Metadata, &m.Metadata, "payment_methods.metadata"); err != nil {
		return nil, err
	}
	return m, nil
}

func PaymentMethodsFromGen(rows []gen.BillingPaymentMethod) ([]*PaymentMethod, error) {
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

func CheckoutAttemptFromGen(c gen.BillingCheckoutAttempt) (*CheckoutAttempt, error) {
	m := &CheckoutAttempt{
		ID:             c.ID,
		CustomerID:     c.CustomerID,
		PriceID:        c.PriceID,
		Mode:           CheckoutAttemptMode(c.Mode),
		Rail:           Rail(c.Rail),
		Status:         CheckoutAttemptStatus(c.Status),
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
	if err := m.ValidateTerms(); err != nil {
		return nil, err
	}
	if err := FromJSONB(c.Metadata, &m.Metadata, "checkout_attempts.metadata"); err != nil {
		return nil, err
	}
	if err := FromJSONB(c.RailFields, &m.RailFields, "checkout_attempts.rail_fields"); err != nil {
		return nil, err
	}
	if err := FromJSONB(c.RailState, &m.RailState, "checkout_attempts.rail_state"); err != nil {
		return nil, err
	}
	if len(c.RoutingReason) > 0 {
		var reason CheckoutRoutingReason
		if err := FromJSONB(c.RoutingReason, &reason, "checkout_attempts.routing_reason"); err != nil {
			return nil, err
		}
		m.RoutingReason = &reason
	}
	return m, nil
}

// NotificationFromGen maps a generated notifications row onto the model.
func NotificationFromGen(n gen.BillingNotification) (*NotificationQueue, error) {
	if n.RecipientKind != "customer" || n.CustomerID == nil {
		return nil, fmt.Errorf("notification %s is not a customer notification", n.ID)
	}
	m := &NotificationQueue{
		ID:         n.ID,
		CustomerID: *n.CustomerID,
		EventType:  NotificationEventType(n.EventType),
		Seen:       n.ReadAt != nil,
		CreatedAt:  n.CreatedAt,
	}
	if err := FromJSONB(n.Data, &m.Data, "notifications.data"); err != nil {
		return nil, err
	}
	return m, nil
}

// PriceKeyMovementFromGen maps a generated price_key_movements row.
func PriceKeyMovementFromGen(r gen.BillingPriceKeyMovement) *PriceKeyMovement {
	return &PriceKeyMovement{
		Archived:    r.Archived,
		ID:          r.ID,
		MerchantID:  r.MerchantID,
		Key:         r.Key,
		PriceID:     r.PriceID,
		EffectiveAt: r.EffectiveAt,
		CreatedAt:   r.CreatedAt,
	}
}

func PriceKeyMovementsFromGen(rows []gen.BillingPriceKeyMovement) []*PriceKeyMovement {
	out := make([]*PriceKeyMovement, 0, len(rows))
	for _, r := range rows {
		out = append(out, PriceKeyMovementFromGen(r))
	}
	return out
}
