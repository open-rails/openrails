package checkout

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/modules/abuse"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/pkg/merchant"
)

// CardAttemptsBlockedError refuses a card attempt while the customer is
// blocked by the card-testing ledger (SEC-30).
type CardAttemptsBlockedError struct{ RetryAfter time.Duration }

func (e *CardAttemptsBlockedError) Error() string {
	return fmt.Sprintf("checkout: too many declined card attempts; retry after %s", e.RetryAfter)
}

// SetCardFailureLedger arms card-testing blocks on every checkout path.
func (s *CheckoutSessionService) SetCardFailureLedger(l *abuse.FailureLedger) {
	if s != nil {
		s.cardFailures = l
	}
}

func (s *CheckoutSessionService) guardCardAttempt(ctx context.Context, user *UserIdentity) error {
	if s == nil || s.cardFailures == nil || user == nil {
		return nil
	}
	id, ok := merchant.FromContext(ctx)
	if !ok || id.IsZero() {
		return nil
	}
	wait, blocked, err := s.cardFailures.Blocked(ctx, id.UUID(), abuse.CustomerSubject(user.ID))
	if err != nil {
		return fmt.Errorf("card attempt ledger: %w", err)
	}
	if blocked {
		return &CardAttemptsBlockedError{RetryAfter: wait}
	}
	return nil
}

// noteCardAttempt counts a declined card against the customer and merchant.
func (s *CheckoutSessionService) noteCardAttempt(ctx context.Context, user *UserIdentity, resp *CheckoutSessionResponse, err error) {
	if s == nil || s.cardFailures == nil || user == nil || !CardAttemptFailed(resp, err) {
		return
	}
	id, ok := merchant.FromContext(ctx)
	if !ok || id.IsZero() {
		return
	}
	if rerr := s.cardFailures.Record(ctx, id.UUID(), abuse.CustomerSubject(user.ID), abuse.MerchantSubject); rerr != nil {
		log.WithError(rerr).Error("record card attempt failure")
	}
}

// CardAttemptFailed reports whether a checkout outcome is a refused card.
func CardAttemptFailed(resp *CheckoutSessionResponse, err error) bool {
	var pmErr *paymentmethods.PaymentMethodError
	if errors.As(err, &pmErr) {
		return true
	}
	return err == nil && resp != nil && resp.Status == "failed"
}

// validateReturnURLs refuses redirect targets outside the host's allowed
// return origins (SEC-33).
func (s *CheckoutSessionService) validateReturnURLs(urls ...string) error {
	for _, raw := range urls {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		if s == nil || !s.config.ReturnURLAllowed(raw) {
			return fmt.Errorf("%w: return URL origin is not allowed", ErrCheckoutSessionValidation)
		}
	}
	return nil
}
