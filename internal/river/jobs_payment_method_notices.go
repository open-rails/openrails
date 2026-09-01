package riverjobs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/collection"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/progress"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/merchant"
)

const (
	KindPaymentMethodNotices = "openrails.payment_method_notices"

	// paymentMethodNoticeMerchantBatch caps the merchants one pass fans out to.
	// The work queue is indexed on the due predicate, so this bounds a pass by
	// PARKED CUSTOMERS, never by the size of the merchant directory.
	paymentMethodNoticeMerchantBatch = 500
	// paymentMethodNoticeBatch caps one merchant's rungs per pass; the
	// remainder rides the next tick.
	paymentMethodNoticeBatch = 200
)

// PaymentMethodNoticeArgs drives the or#870 bucket-2 notification ladder.
type PaymentMethodNoticeArgs struct{}

func (PaymentMethodNoticeArgs) Kind() string { return KindPaymentMethodNotices }

// PaymentMethodNoticeWorker sends the later rungs of the bucket-2 ladder.
//
// Bucket 2 is the one outcome that deliberately stops charging: the rail says
// this card cannot succeed, so retrying it burns attempts against the issuer
// while the customer could fix it in a minute. Stopping is right — but it also
// removes the only clock the customer was ever on. Nothing else will fire, so
// the reminders have to be durable work.
//
// This worker is notification-only, and that is a load-bearing property, not an
// implementation detail. It cannot cancel a subscription, cannot revoke an
// entitlement, and cannot touch a stored payment method — the doctrine's
// standing rule. Running the ladder out closes the row and changes nothing.
type PaymentMethodNoticeWorker struct {
	river.WorkerDefaults[PaymentMethodNoticeArgs]
	DB     *db.DB
	Clock  clockwork.Clock
	Config interface{ IsProviderReadOnly() bool }
}

func (PaymentMethodNoticeWorker) Kind() string { return KindPaymentMethodNotices }

func (w *PaymentMethodNoticeWorker) now() time.Time {
	if w.Clock != nil {
		return w.Clock.Now()
	}
	return time.Now()
}

func (w *PaymentMethodNoticeWorker) Work(ctx context.Context, job *river.Job[PaymentMethodNoticeArgs]) error {
	logger := log.WithContext(ctx).WithField("worker", KindPaymentMethodNotices)

	// Readonly is a pure observer everywhere else in this package, and a
	// customer email is a side effect on the outside world exactly like a
	// provider write.
	if w.Config != nil && w.Config.IsProviderReadOnly() {
		logger.Warn("Readonly mode: payment-method notice ladder observes only (no rungs sent)")
		return nil
	}

	// or#877's lesson, applied up front: subscriptions and payment_method_notices
	// both FORCE RLS, so a due scan on the bare job context would match
	// `merchant_id = NULL` and return an empty slice with no error — a worker
	// that has never done anything and never said so. Enumerate through the
	// SECURITY DEFINER work queue (ids only; it RAISES if its definer cannot
	// bypass RLS), then send every rung inside the merchant's own scope.
	now := w.now()
	merchantIDs, err := w.DB.GenDirectory().ListDuePaymentMethodNoticeMerchants(ctx, gen.ListDuePaymentMethodNoticeMerchantsParams{
		Now: now, MerchantLimit: paymentMethodNoticeMerchantBatch,
	})
	if err != nil {
		return fmt.Errorf("list merchants with due payment-method notices: %w", err)
	}
	if len(merchantIDs) == 0 {
		logger.Debug("payment-method notice ladder: nothing due")
		return nil
	}

	var sent, recovered, ended, exhausted int
	for _, mid := range merchantIDs {
		if mid == nil {
			continue
		}
		merchantID := merchant.ID(*mid)
		progress.Mark(ctx, "payment method notices merchant "+merchantID.String())
		if err := w.DB.RunInMerchantScope(ctx, merchantID, "payment method notice ladder", func(mctx context.Context) error {
			counts, err := w.sendMerchantRungs(mctx, now)
			sent += counts.sent
			recovered += counts.recovered
			ended += counts.ended
			exhausted += counts.exhausted
			return err
		}); err != nil {
			// One merchant's failure must not abort the rest of the run.
			logger.WithError(err).WithField("merchant_id", merchantID.String()).
				Error("payment-method notice ladder: merchant pass failed; continuing")
		}
	}

	logger.WithFields(log.Fields{
		"merchants": len(merchantIDs),
		"sent":      sent,
		"recovered": recovered,
		"ended":     ended,
		"exhausted": exhausted,
	}).Info("payment-method notice ladder: run completed")
	return nil
}

type noticeCounts struct{ sent, recovered, ended, exhausted int }

// sendMerchantRungs claims this merchant's due rungs and acts on each.
//
// One transaction for the whole batch: the claim takes FOR UPDATE SKIP LOCKED,
// and each row's notification insert commits with its own rung advance. That
// pairing is what makes a rung exactly-once — a crash before commit re-sends
// nothing and loses nothing, and a second worker skips the locked rows rather
// than double-notifying.
func (w *PaymentMethodNoticeWorker) sendMerchantRungs(ctx context.Context, now time.Time) (noticeCounts, error) {
	var counts noticeCounts
	err := w.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		merchantID, err := merchant.Require(ctx)
		if err != nil {
			return err
		}
		due, err := q.ClaimDuePaymentMethodNotices(ctx, gen.ClaimDuePaymentMethodNoticesParams{
			MerchantID: merchantID.UUID(), Now: now, RowLimit: paymentMethodNoticeBatch,
		})
		if err != nil {
			return fmt.Errorf("claim due payment-method notices: %w", err)
		}
		notificationRepo := subscriptions.NewNotificationQueueRepo(db.NewWithPgxTx(tx))
		for _, notice := range due {
			outcome, err := w.applyRung(ctx, q, notificationRepo, notice, now)
			if err != nil {
				return err
			}
			switch outcome {
			case NoticeOutcomeSent:
				counts.sent++
			case NoticeOutcomeExhausted:
				counts.sent++
				counts.exhausted++
			case NoticeOutcomeRecovered:
				counts.recovered++
			case NoticeOutcomeEnded:
				counts.ended++
			}
		}
		return nil
	})
	return counts, err
}

// NoticeOutcome is what one due rung turned into.
type NoticeOutcome int

const (
	NoticeOutcomeSent NoticeOutcome = iota
	// NoticeOutcomeExhausted: the rung was sent AND it was the last one.
	NoticeOutcomeExhausted
	NoticeOutcomeRecovered
	NoticeOutcomeEnded
)

// applyRung decides and applies one ladder row.
//
// The subscription is re-read every time rather than trusted from the park:
// between two rungs the customer may have fixed the card, or the
// provider-verification plane may have resolved the row, and either makes the
// next reminder wrong. Asking someone to update a card they already updated is
// how a recovery email becomes a support ticket.
func (w *PaymentMethodNoticeWorker) applyRung(
	ctx context.Context,
	q *gen.Queries,
	notificationRepo *subscriptions.NotificationQueueRepo,
	notice gen.OpenrailsPaymentMethodNotice,
	now time.Time,
) (NoticeOutcome, error) {
	resolve := func(resolution string) error {
		_, err := q.ResolvePaymentMethodNotice(ctx, gen.ResolvePaymentMethodNoticeParams{
			MerchantID: notice.MerchantID, ID: notice.ID, Resolution: resolution, Now: now,
		})
		return err
	}

	sub, err := q.GetSubscriptionByID(ctx, notice.SubscriptionID)
	if errors.Is(err, pgx.ErrNoRows) {
		// The subscription is gone (purged, or its merchant was). Nothing to
		// ask about.
		return NoticeOutcomeEnded, resolve(subscriptions.NoticeResolutionEnded)
	}
	if err != nil {
		return NoticeOutcomeSent, fmt.Errorf("load subscription for notice ladder: %w", err)
	}

	switch models.SubscriptionStatus(sub.Status) {
	case models.StatusUnknown:
		// Still parked awaiting a card — the one state a rung is for.
	case models.StatusCancelled:
		return NoticeOutcomeEnded, resolve(subscriptions.NoticeResolutionEnded)
	default:
		// active / past_due / anything else: the row left the parked state, so
		// the problem this ladder was about is over.
		return NoticeOutcomeRecovered, resolve(subscriptions.NoticeResolutionRecovered)
	}

	rungsSent := int(notice.RungsSent)
	final := collection.IsFinalPaymentMethodNotice(rungsSent)
	if err := notificationRepo.Create(ctx, &models.NotificationQueue{
		ID:         uuidutil.NewV7(),
		CustomerID: notice.CustomerID,
		EventType:  models.NotificationPaymentMethodUpdateRequired,
		Data: map[string]any{
			"failure_code": derefString(notice.FailureCode),
			"rung":         rungsSent + 1,
			"final":        final,
		},
		CreatedAt: now,
	}); err != nil {
		return NoticeOutcomeSent, fmt.Errorf("queue payment-method notice rung: %w", err)
	}

	var nextAt, resolvedAt *time.Time
	var resolution *string
	outcome := NoticeOutcomeSent
	if next, ok := collection.NextPaymentMethodNoticeAt(rungsSent+1, notice.ParkedAt); ok {
		nextAt = &next
	} else {
		// Last rung. The ladder closes and the subscription stays exactly as it
		// is — parked, entitlements intact, stored payment method untouched.
		// or#870 is emphatic that running out of reminders is not evidence of
		// anything, so it terminates nothing.
		exhausted := subscriptions.NoticeResolutionExhausted
		resolvedAt, resolution, outcome = &now, &exhausted, NoticeOutcomeExhausted
		log.WithContext(ctx).WithFields(log.Fields{
			"subscription_id": notice.SubscriptionID,
			"customer_id":     notice.CustomerID,
			"rungs_sent":      rungsSent + 1,
		}).Warn("or#870 bucket 2: notice ladder exhausted; the subscription stays parked with access intact and the stored payment method untouched — nothing is cancelled")
	}

	rows, err := q.AdvancePaymentMethodNoticeRung(ctx, gen.AdvancePaymentMethodNoticeRungParams{
		MerchantID: notice.MerchantID, ID: notice.ID,
		NextNoticeAt: nextAt, ResolvedAt: resolvedAt, Resolution: resolution, Now: now,
	})
	if err != nil {
		return outcome, fmt.Errorf("advance payment-method notice rung: %w", err)
	}
	if rows != 1 {
		// The claim is held FOR UPDATE, so losing it means something outside
		// this pass rewrote the row. Fail the batch rather than leave a rung
		// notified but not counted — that is how a customer gets the same
		// reminder every tick.
		return outcome, fmt.Errorf("advance payment-method notice rung: claim lost (%d rows)", rows)
	}
	return outcome, nil
}

func derefString(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}
