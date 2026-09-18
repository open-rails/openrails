package dunning

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	log "github.com/sirupsen/logrus"

	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/timeutil"
	"github.com/open-rails/openrails/pkg/merchant"
)

var (
	ErrSubscriptionNotRetryable        = errors.New("subscription is not retryable now")
	ErrSubscriptionRetryInProgress     = errors.New("a rebill for this period is executing")
	ErrSubscriptionRetryOutcomeUnknown = errors.New("a submitted rebill has no provider answer yet; nothing is resent until it resolves")
	ErrPaymentRecoveryRailUnsupported  = money.ErrPaymentRecoveryRailUnsupported
	ErrPaymentMethodInvalid            = money.ErrCollectionPaymentMethodInvalid
)

// RetryNow is the customer's own retry-now (#809).
type RetryNow struct {
	DB        *db.DB
	Runner    *intents.Runner
	Lifecycle Lifecycle
	Clock     clockwork.Clock
}

type RetryNowRequest struct {
	Payer          identity.CustomerID
	SubscriptionID uuid.UUID
	// PaymentMethodID, when set, must be the subscription's current method.
	PaymentMethodID *uuid.UUID
	IdempotencyKey  string
}

// RetryNowResult is the subscription and the operation as they stand once
// execution returns. Declined is set for a terminal provider refusal;
// TransactionID for a confirmed charge.
type RetryNowResult struct {
	Subscription  *models.Subscription
	Intent        gen.OpenrailsRailIntent
	Replayed      bool
	Declined      *Decline
	TransactionID string
}

// RequestKey binds one client idempotency key to one subscription.
func RequestKey(subscriptionID uuid.UUID, clientKey string) string {
	digest := sha256.Sum256([]byte(subscriptionID.String() + "\x00" + clientKey))
	return hex.EncodeToString(digest[:16])
}

func (r *RetryNow) now() time.Time { return timeutil.FirstClock(r.Clock).Now().UTC() }

// Run rebills the payer's past-due subscription now through its current
// saved method. The operation is the SAME manual_rebill operation the
// dunning worker derives for this period and attempt ordinal, so the two can
// never submit twice for one attempt: whoever enqueues first owns the charge
// and the other observes its durable state. The client key is bound to the
// operation through the frozen payload and looked up under the subscription
// row lock, so two equal requests resolve to one operation and a replay
// answers with the same attempt after the schedule moved on. The lease is
// the worker's own (ClaimSubscriptionRetryNow); a decline runs the one
// decline doctrine.
func (r *RetryNow) Run(ctx context.Context, request RetryNowRequest) (*RetryNowResult, error) {
	if r == nil || r.DB == nil || r.Runner == nil || r.Lifecycle == nil {
		return nil, fmt.Errorf("retry-now not initialized")
	}
	if request.Payer.IsZero() || request.SubscriptionID == uuid.Nil {
		return nil, fmt.Errorf("payer and subscription_id required")
	}
	request.IdempotencyKey = strings.TrimSpace(request.IdempotencyKey)
	if request.IdempotencyKey == "" || len(request.IdempotencyKey) > 255 {
		return nil, fmt.Errorf("idempotency_key must be between 1 and 255 bytes")
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	repo := subscriptions.NewSubscriptionRepo(r.DB)
	sub, err := repo.GetByID(ctx, request.SubscriptionID)
	if err != nil {
		return nil, err
	}
	if sub.CustomerID != request.Payer.UUID() {
		return nil, subscriptions.ErrSubscriptionNotFound
	}
	ctx = db.WithPSPID(ctx, sub.PspID)
	requestKey := RequestKey(sub.ID, request.IdempotencyKey)

	// A terminal operation whose decline was never applied (a crash between
	// the two) is applied first; that advances the ordinal. It runs outside
	// the row lock below because the lifecycle takes its own.
	if err := r.applyStaleDecline(ctx, repo, mid.UUID(), sub); err != nil {
		return nil, err
	}

	var (
		row      gen.OpenrailsRailIntent
		replayed bool
		claimed  *models.Subscription
	)
	err = r.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		if _, err := q.GetSubscriptionByIDForUpdate(ctx, sub.ID); err != nil {
			return fmt.Errorf("lock subscription: %w", err)
		}
		prior, err := q.GetRailIntentByRequestKey(ctx, gen.GetRailIntentByRequestKeyParams{MerchantID: mid.UUID(), IntentType: intents.TypeManualRebill, SubscriptionID: sub.ID, RequestKey: requestKey})
		switch {
		case err == nil:
			row, replayed = prior, true
			return nil
		case !errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("load rebill request: %w", err)
		}
		fresh, err := subscriptions.NewSubscriptionRepo(r.DB.NewWithPgxTx(tx)).GetByID(ctx, sub.ID)
		if err != nil {
			return err
		}
		sub = fresh
		if err := r.eligible(sub, request.PaymentMethodID); err != nil {
			return err
		}
		periodEnd := sub.CurrentPeriodEndsAt.UTC()
		orderReference := OrderReference(sub)
		key := intents.ManualRebillIdempotencyKey(sub.ID, periodEnd, string(sub.Rail), orderReference, AttemptOrdinal(sub))
		existing, err := q.GetRailIntentByIdempotencyKey(ctx, gen.GetRailIntentByIdempotencyKeyParams{MerchantID: mid.UUID(), IdempotencyKey: key})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("load rebill operation: %w", err)
		}
		if err == nil {
			switch existing.Status {
			case intents.StatusUnknownNeedsVerify:
				return ErrSubscriptionRetryOutcomeUnknown
			case intents.StatusInFlight, intents.StatusFailedRetryable, intents.StatusSucceeded, intents.StatusFailedTerminal:
				return ErrSubscriptionRetryInProgress
			case intents.StatusPending:
				if existing.Attempts > 0 {
					return ErrSubscriptionRetryInProgress
				}
			}
		}
		now := r.now()
		leaseUntil := now.Add(AttemptLease)
		n, err := q.ClaimSubscriptionRetryNow(ctx, gen.ClaimSubscriptionRetryNowParams{ID: sub.ID, MerchantID: mid.UUID(), CustomerID: sub.CustomerID, ClaimedAt: now, LeaseUntil: leaseUntil})
		if err != nil {
			return fmt.Errorf("claim subscription for retry-now: %w", err)
		}
		if n == 0 {
			return fmt.Errorf("%w: the dunning schedule holds this subscription", ErrSubscriptionRetryInProgress)
		}
		sub.LastRetryAt, sub.NextRetryAt = &now, &leaseUntil
		claimed = sub
		row, err = intents.NewStore(r.DB.NewWithPgxTx(tx)).Enqueue(ctx, intents.EnqueueParams{
			MerchantID:     mid.UUID(),
			Provider:       string(sub.Rail),
			IntentType:     intents.TypeManualRebill,
			SubscriptionID: &sub.ID,
			PspID:          sub.PspID,
			Payload: intents.ManualRebillPayload{
				SubscriptionID: sub.ID, PeriodEnd: periodEnd, Rail: string(sub.Rail), OrderReference: orderReference,
				Attempt: AttemptOrdinal(sub), RequestKey: requestKey,
			},
			IdempotencyKey: key,
			NextAttemptAt:  now,
			Origin:         intents.OriginUser,
			OriginReason:   "customer subscription retry-now",
			Actor:          sub.CustomerID.String(),
		})
		if err != nil {
			return fmt.Errorf("enqueue rebill operation: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// The operation is durable; execute it inline (a replay of a still
	// pending operation gets its execution here, never a second one).
	executed, err := r.Runner.ExecuteByID(ctx, row.ID)
	if err != nil {
		return nil, fmt.Errorf("execute rebill operation: %w", err)
	}
	if replayed {
		if executed.Status == intents.StatusFailedTerminal {
			current, err := repo.GetByID(ctx, sub.ID)
			if err != nil {
				return nil, err
			}
			if _, err := ApplyDecline(ctx, r.DB, r.Lifecycle, current, current.Rail, executed); err != nil {
				return nil, err
			}
		}
		return r.result(ctx, repo, sub.ID, sub.Rail, executed, true)
	}
	switch executed.Status {
	case intents.StatusSuperseded, intents.StatusExpired:
		// Relevance moved on under us (renewed, cancelled, period advanced).
		if releaseErr := ReleaseAttempt(ctx, r.DB, mid.UUID(), claimed); releaseErr != nil {
			log.WithContext(ctx).WithError(releaseErr).WithField("subscription_id", sub.ID).Warn("retry-now: release after superseded rebill failed")
		}
		return nil, fmt.Errorf("%w: %s", ErrSubscriptionNotRetryable, strings.TrimSpace(normalizeReason(executed.LastFailureReason)))
	case intents.StatusFailedTerminal:
		if _, err := ApplyDecline(ctx, r.DB, r.Lifecycle, claimed, sub.Rail, executed); err != nil {
			if releaseErr := ReleaseAttempt(ctx, r.DB, mid.UUID(), claimed); releaseErr != nil {
				err = errors.Join(err, releaseErr)
			}
			return nil, err
		}
	}
	return r.result(ctx, repo, sub.ID, sub.Rail, executed, false)
}

// eligible is the rail gate and the state the customer surface accepts: a
// past-due subscription on an OpenRails-driven saved-method rail whose
// current method can be charged. It runs before any provider traffic.
func (r *RetryNow) eligible(sub *models.Subscription, paymentMethodID *uuid.UUID) error {
	descriptor, ok := rails.Lookup(sub.Rail)
	if !ok || !money.RecoveryRailSupported(descriptor) || rails.AutoBilled(sub.Rail, sub.PaymentMethod) {
		return fmt.Errorf("%w: rail %q", ErrPaymentRecoveryRailUnsupported, sub.Rail)
	}
	if sub.Status != models.StatusPastDue || sub.CurrentPeriodEndsAt == nil || sub.CurrentPeriodEndsAt.IsZero() {
		return fmt.Errorf("%w: status %s", ErrSubscriptionNotRetryable, sub.Status)
	}
	pm := sub.PaymentMethod
	if pm == nil || strings.TrimSpace(pm.RailCustomerRef) == "" || strings.TrimSpace(pm.RailMethodRef) == "" || strings.TrimSpace(pm.ParkReason) != "" {
		return fmt.Errorf("%w: the saved payment method cannot be charged", ErrSubscriptionNotRetryable)
	}
	if paymentMethodID != nil && *paymentMethodID != pm.ID {
		return fmt.Errorf("%w: retry-now charges the subscription's current payment method", ErrPaymentMethodInvalid)
	}
	return nil
}

// applyStaleDecline applies the decline doctrine for the current ordinal's
// operation when it failed terminally but the lifecycle never moved (a crash
// between the two). ApplyDecline is idempotent per ordinal.
func (r *RetryNow) applyStaleDecline(ctx context.Context, repo *subscriptions.SubscriptionRepo, merchantID uuid.UUID, sub *models.Subscription) error {
	if sub.Status != models.StatusPastDue || sub.CurrentPeriodEndsAt == nil || sub.CurrentPeriodEndsAt.IsZero() {
		return nil
	}
	key := intents.ManualRebillIdempotencyKey(sub.ID, sub.CurrentPeriodEndsAt.UTC(), string(sub.Rail), OrderReference(sub), AttemptOrdinal(sub))
	existing, err := r.DB.Gen(ctx).GetRailIntentByIdempotencyKey(ctx, gen.GetRailIntentByIdempotencyKeyParams{MerchantID: merchantID, IdempotencyKey: key})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load rebill operation: %w", err)
	}
	if existing.Status != intents.StatusFailedTerminal {
		return nil
	}
	if _, err := ApplyDecline(ctx, r.DB, r.Lifecycle, sub, sub.Rail, existing); err != nil {
		return err
	}
	fresh, err := repo.GetByID(ctx, sub.ID)
	if err != nil {
		return err
	}
	*sub = *fresh
	return nil
}

func normalizeReason(reason *string) string {
	if reason == nil {
		return "rebill no longer applies"
	}
	return *reason
}

func (r *RetryNow) result(ctx context.Context, repo *subscriptions.SubscriptionRepo, subscriptionID uuid.UUID, rail models.Rail, row gen.OpenrailsRailIntent, replayed bool) (*RetryNowResult, error) {
	sub, err := repo.GetByID(ctx, subscriptionID)
	if err != nil {
		return nil, err
	}
	out := &RetryNowResult{Subscription: sub, Intent: row, Replayed: replayed}
	switch row.Status {
	case intents.StatusSucceeded:
		out.TransactionID = intents.EvidenceString(row, "transaction_id")
	case intents.StatusFailedTerminal:
		decline := DeclineOf(rail, row)
		out.Declined = &decline
	case intents.StatusUnknownNeedsVerify:
		log.WithContext(ctx).WithFields(log.Fields{"subscription_id": sub.ID, "intent_id": row.ID}).
			Warn("retry-now: rebill outcome unknown; the verifier resolves it from provider reads (no further automatic charge for this attempt)")
	}
	return out, nil
}
