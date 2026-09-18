package dunning

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"

	"github.com/open-rails/openrails"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/pkg/merchant"
)

// RecoveryView projects a customer's retry-now state onto their subscription.
type RecoveryView struct {
	DB    *db.DB
	Money *money.MoneyService
	Clock clockwork.Clock
}

// Subscription is the retry-now state of one subscription the payer owns:
// what blocks a retry, the schedule's own next attempt, the recorded failures
// this period and the saved methods a rebill may run through.
func (v RecoveryView) Subscription(ctx context.Context, sub *models.Subscription) (*openrails.PaymentRecovery, error) {
	if v.DB == nil || v.Money == nil || sub == nil {
		return nil, fmt.Errorf("recovery view not initialized")
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	q := v.DB.Gen(ctx)
	now, err := q.DatabaseNow(ctx)
	if err != nil {
		return nil, fmt.Errorf("read database clock: %w", err)
	}
	rows, err := v.Money.RecoveryPaymentMethodRows(ctx, identity.CustomerID(sub.CustomerID))
	if err != nil {
		return nil, err
	}
	recovery := &openrails.PaymentRecovery{
		AttemptCount:               AttemptOrdinal(sub),
		CompatiblePaymentMethodIDs: make([]openrails.PaymentMethodID, 0, len(rows)),
	}
	for _, row := range rows {
		// A rebill charges the subscription's own provider account through its
		// vault, so a method on another account or held by a custodian is not
		// one this subscription can be retried on.
		instrument := intents.RebillInstrument{PSPID: row.PspID, Custodian: row.Custodian, CustodianID: row.CustodianID, RailCustomerRef: strings.TrimSpace(row.RailCustomerRef), RailMethodRef: strings.TrimSpace(row.RailMethodRef)}
		if row.PspID == sub.PspID && instrument.Validate() == nil {
			recovery.CompatiblePaymentMethodIDs = append(recovery.CompatiblePaymentMethodIDs, openrails.PaymentMethodID(row.ID))
		}
	}
	pastDue := sub.Status == models.StatusPastDue && sub.CurrentPeriodEndsAt != nil && !sub.CurrentPeriodEndsAt.IsZero()
	if pastDue {
		if failed, err := q.GetLatestFailedPaymentBySubscription(ctx, gen.GetLatestFailedPaymentBySubscriptionParams{MerchantID: mid.UUID(), SubscriptionID: sub.ID}); err == nil {
			recovery.FailureCategory = derefString(failed.FailureReason)
			recovery.LastFailureCode = derefString(failed.FailureCode)
			at := failed.PurchasedAt
			recovery.LastFailedAt = &at
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("load last failed rebill: %w", err)
		}
		live, err := v.liveOperation(ctx, q, mid.UUID(), sub)
		if err != nil {
			return nil, err
		}
		if live != nil {
			recovery.Operation = live
			recovery.AttemptCount++
		}
		// next_retry_at is only ever the schedule: the engine's own next
		// attempt. A live claim is a separate column.
		recovery.NextAttemptAt = sub.NextRetryAt
	}
	windowExpired := false
	if pastDue {
		window, err := Window(ctx, v.DB, sub)
		if err != nil {
			return nil, err
		}
		windowExpired = !now.Before(sub.CurrentPeriodEndsAt.UTC().Add(window))
	}
	recovery.Retryable, recovery.BlockedReason = subscriptionRecoveryEligibility(sub, recovery, now, pastDue, windowExpired)
	return recovery, nil
}

// claimed reports a live rebill attempt claim (a dunning pass or a customer
// retry-now mid-attempt), on the database clock.
func claimed(sub *models.Subscription, now time.Time) bool {
	return sub.DunningClaimedUntil != nil && sub.DunningClaimedUntil.After(now)
}

// liveOperation is the current period's unresolved rebill operation, if any.
func (v RecoveryView) liveOperation(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, sub *models.Subscription) (*openrails.PaymentOperation, error) {
	rows, err := q.ListRailIntentsBySubject(ctx, gen.ListRailIntentsBySubjectParams{MerchantID: merchantID, IntentType: intents.TypeManualRebill, SubscriptionID: sub.ID, RowLimit: 10})
	if err != nil {
		return nil, fmt.Errorf("list rebill operations: %w", err)
	}
	periodEnd := sub.CurrentPeriodEndsAt.UTC().Unix()
	for _, row := range rows {
		var payload intents.ManualRebillPayload
		if len(row.Payload) == 0 || json.Unmarshal(row.Payload, &payload) != nil || payload.PeriodEnd.UTC().Unix() != periodEnd {
			continue
		}
		switch row.Status {
		case intents.StatusPending, intents.StatusInFlight, intents.StatusUnknownNeedsVerify, intents.StatusFailedRetryable:
			return &openrails.PaymentOperation{ID: row.ID, Status: row.Status}, nil
		}
	}
	return nil, nil
}

func subscriptionRecoveryEligibility(sub *models.Subscription, recovery *openrails.PaymentRecovery, now time.Time, pastDue, windowExpired bool) (bool, string) {
	descriptor, ok := rails.Lookup(sub.Rail)
	switch {
	case !pastDue:
		return false, openrails.RecoveryBlockedNotDue
	case !ok || !money.RecoveryRailSupported(descriptor) || rails.AutoBilled(sub.Rail, sub.PaymentMethod):
		return false, openrails.RecoveryBlockedRailUnsupported
	case recovery.Operation != nil && recovery.Operation.Status == intents.StatusUnknownNeedsVerify:
		return false, openrails.RecoveryBlockedOutcomeUnknown
	case recovery.Operation != nil:
		return false, openrails.RecoveryBlockedInProgress
	case claimed(sub, now):
		return false, openrails.RecoveryBlockedInProgress
	case windowExpired:
		return false, openrails.RecoveryBlockedWindowExpired
	case sub.PaymentMethod == nil || strings.TrimSpace(sub.PaymentMethod.ParkReason) != "" ||
		intents.RebillInstrumentOf(sub.PaymentMethod).Validate() != nil:
		return false, openrails.RecoveryBlockedNoPaymentMethod
	case !subscriptions.PaymentMethodMatchesSubscriptionProvider(sub.PaymentMethod, sub):
		return false, openrails.RecoveryBlockedPSPMismatch
	}
	return true, ""
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
