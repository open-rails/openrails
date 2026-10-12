package checkout

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/abuse"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
)

// CardAttemptsBlockedError refuses a card attempt while the customer is
// blocked by the card-testing ledger.
type CardAttemptsBlockedError struct{ RetryAfter time.Duration }

func (e *CardAttemptsBlockedError) Error() string {
	return fmt.Sprintf("checkout: too many declined card attempts; retry after %s", e.RetryAfter)
}

// SetCardFailureLedger arms card-testing blocks on every checkout path.
func (s *CheckoutAttemptService) SetCardFailureLedger(l *abuse.FailureLedger) {
	if s != nil {
		s.cardFailures = l
	}
}

func (s *CheckoutAttemptService) guardCardAttempt(ctx context.Context, user *UserIdentity) error {
	if s == nil || s.cardFailures == nil || user == nil {
		return nil
	}
	id, ok := merchant.FromContext(ctx)
	if !ok || id.IsZero() {
		return nil
	}
	if wait, blocked := s.cardFailures.Blocked(ctx, id.UUID(), abuse.CustomerSubject(user.ID), abuse.AddressSubject(user.ClientIP)); blocked {
		return &CardAttemptsBlockedError{RetryAfter: wait}
	}
	return nil
}

// noteCardAttempt counts a declined card against the customer, their client
// address and the merchant.
func (s *CheckoutAttemptService) noteCardAttempt(ctx context.Context, user *UserIdentity) {
	if s == nil || s.cardFailures == nil || user == nil {
		return
	}
	id, ok := merchant.FromContext(ctx)
	if !ok || id.IsZero() {
		return
	}
	s.cardFailures.Record(ctx, id.UUID(), abuse.CustomerSubject(user.ID), abuse.AddressSubject(user.ClientIP), abuse.MerchantSubject)
}

// cardAttemptFailed reports a provider refusal, never a transport ambiguity.
func cardAttemptFailed(err error) bool {
	var pmErr *paymentmethods.PaymentMethodError
	if errors.As(err, &pmErr) {
		return true
	}
	return false
}

// checkoutPage reports an order page of the hosted checkout (/c/...): a
// redirect PSP or bank challenge started there comes back inside it, framed
// or not.
func (s *CheckoutAttemptService) checkoutPage(raw string) bool {
	if s.checkoutOrigin == nil {
		return false
	}
	origin, ok := config.URLOrigin(raw)
	u, err := url.Parse(strings.TrimSpace(raw))
	return ok && err == nil && u.User == nil && origin != "" && origin == s.checkoutOrigin() && strings.HasPrefix(u.Path, "/c/")
}

// validateReturnURLs refuses redirect targets outside the host's allowed
// return origins.
func (s *CheckoutAttemptService) validateReturnURLs(urls ...string) error {
	for _, raw := range urls {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		if s == nil || !config.ReturnURLAllowed(s.config, raw) && !s.checkoutPage(raw) {
			return fmt.Errorf("%w: return URL origin is not allowed", ErrCheckoutAttemptValidation)
		}
	}
	return nil
}
