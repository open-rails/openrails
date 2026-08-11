// Package business owns the or#910 B2B dunning + budget-alert cycle over the
// or#908 business-profile roster.
//
// DOCTRINE (the or#878 boundary, applied to the invoice ladder): OpenRails
// emits SIGNALS, never shutoffs. The ladder escalates notify-only notices on
// the existing /billing/v1/me/notifications feed (invoice_issued →
// invoice_overdue → invoice_final_notice), and its last rung is a suspension
// RECOMMENDATION — a durable host_lifecycle_events row plus a payer
// notification — because only the host knows what it is running. Budget
// alerts notify on crossed thresholds and NEVER cap: caps are the consumer
// posture's job.
//
// Everything is edge-triggered and deduplicated: per-invoice rungs dedupe on
// deterministic notification ids, the recommendation episode on the profile's
// CAS watermark (recommended_at set once while NULL, cleared once while NOT
// NULL), budget alerts once per (period, threshold). Re-running a pass emits
// nothing new.
package business

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Ladder windows, measured from an invoice's due_at. Deliberately constants
// for now (tensorhub's runcycle shipped with exactly these defaults): a
// merchant-config knob is additive when a merchant actually asks.
const (
	// FinalNoticeAfterDue is how long past due before the final notice.
	FinalNoticeAfterDue = 7 * 24 * time.Hour
	// RecommendSuspensionAfterDue is how long past due before the suspension
	// RECOMMENDATION signal (hosts enforce; OpenRails never revokes access).
	RecommendSuspensionAfterDue = 14 * 24 * time.Hour

	// sweepLookback keeps arrears exposure and budget-alert inputs fresh: the
	// cycle re-rates this much trailing usage (watermarked, so repeat-safe).
	sweepLookback = 31 * 24 * time.Hour

	// invoicePageSize bounds the per-payer dunning scan to the newest page —
	// business rosters are operator-onboarded and invoices are monthly, so a
	// payer with 100 open receivables is already an operator incident.
	invoicePageSize = 100

	subjectCustomer = "customer"

	// Host-lifecycle event types for the recommendation episode.
	EventSuspensionRecommended = "business.suspension_recommended"
	EventSuspensionCleared     = "business.suspension_cleared"
)

// notificationNamespace derives deterministic notification ids so
// CreateIfAbsent collapses replays: uuid5(ns, merchant|customer|kind|key).
var notificationNamespace = uuid.MustParse("6f1a7d3e-908a-4910-b2b0-000000000910")

// Service evaluates the dunning + budget-alert cycle for one merchant at a
// time under a merchant-scoped context.
type Service struct {
	db    *db.DB
	clock clockwork.Clock
}

func NewService(database *db.DB, clock clockwork.Clock) *Service {
	if clock == nil {
		clock = clockwork.NewRealClock()
	}
	return &Service{db: database, clock: clock}
}

// Result summarizes one merchant pass.
type Result struct {
	Payers          int
	NoticesEmitted  int
	Recommendations int
	Clearances      int
	BudgetAlerts    int
}

// EvaluateMerchant runs one pass for the context merchant: flip overdue
// receivables past_due, then per business payer sweep usage freshness, walk
// the invoice ladder, manage the recommendation episode, and fire budget
// alerts. Per-payer failures are joined, never abort the pass.
func (s *Service) EvaluateMerchant(ctx context.Context, now time.Time) (Result, error) {
	var res Result
	if s == nil || s.db == nil {
		return res, fmt.Errorf("business cycle service not initialized")
	}
	now = now.UTC()
	ms := money.NewMoneyService(s.db, s.clock)

	if _, err := ms.MarkInvoicesPastDue(ctx, now); err != nil {
		return res, fmt.Errorf("mark past due: %w", err)
	}
	profiles, err := ms.ListBusinessProfiles(ctx, 0)
	if err != nil {
		return res, err
	}
	var errs []error
	for i := range profiles {
		p := &profiles[i]
		res.Payers++
		if err := s.evaluatePayer(ctx, ms, p, now, &res); err != nil {
			errs = append(errs, fmt.Errorf("payer %s: %w", p.CustomerID.String(), err))
		}
	}
	return res, errors.Join(errs...)
}

func (s *Service) evaluatePayer(ctx context.Context, ms *money.MoneyService, p *money.BusinessProfile, now time.Time, res *Result) error {
	// Exposure freshness: rate trailing usage into pending items so both the
	// credit-line input (GetOutstandingOwed) and budget alerts read live spend.
	if err := ms.SweepUsage(ctx, p.CustomerID, p.Currency, now.Add(-sweepLookback), now); err != nil {
		return fmt.Errorf("sweep usage: %w", err)
	}
	if err := s.runDunning(ctx, ms, p, now, res); err != nil {
		return err
	}
	return s.runBudgetAlerts(ctx, ms, p, now, res)
}

// runDunning walks the payer's open receivables up the notice ladder and
// manages the suspension-recommendation episode from what it finds.
func (s *Service) runDunning(ctx context.Context, ms *money.MoneyService, p *money.BusinessProfile, now time.Time, res *Result) error {
	invoices, _, err := ms.ListInvoices(ctx, p.CustomerID, invoicePageSize, 0)
	if err != nil {
		return fmt.Errorf("list invoices: %w", err)
	}
	anyPastDue := false
	recommendReason := ""
	for i := range invoices {
		inv := &invoices[i]
		if inv.AmountDue <= 0 {
			continue
		}
		if inv.Status != "open" && inv.Status != "past_due" {
			continue
		}
		number := inv.ID.String()
		if inv.InvoiceNumber != nil && *inv.InvoiceNumber != "" {
			number = *inv.InvoiceNumber
		}
		data := map[string]any{
			"invoice_id":     inv.ID.String(),
			"invoice_number": number,
			"amount_due":     inv.AmountDue,
			"currency":       p.Currency,
		}
		if inv.DueAt != nil {
			data["due_at"] = inv.DueAt.UTC().Format(time.RFC3339)
		}
		if err := s.notifyOnce(ctx, p.CustomerID, models.NotificationInvoiceIssued, inv.ID.String(), data, now, res); err != nil {
			return err
		}
		if inv.Status != "past_due" || inv.DueAt == nil {
			continue
		}
		anyPastDue = true
		overdueFor := now.Sub(inv.DueAt.UTC())
		if err := s.notifyOnce(ctx, p.CustomerID, models.NotificationInvoiceOverdue, inv.ID.String(), data, now, res); err != nil {
			return err
		}
		if overdueFor >= FinalNoticeAfterDue {
			final := map[string]any{
				"invoice_id":     inv.ID.String(),
				"invoice_number": number,
				"amount_due":     inv.AmountDue,
				"currency":       p.Currency,
				"suspension_recommendation_at": inv.DueAt.UTC().Add(RecommendSuspensionAfterDue).Format(time.RFC3339),
			}
			if err := s.notifyOnce(ctx, p.CustomerID, models.NotificationInvoiceFinalNotice, inv.ID.String(), final, now, res); err != nil {
				return err
			}
		}
		if overdueFor >= RecommendSuspensionAfterDue && recommendReason == "" {
			recommendReason = fmt.Sprintf("invoice %s unpaid %d days past due", number, int(overdueFor.Hours()/24))
		}
	}

	switch {
	case recommendReason != "" && p.SuspensionRecommendedAt == nil:
		return s.recommendSuspension(ctx, p, recommendReason, now, res)
	case !anyPastDue && p.SuspensionRecommendedAt != nil:
		return s.clearSuspensionRecommendation(ctx, p, now, res)
	}
	return nil
}

// recommendSuspension opens the episode atomically: the CAS watermark, the
// durable host signal and the payer notification commit in one transaction,
// so a crash cannot strand a stamped profile without its signal. The CAS
// (WHERE suspension_recommended_at IS NULL) means exactly one racing
// evaluator emits.
func (s *Service) recommendSuspension(ctx context.Context, p *money.BusinessProfile, reason string, now time.Time, res *Result) error {
	tid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	emitted := false
	err = s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		n, err := q.RecommendBusinessSuspension(ctx, gen.RecommendBusinessSuspensionParams{
			MerchantID: tid.UUID(), CustomerID: p.CustomerID.UUID(),
			Now: now, Reason: reason,
		})
		if err != nil || n == 0 {
			return err // n==0: another evaluator won the edge — emit nothing.
		}
		emitted = true
		data, err := models.ToJSONB(map[string]any{"reason": reason})
		if err != nil {
			return err
		}
		if _, err := q.EnqueueHostLifecycleEvent(ctx, gen.EnqueueHostLifecycleEventParams{
			MerchantID:  tid.UUID(),
			EventType:   EventSuspensionRecommended,
			SubjectType: subjectCustomer,
			SubjectID:   p.CustomerID.UUID(),
			Currency:    p.Currency,
			OccurredAt:  now,
			Data:        data,
			DedupeKey:   fmt.Sprintf("business_suspension:%s:%s:recommended:%d", p.CustomerID.String(), p.Currency, now.Unix()),
		}); err != nil {
			return err
		}
		return nil
	})
	if err != nil || !emitted {
		return err
	}
	res.Recommendations++
	s.notify(ctx, p.CustomerID, models.NotificationBusinessSuspensionRecommended, map[string]any{
		"reason": reason, "currency": p.Currency,
	}, now, res)
	log.WithContext(ctx).WithFields(log.Fields{
		"customer_id": p.CustomerID.String(), "reason": reason,
	}).Warn("Business dunning: suspension RECOMMENDED to host (no access revoked; data retained)")
	return nil
}

// clearSuspensionRecommendation closes the episode: everything past-due is
// settled, so tell the host to reinstate. Same atomicity as the open edge.
func (s *Service) clearSuspensionRecommendation(ctx context.Context, p *money.BusinessProfile, now time.Time, res *Result) error {
	tid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	recommendedAt := ""
	if p.SuspensionRecommendedAt != nil {
		recommendedAt = p.SuspensionRecommendedAt.UTC().Format(time.RFC3339)
	}
	emitted := false
	err = s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		n, err := q.ClearBusinessSuspensionRecommendation(ctx, gen.ClearBusinessSuspensionRecommendationParams{
			MerchantID: tid.UUID(), CustomerID: p.CustomerID.UUID(), Now: now,
		})
		if err != nil || n == 0 {
			return err
		}
		emitted = true
		data, err := models.ToJSONB(map[string]any{"recommended_at": recommendedAt})
		if err != nil {
			return err
		}
		if _, err := q.EnqueueHostLifecycleEvent(ctx, gen.EnqueueHostLifecycleEventParams{
			MerchantID:  tid.UUID(),
			EventType:   EventSuspensionCleared,
			SubjectType: subjectCustomer,
			SubjectID:   p.CustomerID.UUID(),
			Currency:    p.Currency,
			OccurredAt:  now,
			Data:        data,
			DedupeKey:   fmt.Sprintf("business_suspension:%s:%s:cleared:%d", p.CustomerID.String(), p.Currency, now.Unix()),
		}); err != nil {
			return err
		}
		return nil
	})
	if err != nil || !emitted {
		return err
	}
	res.Clearances++
	s.notify(ctx, p.CustomerID, models.NotificationBusinessSuspensionCleared, map[string]any{
		"recommended_at": recommendedAt, "currency": p.Currency,
	}, now, res)
	log.WithContext(ctx).WithField("customer_id", p.CustomerID.String()).
		Info("Business dunning: past-due book settled; suspension recommendation cleared")
	return nil
}

// runBudgetAlerts fires one notice per crossed threshold per calendar-month
// period. Alerts NEVER cap — they are information, not enforcement.
func (s *Service) runBudgetAlerts(ctx context.Context, ms *money.MoneyService, p *money.BusinessProfile, now time.Time, res *Result) error {
	if len(p.BudgetAlertThresholds) == 0 {
		return nil
	}
	pending, err := ms.ListPendingCharges(ctx, p.CustomerID, p.Currency)
	if err != nil {
		return fmt.Errorf("list pending charges: %w", err)
	}
	var spend int64
	for _, item := range pending {
		spend += item.Amount
	}
	period := now.UTC().Format("2006-01")
	for _, threshold := range p.BudgetAlertThresholds {
		if threshold <= 0 || spend < threshold {
			continue
		}
		key := period + ":" + strconv.FormatInt(threshold, 10)
		if err := s.notifyOnce(ctx, p.CustomerID, models.NotificationBudgetAlert, key, map[string]any{
			"period": period, "threshold": threshold, "spend": spend, "currency": p.Currency,
		}, now, res); err != nil {
			return err
		}
	}
	return nil
}

// notifyOnce inserts a feed notification exactly once per (merchant, payer,
// kind, key): the id is deterministic, so CreateIfAbsent collapses replays.
func (s *Service) notifyOnce(ctx context.Context, payer identity.CustomerID, kind models.NotificationEventType, key string, data map[string]any, now time.Time, res *Result) error {
	tid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	id := uuid.NewSHA1(notificationNamespace, []byte(tid.String()+"|"+payer.String()+"|"+string(kind)+"|"+key))
	before, err := s.notificationExists(ctx, id)
	if err != nil {
		return err
	}
	if err := subscriptions.NewNotificationQueueRepo(s.db).CreateIfAbsent(ctx, &models.NotificationQueue{
		ID:         id,
		CustomerID: payer.UUID(),
		EventType:  kind,
		Data:       data,
		CreatedAt:  now,
	}); err != nil {
		return fmt.Errorf("queue %s notification: %w", kind, err)
	}
	if !before {
		res.NoticesEmitted++
		if kind == models.NotificationBudgetAlert {
			res.BudgetAlerts++
		}
	}
	return nil
}

func (s *Service) notificationExists(ctx context.Context, id uuid.UUID) (bool, error) {
	_, err := subscriptions.NewNotificationQueueRepo(s.db).GetByID(ctx, id)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return false, err
}

// notify inserts a non-deduped feed notification. Never fatal: the durable
// state and host signal are already committed, and a lost in-app notice is
// not a reason to re-run a money decision (the or#878 stance).
func (s *Service) notify(ctx context.Context, payer identity.CustomerID, kind models.NotificationEventType, data map[string]any, now time.Time, res *Result) {
	if err := subscriptions.NewNotificationQueueRepo(s.db).Create(ctx, &models.NotificationQueue{
		ID:         uuidutil.NewV7(),
		CustomerID: payer.UUID(),
		EventType:  kind,
		Data:       data,
		CreatedAt:  now,
	}); err != nil {
		log.WithContext(ctx).WithError(err).WithFields(log.Fields{
			"customer_id": payer.String(), "event_type": kind,
		}).Error("failed to queue business dunning notification")
		return
	}
	res.NoticesEmitted++
}
