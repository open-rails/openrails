// Package dunning is the OpenRails-driven subscription rebill machinery shared
// by the scheduled dunning worker and the customer's retry-now (#809): one
// order reference per dunned period, one durable manual_rebill operation per
// attempt ordinal, one decline doctrine.
package dunning

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/destructive"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/collection"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/normalize"
)

// AttemptLease is how long one rebill attempt holds the subscription's
// dunning schedule (next_retry_at) while it executes.
const AttemptLease = 15 * time.Minute

// Lifecycle is the subscription transition the decline doctrine applies.
type Lifecycle interface {
	FailMembership(context.Context, *subscriptions.FailMembershipParams) error
}

// OrderReference is the provider order id every rebill attempt for the
// subscription's current period carries; the verifier correlates by it.
func OrderReference(sub *models.Subscription) string {
	if sub == nil || sub.CurrentPeriodEndsAt == nil {
		return ""
	}
	return fmt.Sprintf("rebill-%s-%d", sub.ID, sub.CurrentPeriodEndsAt.UTC().Unix())
}

// AttemptOrdinal is the subscription's recorded failure count: the ordinal
// the next rebill operation is keyed on.
func AttemptOrdinal(sub *models.Subscription) int {
	if sub == nil || sub.RetryAttempts == nil {
		return 0
	}
	return *sub.RetryAttempts
}

// Decline is what a terminally failed rebill operation said.
type Decline struct {
	Reason       string
	ResponseCode int
	// FailureCode is the response code as text, "" when the gateway gave none.
	FailureCode string
	Outcome     collection.DeclineOutcome
}

// DeclineOf reads the decline off a terminal manual_rebill operation.
func DeclineOf(rail models.Rail, intent gen.OpenrailsRailIntent) Decline {
	d := Decline{Reason: normalize.FromPtr(intent.LastFailureReason), ResponseCode: evidenceResponseCode(intent)}
	if d.Reason == "" {
		d.Reason = "rebill declined"
	}
	if d.ResponseCode != 0 {
		d.FailureCode = fmt.Sprintf("%d", d.ResponseCode)
	}
	d.Outcome = collection.ClassifyDeclineDetail(string(rail), d.FailureCode).Outcome
	return d
}

// ApplyDecline applies the or#870 decline doctrine for a terminally failed
// rebill, once per attempt ordinal: ONE classifier, three outcomes. Bucket 1 keeps the schedule (the
// failure count advances and the next retry is scheduled), bucket 2 stops
// charging without terminating, bucket 3 cancels at the rail — through the
// certainty and kill-switch gates, which park the row instead. The declined
// attempt is recorded as a failed payment.
func ApplyDecline(ctx context.Context, database *db.DB, lifecycle Lifecycle, sub *models.Subscription, rail models.Rail, intent gen.OpenrailsRailIntent) (Decline, error) {
	decline := DeclineOf(rail, intent)
	var failureCode *string
	if decline.FailureCode != "" {
		code := decline.FailureCode
		failureCode = &code
	}
	declineClass := collection.ClassifyDeclineDetail(string(rail), decline.FailureCode)
	collection.AlertUnmappedDecline(ctx, declineClass)

	certainty := ""
	if decline.Outcome == collection.DeclineNonRecoverable {
		certainty = collection.CertaintyNonRetryableDecline
	}
	blocked := ""
	if v := destructive.New(database).Check(ctx, sub.MerchantID); !v.Allowed {
		blocked = v.Reason
	}
	entry := log.WithContext(ctx).WithFields(log.Fields{
		"subscription_id": sub.ID, "intent_id": intent.ID, "response_code": decline.ResponseCode, "reason": decline.Reason,
		"decline_outcome": decline.Outcome.String(), "decline_coverage": declineClass.Coverage.String(),
		"terminal_certainty": certainty, "terminal_blocked": blocked,
	})
	switch {
	case blocked != "":
		entry.Warn("Dunning: decline recorded but destructive actions are gated; no terminal cancellation will execute — " + blocked)
	case decline.Outcome == collection.DeclineNonRecoverable:
		entry.Error("Dunning: non-recoverable decline (or#870 bucket 3); cancelling the subscription at the rail — the stored payment method is NOT touched")
	case decline.Outcome == collection.DeclineFixPaymentMethod:
		entry.Warn("Dunning: customer's card needs fixing (or#870 bucket 2); charging STOPS, subscription and entitlements retained, update-payment-method notice sent")
	default:
		entry.Warn("Dunning: retryable decline (or#870 bucket 1); will retry on schedule")
	}
	reason := decline.Reason
	// The operation's own period and ordinal make a second observer of this
	// decline — or a replay after the subscription moved to a later period —
	// a no-op, checked inside FailMembership's row lock.
	var forAttempt *int
	var forPeriodEnd *time.Time
	var payload intents.ManualRebillPayload
	if len(intent.Payload) > 0 && json.Unmarshal(intent.Payload, &payload) == nil {
		forAttempt, forPeriodEnd = &payload.Attempt, &payload.PeriodEnd
	}
	if err := lifecycle.FailMembership(ctx, &subscriptions.FailMembershipParams{
		Rail:                rail,
		SubscriptionID:      &sub.ID,
		FailureReason:       &reason,
		FailureCode:         failureCode,
		Decline:             decline.Outcome,
		RecordFailedAttempt: true,
		TerminalCertainty:   certainty,
		TerminalBlocked:     blocked,
		ForAttempt:          forAttempt,
		ForPeriodEnd:        forPeriodEnd,
	}); err != nil {
		return decline, fmt.Errorf("apply failure policy after declined rebill: %w", err)
	}
	return decline, nil
}

// evidenceResponseCode reads the gateway decline code off the operation's
// result_evidence (0 when absent — classified soft).
func evidenceResponseCode(intent gen.OpenrailsRailIntent) int {
	if len(intent.ResultEvidence) == 0 {
		return 0
	}
	var evidence struct {
		ResponseCode json.Number `json:"response_code"`
	}
	decoder := json.NewDecoder(bytes.NewReader(intent.ResultEvidence))
	decoder.UseNumber()
	if err := decoder.Decode(&evidence); err != nil {
		return 0
	}
	code, err := evidence.ResponseCode.Int64()
	if err != nil {
		return 0
	}
	return int(code)
}

// ReleaseClaim releases the rebill attempt claim holder took. The schedule
// was never moved, so a row that is still due is picked up again as is.
func ReleaseClaim(ctx context.Context, database *db.DB, merchantID, subscriptionID uuid.UUID, holder string) error {
	if _, err := database.Gen(ctx).ReleaseDunningClaim(ctx, gen.ReleaseDunningClaimParams{ID: subscriptionID, MerchantID: merchantID, Holder: holder}); err != nil {
		return fmt.Errorf("release dunning claim: %w", err)
	}
	return nil
}
