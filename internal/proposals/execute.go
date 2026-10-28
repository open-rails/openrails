package proposals

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/reconcile"
)

// apply executes the change's kind and answers what it did. A change whose
// write finds nothing left to do is superseded: the state moved under it.
func (x *execution) apply(ctx context.Context) (map[string]any, error) {
	c := x.change()
	a := c.Apply
	writer := &reconcile.PGLocalWriter{DB: x.d, Now: func() time.Time { return x.now }}
	switch c.Kind {
	case billing.ChangeCreateSubscription:
		if a.Materialize == nil {
			break
		}
		res, err := writer.MaterializeSubscription(ctx, *a.Materialize)
		if err != nil {
			return nil, err
		}
		if !res.Created {
			return nil, superseded{"already_recorded: the provider subscription is recorded"}
		}
		return map[string]any{"subscription_id": billing.SubscriptionID(res.SubscriptionID).String(),
			"access_granted": res.AccessGranted, "payment_recorded": res.PaymentBackfilled}, nil

	case billing.ChangeRecordCharge:
		if a.BackfillPayment == nil {
			break
		}
		recorded, err := writer.BackfillPayment(ctx, *a.BackfillPayment)
		if err != nil {
			return nil, err
		}
		if !recorded {
			return nil, superseded{"already_recorded: the charge is recorded"}
		}
		return x.recordedPayment(ctx, a.BackfillPayment.TransactionID, "payment_id")

	case billing.ChangeRecordRefund:
		if a.RecordRefund == nil {
			break
		}
		recorded, err := writer.RecordRefund(ctx, *a.RecordRefund)
		if err != nil {
			return nil, err
		}
		if !recorded {
			return nil, superseded{"already_recorded: the refund is recorded"}
		}
		return map[string]any{"refund_recorded": true}, nil

	case billing.ChangeAdoptSubscription, billing.ChangeCancelSubscription:
		if a.Decide == nil {
			break
		}
		return x.decide(ctx, a.Decide)

	case billing.ChangeUpdateCard:
		if a.AdoptPaymentMethod == nil {
			break
		}
		adopt := *a.AdoptPaymentMethod
		adopt.EventRef = "proposal:" + x.finding.ID.String()
		changed, err := writer.AdoptPaymentMethod(ctx, adopt)
		if err != nil {
			return nil, err
		}
		if !changed {
			return nil, superseded{"card_moved: the card already holds the provider's details"}
		}
		return map[string]any{"payment_method_id": billing.PaymentMethodID(adopt.PaymentMethodID).String(), "card_version": adopt.EventRef}, nil

	case billing.ChangeRevokeAccess:
		if a.RevokeAccess == nil {
			break
		}
		return x.revokeAccess(ctx, a.RevokeAccess)

	case billing.ChangeRefundChargeAfterCancel:
		if a.RefundCharge == nil {
			break
		}
		return x.refundCharge(ctx, a.RefundCharge)

	case billing.ChangeCancelProviderSchedule:
		if a.CancelSchedule == nil {
			break
		}
		return x.cancelSchedule(ctx, a.CancelSchedule.SubscriptionID)

	case billing.ChangeCollectSkippedRenewal:
		if a.CollectRenewal == nil {
			break
		}
		return x.collectRenewal(ctx, a.CollectRenewal.SubscriptionID)

	case billing.ChangeCorrectSubscription:
		if a.Correct == nil {
			break
		}
		return x.correct(ctx, a.Correct)
	}
	return nil, fmt.Errorf("a %s change carries nothing to execute", c.Kind)
}

func (x *execution) clock() clockwork.Clock { return clockwork.NewFakeClockAt(x.now) }

func (x *execution) lifecycle() *subscriptions.SubscriptionLifecycleService {
	lc := subscriptions.NewSubscriptionLifecycleService(x.e.DB, nil, nil, nil, nil, nil, x.clock())
	lc.SetConfig(x.e.Config)
	return lc
}

// capture records the subscription and its live access before the change
// writes them, so undo-run can put them back; seal attributes the provider
// writes the change queued for it to the run.
func (x *execution) capture(ctx context.Context, subID uuid.UUID, write func() (map[string]any, error)) (map[string]any, error) {
	recorder := &reconcile.PGDestructiveRunRecorder{DB: x.d}
	since, err := recorder.CaptureSubscription(ctx, x.run, subID)
	if err != nil {
		return nil, err
	}
	out, err := write()
	if err != nil {
		return nil, err
	}
	intents, err := recorder.Seal(ctx, x.run, subID, since)
	if err != nil {
		return nil, err
	}
	x.count("subscriptions", 1)
	x.count("provider_intents", intents)
	return out, nil
}

func (x *execution) count(table string, n int) {
	if n == 0 {
		return
	}
	if x.affected == nil {
		x.affected = map[string]int{}
	}
	x.affected[table] += n
}

// decide runs the decider's transition: a cancel ends access with no
// provider write; the others adopt what the provider showed.
func (x *execution) decide(ctx context.Context, d *reconcile.DecideAction) (map[string]any, error) {
	return x.capture(ctx, d.SubscriptionID, func() (map[string]any, error) {
		sub, err := subscriptions.NewSubscriptionRepo(x.d).GetByIDForUpdate(ctx, d.SubscriptionID)
		if err != nil {
			return nil, err
		}
		// holdFor admitted this approved change against the kill switch.
		applied, err := reconcile.ApplyDecision(reconcile.WithAdmittedCancels(ctx), x.d, x.lifecycle(), sub, d.Decision, x.now)
		if err != nil {
			return nil, err
		}
		if !applied {
			return nil, superseded{"subscription_moved: the decider found nothing to apply"}
		}
		after, err := subscriptions.NewSubscriptionRepo(x.d).GetByID(ctx, d.SubscriptionID)
		if err != nil {
			return nil, err
		}
		return map[string]any{"subscription_id": billing.SubscriptionID(d.SubscriptionID).String(),
			"transition": d.Decision.Kind.String(), "status": string(after.Status)}, nil
	})
}

// revokeAccess ends the subscription's live access now.
func (x *execution) revokeAccess(ctx context.Context, r *reconcile.RevokeAccessAction) (map[string]any, error) {
	return x.capture(ctx, r.SubscriptionID, func() (map[string]any, error) {
		ents := entitlements.NewEntitlementService(x.d, x.clock())
		if err := ents.RevokeSourcesForSubscriptionAsOf(ctx, r.CustomerID.String(), r.SubscriptionID, x.now, models.AccessRevokeChargeback,
			models.AccessSourceSubscription, models.AccessSourceGrace); err != nil {
			return nil, err
		}
		return map[string]any{"subscription_id": billing.SubscriptionID(r.SubscriptionID).String(), "access": "revoked"}, nil
	})
}

// refundCharge queues the full refund of the recorded charge, keyed by the
// proposal so approving it again can never refund twice.
func (x *execution) refundCharge(ctx context.Context, r *reconcile.RefundChargeAction) (map[string]any, error) {
	paymentID, err := x.q.ProposalPaymentID(ctx, gen.ProposalPaymentIDParams{MerchantID: x.mid, PspID: x.psp, TransactionID: r.TransactionID})
	if db.IsNotFound(err) {
		return nil, fmt.Errorf("charge %s is not recorded: approve its record_charge proposal first", r.TransactionID)
	}
	if err != nil {
		return nil, err
	}
	payment, err := payments.NewPaymentService(x.d, x.clock()).GetByID(ctx, paymentID)
	if err != nil {
		return nil, err
	}
	write := func() (map[string]any, error) {
		key, err := intents.QueueChargeAfterCancelRefund(ctx, x.d, payment, intents.RefundIdempotencyKey(payment.ID, "proposal:"+x.finding.ID.String()), x.clock())
		if err != nil {
			return nil, err
		}
		out := map[string]any{"payment_id": billing.PaymentID(payment.ID).String()}
		if key == "" {
			out["refund"] = "not_queued: the rail cannot refund automatically or nothing remains"
		} else {
			out["refund"] = "queued"
			out["refund_intent_idempotency_key"] = key
		}
		return out, nil
	}
	if payment.SubscriptionID == nil {
		return write()
	}
	return x.capture(ctx, *payment.SubscriptionID, write)
}

// cancelSchedule queues the provider cancel of the ended subscription's
// schedule, as the person who approved it asked.
func (x *execution) cancelSchedule(ctx context.Context, subID uuid.UUID) (map[string]any, error) {
	return x.capture(ctx, subID, func() (map[string]any, error) {
		sub, err := subscriptions.NewSubscriptionRepo(x.d).GetByIDForUpdate(ctx, subID)
		if err != nil {
			return nil, err
		}
		if sub.Status.Live() {
			return nil, superseded{"subscription_moved: the subscription is live again"}
		}
		reason := "approved proposal " + billing.FindingID(x.finding.ID).String()
		if err := intents.NewProviderCancelScheduler(x.d, nil, intents.OriginAdmin, reason).ScheduleProviderCancel(ctx, sub, x.now); err != nil {
			return nil, err
		}
		return map[string]any{"subscription_id": billing.SubscriptionID(subID).String(), "provider_cancel": "queued"}, nil
	})
}

// collectRenewal queues the collection of the renewal the provider skipped.
func (x *execution) collectRenewal(ctx context.Context, subID uuid.UUID) (map[string]any, error) {
	return x.capture(ctx, subID, func() (map[string]any, error) {
		sub, err := subscriptions.NewSubscriptionRepo(x.d).GetByIDForUpdate(ctx, subID)
		if err != nil {
			return nil, err
		}
		collected, err := x.lifecycle().CollectSkippedRenewal(ctx, x.d, sub)
		if err != nil {
			return nil, err
		}
		if !collected {
			return nil, superseded{"subscription_moved: the renewal is no longer skipped"}
		}
		return map[string]any{"subscription_id": billing.SubscriptionID(subID).String(), "collection": "queued"}, nil
	})
}

// correct adopts the provider's facts on the held subscription: its mapped
// price (a move to another product ends the old one's grace), its start date
// and its recurring record as the accepted schedule.
func (x *execution) correct(ctx context.Context, c *reconcile.CorrectSubscriptionAction) (map[string]any, error) {
	return x.capture(ctx, c.SubscriptionID, func() (map[string]any, error) {
		repo := subscriptions.NewSubscriptionRepo(x.d)
		sub, err := repo.GetByIDForUpdate(ctx, c.SubscriptionID)
		if err != nil {
			return nil, err
		}
		out := map[string]any{"subscription_id": billing.SubscriptionID(sub.ID).String()}
		if c.PriceID != nil && *c.PriceID != sub.PriceID {
			// A change scheduled since the pull is its renewal's or a person's.
			if pending, err := subscriptions.PendingChange(ctx, x.d, sub.ID); err != nil || pending != nil {
				if err != nil {
					return nil, err
				}
				return nil, superseded{"scheduled_change_pending"}
			}
			row, err := x.q.GetPriceByID(ctx, gen.GetPriceByIDParams{MerchantID: x.mid, ID: *c.PriceID})
			if err != nil {
				return nil, err
			}
			price, err := models.PriceFromGen(row)
			if err != nil {
				return nil, err
			}
			if err := subscriptions.AdoptProviderPrice(ctx, x.d, sub, price, x.now, x.clock()); err != nil {
				return nil, err
			}
			out["price_id"] = row.PublicID
		}
		if c.StartedAt != nil {
			if err := x.q.CorrectSubscriptionStart(ctx, gen.CorrectSubscriptionStartParams{MerchantID: x.mid, ID: sub.ID, StartedAt: c.StartedAt.UTC(), Now: x.now}); err != nil {
				return nil, err
			}
			out["started_at"] = c.StartedAt.UTC().Format(time.RFC3339)
		}
		if c.Schedule != nil {
			if sub, err = repo.GetByIDForUpdate(ctx, sub.ID); err != nil {
				return nil, err
			}
			price, err := x.q.GetPriceByID(ctx, gen.GetPriceByIDParams{MerchantID: x.mid, ID: sub.PriceID})
			if err != nil {
				return nil, err
			}
			source, err := reconcile.AdoptProviderSchedule(ctx, x.d, sub, price, *c.Schedule, x.now)
			if errors.Is(err, reconcile.ErrScheduleTerms) {
				return nil, superseded{"schedule_moved: " + err.Error()}
			}
			if err != nil {
				return nil, err
			}
			out["schedule"] = source
		}
		return out, nil
	})
}

func (x *execution) recordedPayment(ctx context.Context, transactionID, key string) (map[string]any, error) {
	id, err := x.q.ProposalPaymentID(ctx, gen.ProposalPaymentIDParams{MerchantID: x.mid, PspID: x.psp, TransactionID: transactionID})
	if err != nil {
		return nil, err
	}
	return map[string]any{key: billing.PaymentID(id).String(), "transaction_id": transactionID}, nil
}

// lockSubscription locks the subject row for the rest of the id's
// transaction, so nothing moves it between the basis check and the write.
func lockSubscription(ctx context.Context, d *db.DB, id uuid.UUID) (*models.Subscription, error) {
	sub, err := subscriptions.NewSubscriptionRepo(d).GetByIDForUpdate(ctx, id)
	if err != nil {
		if db.IsNotFound(err) {
			return nil, superseded{"subscription_gone: the subscription no longer exists"}
		}
		return nil, err
	}
	return sub, nil
}
