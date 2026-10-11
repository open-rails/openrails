package subscriptions

import (
	"context"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/identity"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/internal/shared/cadence"
	"github.com/open-rails/openrails/internal/shared/timeutil"
)

var errUserEmailUnavailable = errors.New("user email unavailable")

// sendTimeout bounds one send, so a hung provider never stalls the webhook or
// lifecycle path that sends the email.
const sendTimeout = 10 * time.Second

// EmailService renders billing email (subscription, payment and receipt
// notices) and hands it to the deployment's email sender, from the merchant.
type EmailService struct {
	mail         config.EmailSender
	profileStore *merchantconfig.Store
	clock        clockwork.Clock

	// Domain dependencies for building subscription emails
	subscriptionService *SubscriptionService
	productService      *catalog.ProductService
	priceService        *catalog.PriceService
	users               identity.UserDirectory
}

// NewEmailService renders billing email for sender; a nil sender sends none.
// Sender info is merchant-scoped: the merchant document's profile.
func NewEmailService(sender config.EmailSender, profileStore *merchantconfig.Store, clocks ...clockwork.Clock) *EmailService {
	return &EmailService{mail: sender, profileStore: profileStore, clock: timeutil.FirstClock(clocks...)}
}

func (s *EmailService) SetClock(c clockwork.Clock) {
	s.clock = timeutil.FirstClock(c)
}

func (s *EmailService) Clock() clockwork.Clock {
	return s.clock
}

// SetDomainServices configures the domain services needed for subscription emails.
// This is called after creation to avoid circular dependencies.
func (s *EmailService) SetDomainServices(
	subscriptionService *SubscriptionService,
	productService *catalog.ProductService,
	priceService *catalog.PriceService,
	users identity.UserDirectory,
) {
	s.subscriptionService = subscriptionService
	s.productService = productService
	s.priceService = priceService
	s.users = users
}

// now returns the service clock's time, or time.Now() without one.
func (s *EmailService) now() time.Time {
	if s.clock != nil {
		return s.clock.Now()
	}
	return time.Now()
}

// IsEnabled returns true when delivery is possible.
func (s *EmailService) IsEnabled() bool {
	return s != nil && s.mail != nil
}

// SendEmail sends a basic email using the configured provider.
func (s *EmailService) SendEmail(ctx context.Context, to, subject, htmlContent, plainContent string) error {
	if !s.IsEnabled() {
		log.WithContext(ctx).Debug("email service disabled - skipping send")
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	// A merchant without a from_email sends from the deployment's address.
	from := config.EmailAddress{Name: s.storeName(ctx), Address: strings.TrimSpace(s.merchantProfile(ctx).FromEmail)}
	if err := s.mail.Send(ctx, config.Email{From: from, To: to, Subject: subject, Text: plainContent, HTML: htmlContent}); err != nil {
		return fmt.Errorf("send email: %w", err)
	}
	return nil
}

func (s *EmailService) merchantProfile(ctx context.Context) models.MerchantProfileConfiguration {
	if s == nil || s.profileStore == nil {
		return models.MerchantProfileConfiguration{}
	}
	cfg, _, err := s.profileStore.Get(ctx)
	if err != nil {
		log.WithContext(ctx).WithError(err).Debug("merchant profile unavailable for email")
		return models.MerchantProfileConfiguration{}
	}
	return cfg.Profile
}

func (s *EmailService) storeName(ctx context.Context) string {
	name := strings.TrimSpace(s.merchantProfile(ctx).DisplayName)
	if name == "" {
		return "OpenRails"
	}
	return name
}

// cancelURL is where the merchant's customers cancel: its billing page,
// else its support page; "" for neither.
func (s *EmailService) cancelURL(ctx context.Context) string {
	p := s.merchantProfile(ctx)
	if u := strings.TrimSpace(p.BillingURL); u != "" {
		return u
	}
	return strings.TrimSpace(p.SupportURL)
}

// signupURL is the merchant's winback/signup page; "" when undeclared.
func (s *EmailService) signupURL(ctx context.Context) string {
	return strings.TrimSpace(s.merchantProfile(ctx).SignupURL)
}

func (s *EmailService) SendSubscriptionConfirmation(ctx context.Context, data SubscriptionEmailData) error {
	rendered := RenderSubscriptionConfirmationEmail(s.storeName(ctx), data)
	return s.SendEmail(ctx, data.UserEmail, rendered.Subject, rendered.HTML, rendered.Plain)
}

func (s *EmailService) SendSubscriptionRenewal(ctx context.Context, data SubscriptionEmailData) error {
	rendered := RenderSubscriptionRenewalEmail(s.storeName(ctx), data)
	return s.SendEmail(ctx, data.UserEmail, rendered.Subject, rendered.HTML, rendered.Plain)
}

func (s *EmailService) SendSubscriptionCancellation(ctx context.Context, data SubscriptionEmailData, reason PremiumEndReason) error {
	rendered := RenderSubscriptionCancellationEmail(s.storeName(ctx), data, reason)
	return s.SendEmail(ctx, data.UserEmail, rendered.Subject, rendered.HTML, rendered.Plain)
}

func (s *EmailService) SendSubscriptionExpired(ctx context.Context, data SubscriptionEmailData) error {
	rendered := RenderSubscriptionExpiredEmail(s.storeName(ctx), s.signupURL(ctx), data)
	return s.SendEmail(ctx, data.UserEmail, rendered.Subject, rendered.HTML, rendered.Plain)
}

// SendPaymentMethodUpdateRequired is the bucket-2 send: charging has stopped
// but the subscription and the customer's access are intact.
func (s *EmailService) SendPaymentMethodUpdateRequired(ctx context.Context, userID string) error {
	if !s.IsEnabled() {
		log.WithContext(ctx).Debug("email service not available - skipping payment-method-update email")
		return nil
	}
	emailData, err := s.getEmailData(ctx, userID)
	if err != nil {
		if errors.Is(err, errUserEmailUnavailable) {
			log.WithContext(ctx).Debug("email unavailable - skipping payment-method-update email")
			return nil
		}
		return fmt.Errorf("failed to get email data: %w", err)
	}
	rendered := RenderPaymentMethodUpdateRequiredEmail(s.storeName(ctx), s.signupURL(ctx), *emailData)
	return s.SendEmail(ctx, emailData.UserEmail, rendered.Subject, rendered.HTML, rendered.Plain)
}

// SendNotice emails a notice rendered from its own data: a usage order's,
// arrears delinquency's, or a scheduled price change's.
func (s *EmailService) SendNotice(ctx context.Context, n *models.NotificationQueue) error {
	if !s.IsEnabled() {
		return nil
	}
	username, email, err := s.getUserEmail(ctx, n.CustomerID.String())
	if errors.Is(err, errUserEmailUnavailable) {
		log.WithContext(ctx).WithField("event_type", n.EventType).Debug("email unavailable - skipping notice email")
		return nil
	}
	if err != nil {
		return err
	}
	store, billingURL := s.storeName(ctx), s.cancelURL(ctx)
	var content EmailContent
	switch n.EventType {
	case models.NotificationOrderDue:
		content = RenderOrderDueEmail(store, billingURL, username, n.Data)
	case models.NotificationOrderOverdue:
		content = RenderOrderOverdueEmail(store, billingURL, username, n.Data)
	case models.NotificationPaymentMethodFailed:
		content = RenderOrderCardFailedEmail(store, billingURL, username, n.Data)
	case models.NotificationPaymentMethodUpdateRequired:
		content = RenderOrderCardUpdateEmail(store, billingURL, username, n.Data)
	case models.NotificationOrderCollectionStopped:
		content = RenderOrderCollectionStoppedEmail(store, billingURL, username, n.Data)
	case models.NotificationAccountDelinquent:
		content = RenderAccountDelinquentEmail(store, billingURL, username, n.Data)
	case models.NotificationAccountDelinquencyClosed:
		content = RenderAccountDelinquencyClearedEmail(store, username)
	case models.NotificationSubscriptionRepriceScheduled, models.NotificationSubscriptionPlanChangeScheduled:
		content = RenderPriceChangeEmail(store, billingURL, username, s.priceProductName(ctx, n.Data.FromPriceID), n.Data)
	default:
		return fmt.Errorf("no notice email for %s", n.EventType)
	}
	return s.SendEmail(ctx, email, content.Subject, content.HTML, content.Plain)
}

// priceProductName is the display name of a price's product; "" when it
// cannot be read.
func (s *EmailService) priceProductName(ctx context.Context, id billing.PriceID) string {
	if s.priceService == nil || s.productService == nil || id.IsZero() {
		return ""
	}
	price, err := s.priceService.GetByID(ctx, id.UUID())
	if err != nil {
		return ""
	}
	product, err := s.productService.GetByID(ctx, price.ProductID)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(product.DisplayName)
}

// SendPaymentAuthenticationRequired asks the customer to confirm a renewal's
// payment their bank wants authenticated, on the merchant's site.
func (s *EmailService) SendPaymentAuthenticationRequired(ctx context.Context, notification *models.NotificationQueue) error {
	if !s.IsEnabled() {
		return nil
	}
	emailData, err := s.getEmailData(ctx, notification.CustomerID.String())
	if err != nil {
		if errors.Is(err, errUserEmailUnavailable) {
			return nil
		}
		return fmt.Errorf("failed to get email data: %w", err)
	}
	rendered := RenderPaymentAuthenticationRequiredEmail(s.storeName(ctx), s.signupURL(ctx), emailData.Username, notification.Data)
	return s.SendEmail(ctx, emailData.UserEmail, rendered.Subject, rendered.HTML, rendered.Plain)
}

func (s *EmailService) sendPaymentFailed(ctx context.Context, data SubscriptionEmailData) error {
	rendered := RenderPaymentFailedEmail(s.storeName(ctx), "", data)
	return s.SendEmail(ctx, data.UserEmail, rendered.Subject, rendered.HTML, rendered.Plain)
}

func (s *EmailService) SendEntitlementExpiration(ctx context.Context, userEmail, username, entitlementName string, expiresAt time.Time) error {
	subject := fmt.Sprintf("Your %s access expires soon", entitlementName)
	storeName := s.storeName(ctx)
	remaining := cadence.FormatRemaining(expiresAt.Sub(s.now()))
	expiresOn := cadence.FormatInstant(expiresAt, expiresAt.Sub(s.now()))
	htmlContent := renderEmailHTML("entitlement_expiring", emailFields{"Username": username, "Entitlement": entitlementName, "Remaining": remaining, "ExpiresOn": expiresOn, "Store": storeName})
	plainContent := fmt.Sprintf(`
		Access Expiring Soon

		Hi %s,

		Your %s access will expire in %s on %s.

		To continue enjoying premium features, please renew your subscription before the expiration date.

		Thank you for being a valued member!
		The %s Team
	`, username, entitlementName, remaining, expiresOn, storeName)
	return s.SendEmail(ctx, userEmail, subject, htmlContent, plainContent)
}

// SendPurchaseReceipt emails a one-off purchase's receipt to the address the
// purchase was made with, else the customer's.
func (s *EmailService) SendPurchaseReceipt(ctx context.Context, n *models.NotificationQueue) error {
	if !s.IsEnabled() {
		return nil
	}
	if n.Data.Amount == nil {
		return fmt.Errorf("receipt %s has no amount", n.ID)
	}
	username, email, err := s.getUserEmail(ctx, n.CustomerID.String())
	if n.Data.UserEmail != "" {
		email, err = n.Data.UserEmail, nil
	}
	if errors.Is(err, errUserEmailUnavailable) {
		log.WithContext(ctx).WithField("notification_id", n.ID).Warn("receipt: customer has no email")
		return nil
	}
	if err != nil {
		return err
	}
	content := RenderPurchaseReceiptEmail(s.storeName(ctx), username, n.Data, n.CreatedAt)
	return s.SendEmail(ctx, email, content.Subject, content.HTML, content.Plain)
}

// SendSubscriptionChanged sends the receipt of a change staff made at the
// customer's request.
func (s *EmailService) SendSubscriptionChanged(ctx context.Context, userID string, data billing.NotificationData) error {
	if !s.IsEnabled() {
		return nil
	}
	username, email, err := s.getUserEmail(ctx, userID)
	if errors.Is(err, errUserEmailUnavailable) {
		log.WithContext(ctx).Debug("email unavailable - skipping subscription change email")
		return nil
	}
	if err != nil {
		return err
	}
	content := RenderSubscriptionChangedEmail(s.storeName(ctx), username, data)
	return s.SendEmail(ctx, email, content.Subject, content.HTML, content.Plain)
}

// SendSubscriptionConfirmed sends a subscription confirmation email
func (s *EmailService) SendSubscriptionConfirmed(ctx context.Context, userID string, subscriptionID uuid.UUID) error {
	if !s.IsEnabled() {
		log.WithContext(ctx).Debug("email service not available - skipping subscription confirmation email")
		return nil
	}

	emailData, err := s.getEmailDataFor(ctx, userID, subscriptionID)
	if err != nil {
		if errors.Is(err, errUserEmailUnavailable) {
			log.WithContext(ctx).Debug("email unavailable - skipping subscription confirmation email")
			return nil
		}
		return fmt.Errorf("failed to get email data: %w", err)
	}

	return s.SendSubscriptionConfirmation(ctx, *emailData)
}

// SendSubscriptionRenewed sends a subscription renewal email
func (s *EmailService) SendSubscriptionRenewed(ctx context.Context, userID string, subscriptionID uuid.UUID) error {
	if !s.IsEnabled() {
		log.WithContext(ctx).Debug("email service not available - skipping subscription renewal email")
		return nil
	}

	emailData, err := s.getEmailDataFor(ctx, userID, subscriptionID)
	if err != nil {
		if errors.Is(err, errUserEmailUnavailable) {
			log.WithContext(ctx).Debug("email unavailable - skipping subscription renewal email")
			return nil
		}
		return fmt.Errorf("failed to get email data: %w", err)
	}

	return s.SendSubscriptionRenewal(ctx, *emailData)
}

// SendPremiumEnded sends the appropriate email when a premium entitlement ends.
func (s *EmailService) SendPremiumEnded(ctx context.Context, userID string, subscriptionID uuid.UUID, reason PremiumEndReason) error {
	if !s.IsEnabled() {
		log.WithContext(ctx).Debug("email service not available - skipping premium-ended email")
		return nil
	}

	// access_ended works for grant-only customers: no subscription row
	// required, so it resolves the user directly instead of via getEmailData.
	if reason == PremiumEndReasonAccessEnded {
		return s.SendAccessEnded(ctx, userID, s.now())
	}

	emailData, err := s.getEmailDataFor(ctx, userID, subscriptionID)
	if err != nil {
		if errors.Is(err, errUserEmailUnavailable) {
			log.WithContext(ctx).Debug("email unavailable - skipping premium-ended email")
			return nil
		}
		return fmt.Errorf("failed to get email data: %w", err)
	}

	switch reason {
	case PremiumEndReasonExpired:
		return s.SendSubscriptionExpired(ctx, *emailData)
	case PremiumEndReasonNonRecoverable:
		// Bucket 3: the mandate is gone. Their saved card was not touched, and
		// re-subscribing is the way back.
		rendered := RenderSubscriptionNonRecoverableEmail(s.storeName(ctx), s.signupURL(ctx), *emailData)
		return s.SendEmail(ctx, emailData.UserEmail, rendered.Subject, rendered.HTML, rendered.Plain)
	case PremiumEndReasonChargeback, PremiumEndReasonRefund, PremiumEndReasonAdmin, PremiumEndReasonRail:
		return s.SendSubscriptionCancellation(ctx, *emailData, reason)
	case PremiumEndReasonUserCancel:
		fallthrough
	case PremiumEndReasonUnknown:
		return s.SendSubscriptionCancellation(ctx, *emailData, PremiumEndReasonUserCancel)
	default:
		return s.SendSubscriptionCancellation(ctx, *emailData, reason)
	}
}

// SendAccessEnded sends the neutral access-ended notice. It needs no
// subscription row, so grant-only customers get it too.
func (s *EmailService) SendAccessEnded(ctx context.Context, userID string, endedAt time.Time) error {
	if !s.IsEnabled() {
		log.WithContext(ctx).Debug("email service not available - skipping access-ended email")
		return nil
	}
	username, email, err := s.getUserEmail(ctx, userID)
	if err != nil {
		if errors.Is(err, errUserEmailUnavailable) {
			log.WithContext(ctx).Debug("email unavailable - skipping access-ended email")
			return nil
		}
		return fmt.Errorf("failed to get user profile: %w", err)
	}
	rendered := RenderAccessEndedEmail(s.storeName(ctx), s.signupURL(ctx), username, endedAt)
	return s.SendEmail(ctx, email, rendered.Subject, rendered.HTML, rendered.Plain)
}

// SendPaymentFailed sends a payment failure email
func (s *EmailService) SendPaymentFailed(ctx context.Context, userID string) error {
	if !s.IsEnabled() {
		log.WithContext(ctx).Debug("email service not available - skipping payment failure email")
		return nil
	}

	emailData, err := s.getEmailData(ctx, userID)
	if err != nil {
		if errors.Is(err, errUserEmailUnavailable) {
			log.WithContext(ctx).Debug("email unavailable - skipping payment failure email")
			return nil
		}
		return fmt.Errorf("failed to get email data: %w", err)
	}

	return s.sendPaymentFailed(ctx, *emailData)
}

// SendEntitlementExpired sends an entitlement expiration email
func (s *EmailService) SendEntitlementExpired(ctx context.Context, userID string, entitlementName string, expiresAt time.Time) error {
	if !s.IsEnabled() {
		log.WithContext(ctx).Debug("email service not available - skipping entitlement expiration email")
		return nil
	}

	username, email, err := s.getUserEmail(ctx, userID)
	if err != nil {
		if errors.Is(err, errUserEmailUnavailable) {
			log.WithContext(ctx).Debug("email unavailable - skipping entitlement expiration email")
			return nil
		}
		return fmt.Errorf("failed to get user profile: %w", err)
	}

	return s.SendEntitlementExpiration(ctx, email, username, entitlementName, expiresAt)
}

// getEmailData fetches subscription data for email notifications
func (s *EmailService) getEmailData(ctx context.Context, userID string) (*SubscriptionEmailData, error) {
	return s.getEmailDataFor(ctx, userID, uuid.Nil)
}

// getEmailDataFor is the email data of the customer's subscription
// subscriptionID; uuid.Nil, or one that is not the customer's, falls back to
// its active or last subscription.
func (s *EmailService) getEmailDataFor(ctx context.Context, userID string, subscriptionID uuid.UUID) (*SubscriptionEmailData, error) {
	if s.subscriptionService == nil {
		return nil, fmt.Errorf("subscription service not configured")
	}

	var subscription *models.Subscription
	var err error
	if subscriptionID != uuid.Nil {
		if named, nerr := s.subscriptionService.GetByID(ctx, subscriptionID); nerr == nil && named.CustomerID.String() == userID {
			subscription = named
		}
	}
	if subscription == nil {
		subscription, err = s.subscriptionService.GetActiveSubscription(ctx, userID)
	}
	if err != nil {
		if db.IsNotFound(err) {
			subscription, err = s.subscriptionService.GetByUserID(ctx, userID)
			if err != nil {
				if db.IsNotFound(err) {
					return nil, errUserEmailUnavailable
				}
				return nil, fmt.Errorf("failed to get subscription for user %s: %w", userID, err)
			}
		} else {
			return nil, fmt.Errorf("failed to get active subscription: %w", err)
		}
	}

	username, email, err := s.getUserEmail(ctx, userID)
	if err != nil {
		return nil, err
	}

	if email == "" {
		return nil, errUserEmailUnavailable
	}

	price, err := s.priceService.GetByID(ctx, subscription.PriceID)
	if err != nil {
		return nil, fmt.Errorf("failed to get price: %w", err)
	}

	productName := ""
	if s.productService != nil {
		if prod, perr := s.productService.GetByID(ctx, price.ProductID); perr == nil {
			productName = strings.TrimSpace(prod.DisplayName)
		}
	}

	periodStart := s.now()
	periodEnd := s.now()
	if subscription.CurrentPeriodStartsAt != nil {
		periodStart = *subscription.CurrentPeriodStartsAt
		switch cycleHours := price.RecurringCycleHours(); {
		case subscription.CurrentPeriodEndsAt != nil:
			periodEnd = *subscription.CurrentPeriodEndsAt
		case cycleHours != nil && *cycleHours > 0:
			periodEnd = periodStart.Add(time.Duration(*cycleHours) * time.Hour)
		default:
			return nil, fmt.Errorf("subscription %s has no period end and price %s no billing cycle", subscription.ID, price.ID)
		}
	}

	// An immediate cancel ends access before the paid period does.
	if subscription.EndedAt != nil && subscription.EndedAt.Before(periodEnd) {
		periodEnd = *subscription.EndedAt
	}

	paymentMethod := describePaymentMethod(subscription)
	if paymentMethod == "" {
		paymentMethod = railDisplayName(subscription.Rail)
		if paymentMethod == "" {
			paymentMethod = "Credit Card"
		}
	}

	return &SubscriptionEmailData{
		UserEmail:      email,
		Username:       username,
		SubscriptionID: subscription.ID,
		ProductName:    productName,
		Amount:         price.Amount,
		Currency:       price.Currency,
		PeriodStart:    periodStart,
		PeriodEnd:      periodEnd,
		PaymentMethod:  paymentMethod,
		TransactionID:  "",
		CancelURL:      s.cancelURL(ctx),
	}, nil
}

// getUserEmail is the customer's declared username and email. A customer
// with no email, or a failed lookup, is errUserEmailUnavailable: callers skip
// the email (emails are best-effort, never load-bearing).
func (s *EmailService) getUserEmail(ctx context.Context, userID string) (username string, email string, err error) {
	if s.users == nil {
		return "", "", errUserEmailUnavailable
	}
	uname, mail, ok, qerr := s.users.EmailIdentity(ctx, userID)
	if qerr != nil {
		log.WithContext(ctx).WithError(qerr).Warn("user directory email lookup failed; skipping email")
		return "", "", errUserEmailUnavailable
	}
	if !ok || mail == "" {
		return "", "", errUserEmailUnavailable
	}
	return uname, mail, nil
}

func describePaymentMethod(subscription *models.Subscription) string {
	if subscription == nil || subscription.PaymentMethod == nil {
		return ""
	}

	pm := subscription.PaymentMethod
	cardType, lastFour := pm.Card.Brand, pm.Card.Last4

	parts := make([]string, 0, 2)
	if cardType != "" {
		parts = append(parts, cardType)
	} else {
		friendly := railDisplayName(pm.Rail)
		if friendly != "" {
			parts = append(parts, friendly)
		}
	}

	if lastFour != "" {
		parts = append(parts, fmt.Sprintf("****%s", lastFour))
	}

	if len(parts) == 0 {
		return ""
	}

	return strings.Join(parts, " ")
}

// railDisplayName is the rail registry's display name; unknown rails fall back
// to upper case.
func railDisplayName(rail models.Rail) string {
	return rails.DisplayName(rail)
}

// SendUsageCharged emails the receipt of a usage order charged under the
// customer's collection agreement, with how to cancel it.
func (s *EmailService) SendUsageCharged(ctx context.Context, n *models.NotificationQueue) error {
	if !s.IsEnabled() {
		return nil
	}
	if n.Data.Amount == nil {
		return fmt.Errorf("usage receipt %s has no amount", n.ID)
	}
	username, email, err := s.getUserEmail(ctx, n.CustomerID.String())
	if errors.Is(err, errUserEmailUnavailable) {
		return nil
	}
	if err != nil {
		return err
	}
	content := RenderUsageChargedEmail(s.storeName(ctx), username, s.cancelURL(ctx), n.Data, n.CreatedAt)
	return s.SendEmail(ctx, email, content.Subject, content.HTML, content.Plain)
}

// SendAgreementRevoked confirms the customer's revocation of its agreement
// to be charged for usage.
func (s *EmailService) SendAgreementRevoked(ctx context.Context, n *models.NotificationQueue) error {
	if !s.IsEnabled() {
		return nil
	}
	username, email, err := s.getUserEmail(ctx, n.CustomerID.String())
	if errors.Is(err, errUserEmailUnavailable) {
		return nil
	}
	if err != nil {
		return err
	}
	content := RenderAgreementRevokedEmail(s.storeName(ctx), username, s.cancelURL(ctx), n.Data, n.CreatedAt)
	return s.SendEmail(ctx, email, content.Subject, content.HTML, content.Plain)
}
