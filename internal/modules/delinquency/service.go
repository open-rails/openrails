package delinquency

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

// Host-lifecycle event types emitted on a delinquency transition. One per
// destination state; `from_state` in the payload carries where it came from, so
// a host that only cares about the cutoff can filter on the type alone.
const (
	EventGrace      = "delinquency.grace"
	EventDelinquent = "delinquency.entered"
	EventCleared    = "delinquency.cleared"

	subjectCustomer = "customer"
)

// eventTypeFor maps a destination state onto the signal the host consumes.
func eventTypeFor(s State) string {
	switch s {
	case StateDelinquent:
		return EventDelinquent
	case StateGrace:
		return EventGrace
	default:
		return EventCleared
	}
}

// PassBatch caps how many (payer, currency) pairs one merchant's pass examines
// per leg. The enter leg is oldest-debt-first, so the most urgent payers go
// first and the rest wait for the next pass.
const PassBatch = 5000

// Transition is one observed state change, already recorded and signalled.
type Transition struct {
	CustomerID uuid.UUID
	Currency   string
	From       State
	To         State
}

// PassResult reports what one merchant's evaluation pass did.
type PassResult struct {
	Evaluated   int
	Transitions []Transition
}

// Service evaluates, stores and signals delinquency. Every method expects a
// merchant-scoped context (MerchantTx / RunInMerchantScope); there is no
// deployment-wide read of anyone's debt here.
type Service struct {
	db    *db.DB
	clock clockwork.Clock
}

// NewService builds the delinquency service over a merchant-scoped DB.
func NewService(database *db.DB, clock clockwork.Clock) *Service {
	return &Service{db: database, clock: clock}
}

func (s *Service) now() time.Time {
	if s == nil || s.clock == nil {
		return time.Now().UTC()
	}
	return s.clock.Now().UTC()
}

// Policy resolves the merchant's declared delinquency policy. The amount floor
// derives from the invoice monthly floor unless arrears_delinquency_floor
// overrides it.
func (s *Service) Policy(ctx context.Context) (Policy, error) {
	if s == nil || s.db == nil {
		return Policy{}, fmt.Errorf("delinquency service not initialized")
	}
	cfg, _, err := merchantconfig.NewStore(s.db).Get(ctx)
	if err != nil {
		return Policy{}, err
	}
	return PolicyFromConfig(cfg)
}

// PolicyFromConfig resolves a policy from a merchant configuration document.
func PolicyFromConfig(cfg models.MerchantConfiguration) (Policy, error) {
	p := Policy{GraceDays: DefaultGraceDays, AmountFloor: DefaultAmountFloor}
	if cfg.ArrearsGraceDays != nil {
		p.GraceDays = *cfg.ArrearsGraceDays
	}
	switch {
	case cfg.ArrearsDelinquencyFloor != nil:
		p.AmountFloor = *cfg.ArrearsDelinquencyFloor
	case cfg.InvoiceMonthlyFloor != nil:
		p.AmountFloor = *cfg.InvoiceMonthlyFloor
	}
	if err := p.Validate(); err != nil {
		return Policy{}, err
	}
	return p, nil
}

// Evaluate runs one merchant's delinquency pass: classify every payer with due
// work, record the transitions, and emit the signal for each. Candidates come
// from two indexed scans, never a roster: payers with an overdue open
// receivable (enter), and payers already parked non-current (exit, the only
// way a settled debt is noticed). Anyone else never gets a row.
func (s *Service) Evaluate(ctx context.Context, now time.Time) (PassResult, error) {
	var out PassResult
	if s == nil || s.db == nil {
		return out, fmt.Errorf("delinquency service not initialized")
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return out, err
	}
	if now.IsZero() {
		now = s.now()
	}
	now = now.UTC()
	policy, err := s.Policy(ctx)
	if err != nil {
		return out, err
	}

	// Pinned explicitly; reentrant, so the worker's outer scope stands.
	var overdue []gen.ListOverdueInvoiceAggregatesRow
	var parked []gen.BillingCustomerDelinquency
	if err := s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		q := s.db.Gen(ctx)
		var qErr error
		if overdue, qErr = q.ListOverdueInvoiceAggregates(ctx, gen.ListOverdueInvoiceAggregatesParams{
			MerchantID: tid.UUID(), Now: now, RowLimit: PassBatch,
		}); qErr != nil {
			return fmt.Errorf("list overdue aggregates: %w", qErr)
		}
		if parked, qErr = q.ListNonCurrentDelinquency(ctx, gen.ListNonCurrentDelinquencyParams{
			MerchantID: tid.UUID(), RowLimit: PassBatch,
		}); qErr != nil {
			return fmt.Errorf("list parked payers: %w", qErr)
		}
		return nil
	}); err != nil {
		return out, fmt.Errorf("delinquency: %w", err)
	}

	var errs []error
	type key struct {
		customer uuid.UUID
		currency string
	}
	type candidate struct {
		exposure Exposure
		// policy is the payer's effective policy: the merchant's with the bound
		// billing policy's overrides applied.
		policy Policy
	}
	candidates := make(map[key]candidate, len(overdue)+len(parked))
	for _, r := range overdue {
		effective, perr := policy.withOverrides(int(r.GraceDays), r.AmountFloor)
		if perr != nil {
			errs = append(errs, fmt.Errorf("payer %s/%s: %w", r.CustomerID, r.Currency, perr))
			continue
		}
		candidates[key{r.CustomerID, r.Currency}] = candidate{
			exposure: Exposure{
				OverdueStartedAt: r.OverdueStartedAt.UTC(),
				OverdueAmount:    r.OverdueAmount,
				OverdueInvoices:  int(r.OverdueInvoices),
			},
			policy: effective,
		}
	}
	for _, r := range parked {
		// A parked payer absent from the overdue scan owes nothing: zero
		// exposure classifies `current` under any policy.
		if _, ok := candidates[key{r.CustomerID, r.Currency}]; !ok {
			candidates[key{r.CustomerID, r.Currency}] = candidate{policy: policy}
		}
	}

	for k, c := range candidates {
		exposure := c.exposure
		transition, changed, err := s.apply(ctx, tid, c.policy, k.customer, k.currency, exposure, now)
		if err != nil {
			// One payer's failure must not abort the merchant's pass.
			errs = append(errs, fmt.Errorf("payer %s/%s: %w", k.customer, k.currency, err))
			continue
		}
		out.Evaluated++
		if changed {
			out.Transitions = append(out.Transitions, transition)
		}
	}
	return out, errors.Join(errs...)
}

// apply records one payer's evaluation and signals a state change in the same
// transaction: a stored but unannounced transition is a lost shutoff
// instruction, and an announcement without stored state repeats forever.
func (s *Service) apply(ctx context.Context, tid billing.MerchantID, policy Policy, customerID uuid.UUID, currency string, exposure Exposure, now time.Time) (Transition, bool, error) {
	state := Classify(policy, exposure, now)

	var since *time.Time
	if state != StateCurrent {
		t := exposure.OverdueStartedAt.UTC()
		since = &t
	}

	var transition Transition
	changed := false
	err := s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		row, err := q.UpsertCustomerDelinquency(ctx, gen.UpsertCustomerDelinquencyParams{
			MerchantID:       tid.UUID(),
			CustomerID:       customerID,
			Currency:         currency,
			State:            string(state),
			OverdueStartedAt: since,
			OverdueAmount:    exposure.OverdueAmount,
			OverdueInvoices:  int64(exposure.OverdueInvoices),
			Now:              now,
		})
		if err != nil {
			return err
		}
		// "" = no row before. A first sighting already `current` is not a
		// transition.
		previous := StateCurrent
		if row.PreviousState != "" {
			previous = ParseState(row.PreviousState)
		}
		if previous == ParseState(row.State) {
			return nil
		}
		changed = true
		transition = Transition{CustomerID: customerID, Currency: currency, From: previous, To: ParseState(row.State)}

		payload := map[string]any{
			"from_state":       previous.String(),
			"to_state":         row.State,
			"currency":         currency,
			"overdue_amount":   row.OverdueAmount,
			"overdue_invoices": row.OverdueInvoices,
			"grace_days":       policy.GraceDays,
			"amount_floor":     policy.AmountFloor,
		}
		if row.OverdueStartedAt != nil {
			payload["overdue_started_at"] = row.OverdueStartedAt.UTC().Format(time.RFC3339)
		}
		data, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("encode delinquency event: %w", err)
		}
		// transition_seq makes racing evaluators of one transition collapse
		// into one instruction to the host.
		dedupe := fmt.Sprintf("delinquency:%s:%s:%d", customerID, currency, row.TransitionSeq)
		if _, err := q.EnqueueHostLifecycleEvent(ctx, gen.EnqueueHostLifecycleEventParams{
			MerchantID:  tid.UUID(),
			EventType:   eventTypeFor(ParseState(row.State)),
			SubjectType: subjectCustomer,
			SubjectID:   customerID,
			Currency:    currency,
			OccurredAt:  now,
			Data:        data,
			DedupeKey:   dedupe,
		}); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return Transition{}, false, err
	}
	if changed {
		s.notify(ctx, transition, exposure, now)
	}
	return transition, changed, nil
}

// notify tells the payer on entering and leaving delinquent. Entering grace
// is silent: collection already reported the failed charge. Never fatal: the
// state and host signal are already durable.
func (s *Service) notify(ctx context.Context, t Transition, exposure Exposure, now time.Time) {
	var eventType models.NotificationEventType
	switch {
	case t.To == StateDelinquent:
		eventType = models.NotificationAccountDelinquent
	case t.To == StateCurrent && t.From == StateDelinquent:
		eventType = models.NotificationAccountDelinquencyClosed
	default:
		return
	}
	overdue := exposure.OverdueAmount
	data := billing.NotificationData{
		Currency: t.Currency, OverdueAmount: &overdue, OverdueInvoices: exposure.OverdueInvoices,
		FromState: t.From.String(), ToState: t.To.String(),
	}
	if exposure.Owes() {
		since := exposure.OverdueStartedAt.UTC()
		data.OverdueStartedAt = &since
	}
	if err := subscriptions.NewNotificationQueueRepo(s.db).Create(ctx, &models.NotificationQueue{
		ID:         uuidutil.NewV7(),
		CustomerID: t.CustomerID,
		EventType:  eventType,
		Data:       data,
		CreatedAt:  now,
	}); err != nil {
		log.WithContext(ctx).WithError(err).WithFields(log.Fields{
			"customer_id": t.CustomerID, "currency": t.Currency, "event_type": eventType,
		}).Error("failed to queue delinquency notification")
	}
}

// IsDelinquent is the admission gate's question, answered conservatively: it
// reads the stored state (one primary-key lookup) and refuses only if a live
// recompute against the invoices agrees, so a payer who just settled or was
// never evaluated is never refused. Any error fails open: our malfunction must
// not become the customer's outage.
func (s *Service) IsDelinquent(ctx context.Context, payer identity.CustomerID, currency string) (bool, error) {
	if s == nil || s.db == nil {
		return false, nil
	}
	if payer.IsZero() || currency == "" {
		return false, nil
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return false, err
	}
	delinquent := false
	// Pinned: admission may come from a seam with no merchant connection yet.
	err = s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		q := s.db.Gen(ctx)
		stored, err := q.GetCustomerDelinquency(ctx, gen.GetCustomerDelinquencyParams{
			MerchantID: tid.UUID(), CustomerID: payer.UUID(), Currency: currency,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if ParseState(stored.State) != StateDelinquent {
			return nil
		}

		policy, err := s.Policy(ctx)
		if err != nil {
			return err
		}
		now := s.now()
		live, err := q.GetOverdueInvoiceAggregate(ctx, gen.GetOverdueInvoiceAggregateParams{
			MerchantID: tid.UUID(), CustomerID: payer.UUID(), Currency: currency, Now: now,
		})
		if err != nil {
			return err
		}
		delinquent = Classify(policy, Exposure{
			OverdueStartedAt: live.OverdueStartedAt.UTC(),
			OverdueAmount:    live.OverdueAmount,
			OverdueInvoices:  int(live.OverdueInvoices),
		}, now) == StateDelinquent
		return nil
	})
	return delinquent, err
}
