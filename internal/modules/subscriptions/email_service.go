package subscriptions

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/sendgrid/sendgrid-go"
	"github.com/sendgrid/sendgrid-go/helpers/mail"
	log "github.com/sirupsen/logrus"

	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	repo "github.com/open-rails/openrails/internal/db/repo"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/payments/processors"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

var errUserEmailUnavailable = errors.New("user email unavailable")

// EmailService handles all email notifications including subscription-related emails.
// It wraps the SendGrid SDK and has domain knowledge for building subscription/payment emails.
type EmailService struct {
	client *sendgrid.Client
	from   *mail.Email
	store  *config.StoreConfig
	Clock  clockwork.Clock

	// Domain dependencies for building subscription emails
	subscriptionService *SubscriptionService
	productService      *catalog.ProductService
	priceService        *catalog.PriceService
	profiles            *repo.ProfileRepo
}

// OneOffPurchaseEmailData contains data for one-off purchase receipts
type OneOffPurchaseEmailData struct {
	UserEmail     string
	Amount        int64 // Amount in cents (smallest currency unit)
	Currency      string
	ProductName   string
	PaymentMethod string
	IsPremium     bool
}

// NewEmailService wires the SendGrid SDK into the billing domain service.
// Sender info (from_email, from_name) comes from StoreConfig.
func NewEmailService(sendgridCfg *config.SendGridConfig, storeCfg *config.StoreConfig) (*EmailService, error) {
	if sendgridCfg == nil {
		return nil, fmt.Errorf("sendgrid configuration not provided")
	}
	if storeCfg == nil {
		return nil, fmt.Errorf("store configuration not provided")
	}

	apiKey := strings.TrimSpace(sendgridCfg.APIKey)
	fromEmail := strings.TrimSpace(storeCfg.FromEmail)
	fromName := strings.TrimSpace(storeCfg.Name)
	if apiKey == "" {
		return nil, fmt.Errorf("sendgrid api_key is required")
	}
	if fromEmail == "" {
		return nil, fmt.Errorf("store.from_email is required when sendgrid is configured")
	}

	client := sendgrid.NewSendClient(apiKey)
	from := mail.NewEmail(fromName, fromEmail)

	return &EmailService{client: client, from: from, store: storeCfg}, nil
}

// SetDomainServices configures the domain services needed for subscription emails.
// This is called after creation to avoid circular dependencies.
func (s *EmailService) SetDomainServices(
	subscriptionService *SubscriptionService,
	productService *catalog.ProductService,
	priceService *catalog.PriceService,
	profiles *repo.ProfileRepo,
) {
	s.subscriptionService = subscriptionService
	s.productService = productService
	s.priceService = priceService
	s.profiles = profiles
}

// now returns the current time from the service's clock, or time.Now() if no clock is set.
func (s *EmailService) now() time.Time {
	if s.Clock != nil {
		return s.Clock.Now()
	}
	return time.Now()
}

// IsEnabled returns true when delivery is possible.
func (s *EmailService) IsEnabled() bool {
	return s != nil && s.client != nil && s.from != nil
}

// SendEmail sends a basic email using the configured provider.
func (s *EmailService) SendEmail(ctx context.Context, to, subject, htmlContent, plainContent string) error {
	if !s.IsEnabled() {
		log.WithContext(ctx).Debug("email service disabled - skipping send")
		return nil
	}

	toMail := mail.NewEmail("", to)
	msg := mail.NewSingleEmail(s.from, subject, toMail, plainContent, htmlContent)

	return s.send(ctx, msg)
}

func (s *EmailService) storeName() string {
	if s == nil || s.store == nil {
		return "My Store"
	}
	name := strings.TrimSpace(s.store.Name)
	if name == "" {
		return "My Store"
	}
	return name
}

func (s *EmailService) storeCustomerPortalURL() string {
	if s == nil || s.store == nil {
		return ""
	}
	return strings.TrimSpace(s.store.CustomerPortalURL)
}

func (s *EmailService) SendSubscriptionConfirmation(ctx context.Context, data SubscriptionEmailData) error {
	rendered := RenderSubscriptionConfirmationEmail(s.storeName(), data)
	return s.SendEmail(ctx, data.UserEmail, rendered.Subject, rendered.HTML, rendered.Plain)
}

func (s *EmailService) SendSubscriptionRenewal(ctx context.Context, data SubscriptionEmailData) error {
	rendered := RenderSubscriptionRenewalEmail(s.storeName(), data)
	return s.SendEmail(ctx, data.UserEmail, rendered.Subject, rendered.HTML, rendered.Plain)
}

func (s *EmailService) SendSubscriptionCancellation(ctx context.Context, data SubscriptionEmailData, reason PremiumEndReason) error {
	rendered := RenderSubscriptionCancellationEmail(s.storeName(), data, reason)
	return s.SendEmail(ctx, data.UserEmail, rendered.Subject, rendered.HTML, rendered.Plain)
}

func (s *EmailService) SendSubscriptionExpired(ctx context.Context, data SubscriptionEmailData) error {
	rendered := RenderSubscriptionExpiredEmail(s.storeName(), s.storeCustomerPortalURL(), data)
	return s.SendEmail(ctx, data.UserEmail, rendered.Subject, rendered.HTML, rendered.Plain)
}

func (s *EmailService) sendPaymentFailed(ctx context.Context, data SubscriptionEmailData) error {
	rendered := RenderPaymentFailedEmail(s.storeName(), s.storeCustomerPortalURL(), data)
	return s.SendEmail(ctx, data.UserEmail, rendered.Subject, rendered.HTML, rendered.Plain)
}

func (s *EmailService) SendEntitlementExpiration(ctx context.Context, userEmail, username, entitlementName string, expiresAt time.Time) error {
	subject := fmt.Sprintf("Your %s access expires soon", entitlementName)
	storeName := s.storeName()
	daysUntilExpiry := int(time.Until(expiresAt).Hours() / 24)
	htmlContent := fmt.Sprintf(`
		<h2>Access Expiring Soon</h2>
		<p>Hi %s,</p>
		<p>This is a reminder that your <strong>%s</strong> access will expire in %d days on <strong>%s</strong>.</p>
		<p>To continue enjoying premium features, please renew your subscription before the expiration date.</p>
		<p>Thank you for being a valued member!</p>
		<p>The %s Team</p>
	`, username, entitlementName, daysUntilExpiry, expiresAt.Format("January 2, 2006"), storeName)
	plainContent := fmt.Sprintf(`
		Access Expiring Soon

		Hi %s,

		Your %s access will expire in %d days on %s.

		To continue enjoying premium features, please renew your subscription before the expiration date.

		Thank you for being a valued member!
		The %s Team
	`, username, entitlementName, daysUntilExpiry, expiresAt.Format("January 2, 2006"), storeName)
	return s.SendEmail(ctx, userEmail, subject, htmlContent, plainContent)
}

// SendTemplatedEmail sends a template-based email using the configured provider.
func (s *EmailService) SendTemplatedEmail(ctx context.Context, to, templateID string, templateData map[string]any) error {
	if !s.IsEnabled() {
		log.WithContext(ctx).WithField("template_id", templateID).Debug("email service disabled - skipping templated send")
		return nil
	}

	toMail := mail.NewEmail("", to)
	msg := mail.NewV3Mail()
	msg.SetFrom(s.from)
	msg.SetTemplateID(templateID)
	personalization := mail.NewPersonalization()
	personalization.AddTos(toMail)
	for key, value := range templateData {
		personalization.SetDynamicTemplateData(key, value)
	}
	msg.AddPersonalizations(personalization)

	return s.send(ctx, msg)
}

// SendOneOffPurchaseReceipt sends a receipt for a one-off purchase (e.g., Solana payment).
func (s *EmailService) SendOneOffPurchaseReceipt(ctx context.Context, data OneOffPurchaseEmailData) error {
	if !s.IsEnabled() {
		log.WithContext(ctx).Debug("email service disabled - skipping one-off receipt send")
		return nil
	}

	productName := data.ProductName
	if productName == "" {
		productName = "Premium content"
	}

	amountLine := moneyutil.FormatDisplay(data.Amount, data.Currency)

	issuedAt := s.now().Format("Jan 2, 2006 15:04 MST")

	paymentMethod := strings.ToLower(data.PaymentMethod)
	isSolana := paymentMethod == "solana"

	storeName := "My Store"
	if s.store != nil && strings.TrimSpace(s.store.Name) != "" {
		storeName = strings.TrimSpace(s.store.Name)
	}

	subject := fmt.Sprintf("Thanks for supporting %s!", storeName)
	if isSolana {
		subject = "Your Solana purchase is confirmed"
	}

	messageIntro := "Thanks for completing your purchase!"
	if data.IsPremium {
		messageIntro = fmt.Sprintf("Thanks for unlocking %s Premium!", storeName)
	}

	if isSolana {
		htmlContent := fmt.Sprintf(`
			<h2>Solana Payment Received</h2>
			<p>Hi there,</p>
			<p>%s This one-time Solana transaction instantly extended your premium access.</p>
			<ul>
			<li><strong>Product:</strong> %s</li>
			<li><strong>Amount:</strong> %s</li>
			<li><strong>Date:</strong> %s</li>
			</ul>
			<p>Enjoy your premium benefits; no rebill will occur automatically.</p>
			<p>The %s Team</p>
		`, messageIntro, productName, amountLine, issuedAt, storeName)

		plainContent := fmt.Sprintf(`
		Solana Payment Received

		%s This one-time Solana transaction instantly extended your premium access.
		Product: %s
		Amount: %s
		Date: %s

		Enjoy your premium benefits; there won't be an automatic rebill.
		The %s Team
		`, messageIntro, productName, amountLine, issuedAt, storeName)

		return s.SendEmail(ctx, data.UserEmail, subject, htmlContent, plainContent)
	}

	htmlContent := fmt.Sprintf(`
		<h2>Payment Received</h2>
		<p>Hi there,</p>
		<p>%s</p>
		<ul>
			<li><strong>Product:</strong> %s</li>
			<li><strong>Amount:</strong> %s</li>
			<li><strong>Date:</strong> %s</li>
		</ul>
		<p>Your access has been updated instantly. Enjoy!</p>
		<p>The %s Team</p>
	`, messageIntro, productName, amountLine, issuedAt, storeName)

	plainContent := fmt.Sprintf(`
		Payment Received

		%s
		Product: %s
		Amount: %s
		Date: %s

		Your access has been updated instantly. Enjoy!
		The %s Team
	`, messageIntro, productName, amountLine, issuedAt, storeName)

	return s.SendEmail(ctx, data.UserEmail, subject, htmlContent, plainContent)
}

func (s *EmailService) send(ctx context.Context, msg *mail.SGMailV3) error {
	res, err := s.client.SendWithContext(ctx, msg)
	if err != nil {
		return fmt.Errorf("sendgrid email send failed: %w", err)
	}
	if res.StatusCode >= 400 {
		return fmt.Errorf("sendgrid api error: status %d, body: %s", res.StatusCode, res.Body)
	}

	log.WithContext(ctx).WithField("status", res.StatusCode).Debug("email sent successfully via sendgrid")
	return nil
}

// ============================================================================
// Subscription Email Methods (formerly in SubscriptionEmailService)
// ============================================================================

// SendSubscriptionConfirmed sends a subscription confirmation email
func (s *EmailService) SendSubscriptionConfirmed(ctx context.Context, userID string) error {
	if !s.IsEnabled() {
		log.WithContext(ctx).Debug("email service not available - skipping subscription confirmation email")
		return nil
	}

	emailData, err := s.getEmailData(ctx, userID)
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
func (s *EmailService) SendSubscriptionRenewed(ctx context.Context, userID string) error {
	if !s.IsEnabled() {
		log.WithContext(ctx).Debug("email service not available - skipping subscription renewal email")
		return nil
	}

	emailData, err := s.getEmailData(ctx, userID)
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
func (s *EmailService) SendPremiumEnded(ctx context.Context, userID string, reason PremiumEndReason) error {
	if !s.IsEnabled() {
		log.WithContext(ctx).Debug("email service not available - skipping premium-ended email")
		return nil
	}

	emailData, err := s.getEmailData(ctx, userID)
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
	case PremiumEndReasonChargeback, PremiumEndReasonRefund, PremiumEndReasonAdmin, PremiumEndReasonProcessor:
		return s.SendSubscriptionCancellation(ctx, *emailData, reason)
	case PremiumEndReasonUserCancel:
		fallthrough
	case PremiumEndReasonUnknown:
		return s.SendSubscriptionCancellation(ctx, *emailData, PremiumEndReasonUserCancel)
	default:
		return s.SendSubscriptionCancellation(ctx, *emailData, reason)
	}
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
	if s.subscriptionService == nil {
		return nil, fmt.Errorf("subscription service not configured")
	}

	// Get the user's active subscription or last known subscription as fallback
	subscription, err := s.subscriptionService.GetActiveSubscription(ctx, userID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			subscription, err = s.subscriptionService.GetByUserID(ctx, userID)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return nil, errUserEmailUnavailable
				}
				return nil, fmt.Errorf("failed to get subscription for user %s: %w", userID, err)
			}
		} else {
			return nil, fmt.Errorf("failed to get active subscription: %w", err)
		}
	}

	var (
		username string
		email    string
	)

	if subscription.UserEmail != nil && strings.TrimSpace(*subscription.UserEmail) != "" {
		email = strings.TrimSpace(*subscription.UserEmail)
	}

	if email == "" {
		var err error
		username, email, err = s.getUserEmail(ctx, userID)
		if err != nil {
			return nil, err
		}
	}

	if email == "" {
		return nil, errUserEmailUnavailable
	}

	price, err := s.priceService.GetByID(ctx, subscription.PriceID)
	if err != nil {
		return nil, fmt.Errorf("failed to get price: %w", err)
	}
	priceName := strings.TrimSpace(price.DisplayName)

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
		if price.BillingCycleDays != nil && *price.BillingCycleDays > 0 {
			periodEnd = periodStart.AddDate(0, 0, *price.BillingCycleDays)
		} else {
			periodEnd = periodStart.AddDate(0, 1, 0)
		}
		if subscription.CurrentPeriodEndsAt != nil {
			periodEnd = *subscription.CurrentPeriodEndsAt
		}
	}

	paymentMethod := describePaymentMethod(subscription)
	if paymentMethod == "" {
		paymentMethod = processorDisplayName(subscription.Processor)
		if paymentMethod == "" {
			paymentMethod = "Credit Card"
		}
	}

	return &SubscriptionEmailData{
		UserEmail:      email,
		Username:       username,
		SubscriptionID: subscription.ID,
		ProductName:    productName,
		PriceName:      priceName,
		Amount:         price.Amount,
		Currency:       price.Currency,
		PeriodStart:    periodStart,
		PeriodEnd:      periodEnd,
		PaymentMethod:  paymentMethod,
		TransactionID:  "",
	}, nil
}

// getUserEmail gets user profile and validates email exists
func (s *EmailService) getUserEmail(ctx context.Context, userID string) (username string, email string, err error) {
	if s.profiles == nil {
		return "", "", errUserEmailUnavailable
	}
	uid, perr := uuid.Parse(userID)
	if perr != nil {
		return "", "", errUserEmailUnavailable
	}
	uname, mail, verified, active, qerr := s.profiles.GetUserEmail(ctx, uid)
	if qerr != nil || mail == "" || !active {
		return "", "", errUserEmailUnavailable
	}
	_ = verified
	return uname, mail, nil
}

func describePaymentMethod(subscription *models.Subscription) string {
	if subscription == nil || subscription.PaymentMethod == nil {
		return ""
	}

	pm := subscription.PaymentMethod
	cardType := ""
	if pm.CardType != nil {
		cardType = strings.TrimSpace(*pm.CardType)
	}
	lastFour := ""
	if pm.LastFour != nil {
		lastFour = strings.TrimSpace(*pm.LastFour)
	}

	parts := make([]string, 0, 2)
	if cardType != "" {
		parts = append(parts, cardType)
	} else {
		friendly := processorDisplayName(pm.Processor)
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

func processorDisplayName(processor models.Processor) string {
	if processors.IsNMIBackedProcessor(processor) {
		return "Credit Card"
	}

	switch processor {
	case models.ProcessorCCBill:
		return "Credit Card"
	case models.ProcessorPayPal:
		return "PayPal"
	case models.ProcessorSolana:
		return "Solana"
	default:
		clean := strings.TrimSpace(string(processor))
		if clean == "" {
			return ""
		}
		return strings.ToUpper(clean)
	}
}
