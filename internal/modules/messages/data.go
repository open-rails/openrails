package messages

import (
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// fact is what a message is about and who it is to.
type fact struct {
	EventType  string
	Data       billing.NotificationData
	OccurredAt time.Time
	CustomerID uuid.UUID
	// Customer is the name the message greets.
	Customer string
	// ProductName is the subscription's product when the fact names none.
	ProductName string
	Profile     models.MerchantProfileConfiguration
}

// messageData is f as a message renders it.
func messageData(f fact) billing.MessageData {
	d, p := f.Data, f.Profile
	product := d.ProductName
	if product == "" {
		product = f.ProductName
	}
	if d.ToProductName == "" {
		d.ToProductName = d.NewProduct
	}
	return billing.MessageData{
		Version:   billing.MessageDataVersion,
		EventType: f.EventType,
		Customer:  billing.MessageCustomer{ID: billing.CustomerID(f.CustomerID).String(), Name: f.Customer},
		Merchant: billing.MessageMerchant{
			Name: p.DisplayName, LogoURL: p.LogoURL, SupportURL: p.SupportURL, BillingURL: p.BillingURL, SubscribeURL: p.SubscribeURL,
			TermsURL: p.TermsURL, PrivacyURL: p.PrivacyURL, StatementDescriptor: p.StatementDescriptor,
		},
		Event: billing.MessageEvent{
			OccurredAt: day(&f.OccurredAt), Reason: d.Reason,
			ProductName: product, FromProductName: d.FromProductName, ToProductName: d.ToProductName,
			Amount: money(d.Amount, d.Currency), OldAmount: money(d.OldAmount, d.Currency), NewAmount: money(d.NewAmount, d.Currency),
			AmountDue: money(d.AmountDue, d.Currency), OverdueAmount: money(d.OverdueAmount, d.Currency),
			AvailableAmount: money(d.AvailableAmount, d.Currency), OwedAmount: money(d.OwedAmount, d.Currency),
			LowBalanceAmount: money(d.LowBalanceAmount, d.Currency),
			EndedAt:          day(d.EndedAt), EffectiveAt: day(d.EffectiveAt), PeriodStartsAt: day(d.PeriodStartsAt), PeriodEndsAt: day(d.PeriodEndsAt),
			DueAt: day(d.DueAt), NextAttemptAt: day(d.NextAttemptAt), RunwayEndsAt: day(d.RunwayEndsAt), OverdueStartedAt: day(d.OverdueStartedAt),
			OrderNumber: count(d.OrderNumber), OverdueOrders: count(int64(d.OverdueOrders)), Quantity: quantity(d.Quantity),
			AccessEnded: d.AccessEnded, DowngradeApplied: d.DowngradeApplied,
			RetryStatus: d.RetryStatus, RecoveryAction: d.RecoveryAction, CollectionMethod: d.CollectionMethod,
			OrderID: id(d.OrderID.IsZero(), d.OrderID.String()), PaymentID: id(d.PaymentID.IsZero(), d.PaymentID.String()),
			RefundedPaymentID: id(d.RefundedPaymentID.IsZero(), d.RefundedPaymentID.String()),
			SubscriptionID:    id(d.SubscriptionID.IsZero(), d.SubscriptionID.String()),
		},
		Links: links(p),
	}
}

// links are the profile's destinations, each falling back to its support
// page.
func links(p models.MerchantProfileConfiguration) billing.MessageLinks {
	or := func(urls ...string) string {
		for _, u := range urls {
			if u != "" {
				return u
			}
		}
		return ""
	}
	billingURL := or(p.BillingURL, p.SupportURL)
	return billing.MessageLinks{ManageBilling: billingURL, UpdatePaymentMethod: billingURL, Pay: billingURL, Resubscribe: or(p.SubscribeURL, billingURL)}
}

func money(v *int64, currency string) string {
	if v == nil || currency == "" {
		return ""
	}
	return moneyutil.FormatAmount(*v, currency)
}

func day(t *time.Time) string {
	if t == nil || t.IsZero() {
		return ""
	}
	return t.UTC().Format("January 2, 2006")
}

func count(n int64) string {
	if n <= 0 {
		return ""
	}
	return strconv.FormatInt(n, 10)
}

func quantity(n *int) string {
	if n == nil {
		return ""
	}
	return strconv.Itoa(*n)
}

func id(zero bool, s string) string {
	if zero {
		return ""
	}
	return s
}

// Sample is data an event type's message renders from in a check: every
// field the event type has, filled, from a sample merchant.
func Sample(eventType string) billing.MessageData { return messageData(sample(eventType)) }

// sample is a fact of eventType with every field it has filled, to a sample
// customer from a sample merchant.
func sample(eventType string) fact {
	at := time.Date(2026, 11, 23, 15, 4, 0, 0, time.UTC)
	later, sooner := at.AddDate(0, 1, 0), at.AddDate(0, 0, 7)
	amount := func(v int64) *int64 { return &v }
	seats := 3
	d := billing.NotificationData{Currency: "USD"}
	order, payment := billing.OrderID(uuid.MustParse("019a0000-0000-7000-8000-000000000001")), billing.PaymentID(uuid.MustParse("019a0000-0000-7000-8000-000000000002"))
	sub := billing.SubscriptionID(uuid.MustParse("019a0000-0000-7000-8000-000000000003"))
	switch models.NotificationEventType(eventType) {
	case models.NotificationOneOffPurchaseCompleted, models.NotificationUsageCharged:
		d.Amount, d.ProductName, d.PaymentID, d.OrderID, d.OrderNumber = amount(12_500_000), "Field guide", payment, order, 1001
	case models.NotificationRefundCompleted:
		d.Amount, d.ProductName, d.PaymentID, d.OrderID, d.OrderNumber = amount(12_500_000), "Field guide", payment, order, 1001
		d.RefundedPaymentID = billing.PaymentID(uuid.MustParse("019a0000-0000-7000-8000-000000000004"))
	case models.NotificationPremiumStarted:
		d.SubscriptionID = sub
	case models.NotificationPremiumRenewed:
		d.SubscriptionID, d.PeriodStartsAt, d.PeriodEndsAt = sub, &at, &later
	case models.NotificationPremiumEnded:
		d.SubscriptionID, d.Reason, d.EndedAt, d.AccessEnded = sub, "user_cancel", &at, true
	case models.NotificationSubscriptionCanceled:
		d.SubscriptionID, d.Reason, d.EndedAt = sub, "user_cancel", &later
	case models.NotificationPaymentMethodFailed:
		d.SubscriptionID, d.Amount, d.NextAttemptAt = sub, amount(9_990_000), &sooner
		d.RetryStatus, d.RecoveryAction = billing.NotificationRetryScheduled, billing.NotificationRecoveryUpdatePaymentMethod
	case models.NotificationPaymentMethodUpdateRequired:
		d.SubscriptionID = sub
		d.RetryStatus, d.RecoveryAction = billing.NotificationRetryPaymentMethodRequired, billing.NotificationRecoveryUpdatePaymentMethod
	case models.NotificationPaymentAuthenticationRequired:
		d.Amount, d.ProductName, d.OrderID = amount(9_990_000), "Pro plan", order
		d.RetryStatus, d.RecoveryAction = billing.NotificationRetryStopped, billing.NotificationRecoveryAuthenticatePayment
	case models.NotificationOrderCollectionStopped:
		d.OrderID, d.AmountDue, d.Reason = order, amount(30_000_000), "schedule_exhausted"
		d.RetryStatus, d.RecoveryAction = billing.NotificationRetryStopped, billing.NotificationRecoveryUpdatePaymentMethod
	case models.NotificationAccountDelinquent, models.NotificationAccountDelinquencyClosed:
		d.OverdueAmount, d.OverdueOrders, d.OverdueStartedAt = amount(30_000_000), 2, &at
	case models.NotificationBalanceLow, models.NotificationBalanceDepleted:
		d.AvailableAmount, d.OwedAmount, d.LowBalanceAmount, d.RunwayEndsAt = amount(4_000_000), amount(0), amount(5_000_000), &sooner
	case models.NotificationOrderDue, models.NotificationOrderOverdue:
		d.OrderID, d.AmountDue, d.DueAt, d.CollectionMethod = order, amount(30_000_000), &sooner, billing.CollectSendInvoice
	case models.NotificationSubscriptionRepriceScheduled:
		d.SubscriptionID, d.OldAmount, d.NewAmount, d.EffectiveAt = sub, amount(10_000_000), amount(12_000_000), &later
	case models.NotificationSubscriptionPlanChangeScheduled:
		d.SubscriptionID, d.OldAmount, d.NewAmount, d.EffectiveAt, d.ToProductName = sub, amount(10_000_000), amount(20_000_000), &later, "Team plan"
	case models.NotificationSubscriptionChanged:
		d.SubscriptionID, d.Amount, d.Quantity, d.EffectiveAt, d.ToProductName = sub, amount(5_000_000), &seats, &at, "Team plan"
	case models.NotificationAgreementRevoked:
		d.EffectiveAt, d.AmountDue, d.OrderID = &at, amount(3_000_000), order
	case models.NotificationPaymentMethodAutoUpdated:
	}
	return fact{
		EventType: eventType, Data: d, OccurredAt: at, CustomerID: uuid.MustParse("019a0000-0000-7000-8000-000000000005"),
		Customer: "Ada Lovelace", ProductName: "Pro plan",
		Profile: models.MerchantProfileConfiguration{
			DisplayName: "Example Store", LogoURL: "https://store.example/logo.png", SupportURL: "https://store.example/help",
			BillingURL: "https://store.example/account/billing", SubscribeURL: "https://store.example/plans",
			TermsURL: "https://store.example/terms", PrivacyURL: "https://store.example/privacy", StatementDescriptor: "EXAMPLE STORE",
		},
	}
}
