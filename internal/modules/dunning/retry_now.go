package dunning

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	identity "github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
)

var (
	ErrSubscriptionNotRetryable             = errors.New("subscription is not retryable now")
	ErrSubscriptionRetryInProgress          = errors.New("a rebill for this period is executing")
	ErrSubscriptionRetryOutcomeUnknown      = errors.New("a submitted rebill has no provider answer yet; nothing is resent until it resolves")
	ErrSubscriptionRetryIdempotencyConflict = errors.New("the idempotency key already names a different retry-now request")
	ErrDunningWindowExpired                 = fmt.Errorf("%w: the missed renewal is older than the dunning window", ErrSubscriptionNotRetryable)
	ErrPaymentRecoveryRailUnsupported       = money.ErrPaymentRecoveryRailUnsupported
	ErrPaymentMethodInvalid                 = money.ErrCollectionPaymentMethodInvalid
	ErrPaymentMethodPSPMismatch             = errors.New("payment method belongs to another provider account")
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

// RequestKey binds one client idempotency key to one payer's retry-now
// requests. It is not bound to the subscription: the same key on another
// subscription, or with another payment method, is a conflict.
func RequestKey(payer identity.CustomerID, clientKey string) string {
	digest := sha256.Sum256([]byte("retry-now\x00" + payer.UUID().String() + "\x00" + clientKey))
	return hex.EncodeToString(digest[:16])
}

// Run rebills the payer's past-due subscription now through its current
// saved method. The operation is the SAME manual_rebill operation the
// dunning worker derives for this period and attempt ordinal, so the two can
// never submit twice for one attempt: whoever enqueues first owns the charge
// and the other observes its durable state. The charge is frozen at enqueue
// (FreezeRebill) and confirmed only by an exact provider receipt. The client
// key is serialized by an advisory lock and bound to its first request (the
// subscription and the named method) through the frozen payload, so the same
// request replays its attempt and a different one is a conflict. The attempt
// holds the worker's own explicit claim (holder + expiry on the database
// clock, never the schedule) until it has an answer; a decline runs the one
// decline doctrine, bound to its period and ordinal.
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
	requestKey := RequestKey(request.Payer, request.IdempotencyKey)

	// A terminal operation whose decline was never applied (a crash between
	// the two) is applied first; that advances the ordinal. It runs outside
	// the row lock below because the lifecycle takes its own.
	if err := r.applyStaleDecline(ctx, repo, mid.UUID(), sub); err != nil {
		return nil, err
	}

	var (
		row      gen.OpenrailsRailIntent
		replayed bool
		claimed  bool
	)
	holder := "retry-now:" + requestKey
	err = r.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		txDB := r.DB.NewWithPgxTx(tx)
		if err := q.LockRecoveryRequestKey(ctx, mid.String()+":retry-now:"+requestKey); err != nil {
			return fmt.Errorf("lock request key: %w", err)
		}
		locked, err := q.GetSubscriptionByIDForUpdate(ctx, sub.ID)
		if err != nil {
			return fmt.Errorf("lock subscription: %w", err)
		}
		prior, err := q.GetManualRebillByRequestKey(ctx, gen.GetManualRebillByRequestKeyParams{MerchantID: mid.UUID(), RequestKey: requestKey})
		switch {
		case err == nil:
			var frozen intents.ManualRebillPayload
			if len(prior.Payload) == 0 || json.Unmarshal(prior.Payload, &frozen) != nil {
				return fmt.Errorf("%w: its operation %s holds no request", ErrSubscriptionRetryIdempotencyConflict, prior.ID)
			}
			if frozen.SubscriptionID != sub.ID || !sameMethod(frozen.RequestPaymentMethodID, request.PaymentMethodID) {
				return ErrSubscriptionRetryIdempotencyConflict
			}
			row, replayed = prior, true
			return nil
		case !errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("load rebill request: %w", err)
		}
		// #657 same-PSP invariant, under the instrument's shared row lock (the
		// #297 custody remap takes it exclusively).
		if locked.PaymentMethodID != nil {
			if _, err := q.GetPaymentMethodForShare(ctx, gen.GetPaymentMethodForShareParams{MerchantID: mid.UUID(), ID: *locked.PaymentMethodID}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("lock payment method: %w", err)
			}
		}
		fresh, err := subscriptions.NewSubscriptionRepo(txDB).GetByID(ctx, sub.ID)
		if err != nil {
			return err
		}
		sub = fresh
		if err := eligible(sub, request.PaymentMethodID); err != nil {
			return err
		}
		// #839: never charge a missed renewal older than the dunning window,
		// read on the database clock.
		now, err := q.DatabaseNow(ctx)
		if err != nil {
			return fmt.Errorf("read database clock: %w", err)
		}
		window, err := Window(ctx, txDB, sub)
		if err != nil {
			return err
		}
		windowEnd := sub.CurrentPeriodEndsAt.UTC().Add(window)
		if !now.Before(windowEnd) {
			return ErrDunningWindowExpired
		}
		payload, err := FreezeRebill(ctx, txDB, sub)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrSubscriptionNotRetryable, err)
		}
		payload.RequestKey, payload.RequestPaymentMethodID = requestKey, request.PaymentMethodID
		key := intents.ManualRebillIdempotencyKey(sub.ID, payload.PeriodEnd, string(sub.Rail), payload.OrderReference, payload.Attempt)
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
				// A never-attempted operation (the worker's, materialized under
				// a limited mode) is adopted; another customer request's is not.
				var other intents.ManualRebillPayload
				if existing.Attempts > 0 || (json.Unmarshal(existing.Payload, &other) == nil && other.RequestKey != "") {
					return ErrSubscriptionRetryInProgress
				}
			}
		}
		_, err = q.ClaimSubscriptionRetryNow(ctx, gen.ClaimSubscriptionRetryNowParams{
			ID: sub.ID, MerchantID: mid.UUID(), CustomerID: sub.CustomerID, Holder: holder, LeaseSeconds: int32(AttemptLease / time.Second),
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: a rebill attempt claim is live", ErrSubscriptionRetryInProgress)
		}
		if err != nil {
			return fmt.Errorf("claim subscription for retry-now: %w", err)
		}
		claimed = true
		row, err = intents.NewStore(txDB).Enqueue(ctx, intents.EnqueueParams{
			MerchantID:     mid.UUID(),
			Provider:       string(sub.Rail),
			IntentType:     intents.TypeManualRebill,
			SubscriptionID: &sub.ID,
			PspID:          sub.PspID,
			Payload:        payload,
			IdempotencyKey: key,
			NextAttemptAt:  now,
			ExpiresAt:      &windowEnd,
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
	if claimed {
		defer func() {
			releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			if err := ReleaseClaim(releaseCtx, r.DB, mid.UUID(), sub.ID, holder); err != nil {
				log.WithContext(ctx).WithError(err).WithField("subscription_id", sub.ID).Warn("retry-now: claim release failed; it expires on its own")
			}
		}()
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
		// Relevance moved on under us (renewed, cancelled, period advanced,
		// or the instrument drifted — its re-check runs under the row lock).
		if current, err := repo.GetByID(ctx, sub.ID); err == nil {
			if err := eligible(current, request.PaymentMethodID); err != nil {
				return nil, err
			}
		}
		return nil, fmt.Errorf("%w: %s", ErrSubscriptionNotRetryable, strings.TrimSpace(normalizeReason(executed.LastFailureReason)))
	case intents.StatusFailedTerminal:
		if _, err := ApplyDecline(ctx, r.DB, r.Lifecycle, sub, sub.Rail, executed); err != nil {
			return nil, err
		}
	}
	return r.result(ctx, repo, sub.ID, sub.Rail, executed, false)
}

func sameMethod(a, b *uuid.UUID) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// eligible is the rail gate and the state the customer surface accepts: a
// past-due subscription on an OpenRails-driven saved-method rail whose
// current method can be charged and was vaulted by the subscription's own
// provider account (#657). It runs before any provider traffic.
func eligible(sub *models.Subscription, paymentMethodID *uuid.UUID) error {
	descriptor, ok := rails.Lookup(sub.Rail)
	if !ok || !money.RecoveryRailSupported(descriptor) || rails.AutoBilled(sub.Rail, sub.PaymentMethod) {
		return fmt.Errorf("%w: rail %q", ErrPaymentRecoveryRailUnsupported, sub.Rail)
	}
	if sub.Status != models.StatusPastDue || sub.CurrentPeriodEndsAt == nil || sub.CurrentPeriodEndsAt.IsZero() {
		return fmt.Errorf("%w: status %s", ErrSubscriptionNotRetryable, sub.Status)
	}
	pm := sub.PaymentMethod
	if pm == nil || strings.TrimSpace(pm.ParkReason) != "" || intents.RebillInstrumentOf(pm).Validate() != nil {
		return fmt.Errorf("%w: the saved payment method cannot be charged", ErrSubscriptionNotRetryable)
	}
	if paymentMethodID != nil && *paymentMethodID != pm.ID {
		return fmt.Errorf("%w: retry-now charges the subscription's current payment method", ErrPaymentMethodInvalid)
	}
	if !subscriptions.PaymentMethodMatchesSubscriptionProvider(pm, sub) {
		return fmt.Errorf("%w: subscription PSP %s, payment method PSP %s; card re-entry on the subscription's provider account is required", ErrPaymentMethodPSPMismatch, sub.PspID, pm.PspID)
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
