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

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/logx"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/invoices/usage"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

// A change to a usage invoice evaluates its customer in the same transaction:
// paying, voiding or writing one off clears a customer at once. A policy that
// cannot be read never fails the change: the customer is evaluated on the next
// pass instead.
func init() {
	usage.Changed = func(ctx context.Context, d *db.DB, invoice gen.BillingInvoice, now time.Time) error {
		_, _, err := EvaluateTx(ctx, d, invoice.CustomerID, invoice.Currency, now)
		if !errors.Is(err, errPolicyUnavailable) {
			return err
		}
		d.Log.WarnContext(ctx, "Delinquency: evaluation deferred to the next pass", logx.Err(err), "customer_id", invoice.CustomerID)
		return d.Gen(ctx).ScheduleDelinquency(ctx, gen.ScheduleDelinquencyParams{MerchantID: invoice.MerchantID, CustomerID: invoice.CustomerID, Currency: invoice.Currency, Now: now})
	}
}

// errPolicyUnavailable is a customer's policy that could not be read.
var errPolicyUnavailable = errors.New("delinquency policy unavailable")

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

// Transition is one observed state change, already recorded and signalled.
type Transition struct {
	CustomerID uuid.UUID
	Currency   string
	From       State
	To         State
}

// Service evaluates, stores and signals delinquency. Every method expects a
// merchant-scoped context (MerchantTx / RunInMerchantScope).
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

// PolicyFromConfig resolves the policy for debt in a currency from a merchant
// configuration document: its arrears grace and that currency's floor.
func PolicyFromConfig(cfg models.MerchantConfiguration, code string) (Policy, error) {
	p := Policy{GraceDays: DefaultGraceDays, AmountFloor: DefaultAmountFloor(code)}
	if cfg.ArrearsGraceDays != nil {
		p.GraceDays = *cfg.ArrearsGraceDays
	}
	if cfg.UsageBilling != nil {
		for _, c := range cfg.UsageBilling.Currencies {
			if string(c.Currency) == code && c.DelinquencyFloor != nil {
				p.AmountFloor = *c.DelinquencyFloor
			}
		}
	}
	if err := p.Validate(); err != nil {
		return Policy{}, err
	}
	return p, nil
}

// Evaluate evaluates one customer in a currency in its own transaction, on the
// service's clock: the due pass's step.
func (s *Service) Evaluate(ctx context.Context, customerID uuid.UUID, currency string) (Transition, bool, error) {
	if s == nil || s.db == nil {
		return Transition{}, false, fmt.Errorf("delinquency service not initialized")
	}
	var t Transition
	var changed bool
	err := s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		t, changed, err = EvaluateTx(ctx, s.db.NewWithPgxTx(tx), customerID, currency, s.now())
		return err
	})
	return t, changed, err
}

// EvaluateTx evaluates one customer in a currency in the caller's transaction (d
// is bound to it): it classifies the customer's usage invoices against its
// effective policy, records the state with the next instant time alone changes
// it, signals a transition, and tells the customer of each open invoice newly past
// due. It serializes on the customer, as every change to its usage invoices
// does. A customer that never owed and has nothing falling due gets no row.
func EvaluateTx(ctx context.Context, d *db.DB, customerID uuid.UUID, currency string, now time.Time) (Transition, bool, error) {
	tid, err := merchant.Require(ctx)
	if err != nil {
		return Transition{}, false, err
	}
	now = now.UTC()
	q := d.Gen(ctx)
	if _, err := q.LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: tid.UUID(), ID: customerID}); err != nil {
		return Transition{}, false, fmt.Errorf("lock delinquency customer: %w", err)
	}
	policy, exposure, live, err := readExposure(ctx, d, q, tid, customerID, currency, now)
	if err != nil {
		return Transition{}, false, err
	}
	state := Classify(policy, exposure, now)
	next := nextEvaluation(policy, exposure, state, live)
	if state == StateCurrent && next == nil {
		_, err := q.GetCustomerDelinquency(ctx, gen.GetCustomerDelinquencyParams{MerchantID: tid.UUID(), CustomerID: customerID, Currency: currency})
		if errors.Is(err, pgx.ErrNoRows) {
			return Transition{}, false, nil
		}
		if err != nil {
			return Transition{}, false, err
		}
	}
	var since *time.Time
	if state != StateCurrent {
		t := exposure.OverdueStartedAt.UTC()
		since = &t
	}
	row, err := q.UpsertCustomerDelinquency(ctx, gen.UpsertCustomerDelinquencyParams{
		MerchantID:       tid.UUID(),
		CustomerID:       customerID,
		Currency:         currency,
		State:            string(state),
		OverdueStartedAt: since,
		OverdueAmount:    exposure.OverdueAmount,
		OverdueInvoices:  int64(exposure.OverdueInvoices),
		Now:              now,
		NextEvaluationAt: next,
	})
	if err != nil {
		return Transition{}, false, err
	}
	if err := q.NotifyOverdueInvoices(ctx, gen.NotifyOverdueInvoicesParams{MerchantID: tid.UUID(), CustomerID: customerID, Currency: currency, Now: now}); err != nil {
		return Transition{}, false, fmt.Errorf("notify overdue invoices: %w", err)
	}
	// "" = no row before: a first sighting already current is not a transition.
	previous := StateCurrent
	if row.PreviousState != "" {
		previous = ParseState(row.PreviousState)
	}
	if previous == ParseState(row.State) {
		return Transition{}, false, nil
	}
	transition := Transition{CustomerID: customerID, Currency: currency, From: previous, To: ParseState(row.State)}
	// The stored transition and its announcement share the transaction: a
	// stored but unannounced transition is a lost shutoff instruction, and an
	// announcement without stored state repeats forever.
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
		return Transition{}, false, fmt.Errorf("encode delinquency event: %w", err)
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
		return Transition{}, false, err
	}
	if err := notify(ctx, d, transition, exposure, now, uuidutil.DerivedV7(uuidutil.TimeOf(customerID), tid.UUID(), dedupe+":notification")); err != nil {
		return Transition{}, false, err
	}
	// One line per transition: the decision an operator is asked to explain.
	d.Log.InfoContext(ctx, "Delinquency: state transition recorded and signalled to the host", "merchant_id", tid.String(), "customer_id", customerID.String(), "currency", currency, "from_state", transition.From.String(), "to_state", transition.To.String())
	return transition, true, nil
}

// liveExposure is when a customer's next usage invoice falls due; pending is
// false with none.
type liveExposure struct {
	nextDueAt time.Time
	pending   bool
}

// readExposure reads a customer's overdue usage invoices against now and its
// effective policy: the merchant's with the bound billing policy's overrides.
func readExposure(ctx context.Context, d *db.DB, q *gen.Queries, tid billing.MerchantID, customerID uuid.UUID, currency string, now time.Time) (Policy, Exposure, liveExposure, error) {
	settings, err := merchantconfig.NewStore(d).Settings(ctx)
	if err != nil {
		return Policy{}, Exposure{}, liveExposure{}, fmt.Errorf("%w: %w", errPolicyUnavailable, err)
	}
	merchantPolicy, err := PolicyFromConfig(settings.Config, currency)
	if err != nil {
		return Policy{}, Exposure{}, liveExposure{}, fmt.Errorf("%w: %w", errPolicyUnavailable, err)
	}
	row, err := q.GetOverdueInvoiceAggregate(ctx, gen.GetOverdueInvoiceAggregateParams{
		MerchantID: tid.UUID(), CustomerID: customerID, Currency: currency, Now: now, Policies: settings.PoliciesParam(),
	})
	if err != nil {
		return Policy{}, Exposure{}, liveExposure{}, err
	}
	policy, err := merchantPolicy.withOverrides(int(row.GraceDays), row.AmountFloor)
	if err != nil {
		return Policy{}, Exposure{}, liveExposure{}, fmt.Errorf("%w: %w", errPolicyUnavailable, err)
	}
	exposure := Exposure{OverdueStartedAt: row.OverdueStartedAt.UTC(), OverdueAmount: row.OverdueAmount, OverdueInvoices: int(row.OverdueInvoices)}
	return policy, exposure, liveExposure{nextDueAt: row.NextDueAt.UTC(), pending: row.PendingInvoices > 0}, nil
}

// nextEvaluation is the next instant time alone changes state's reading: the
// next usage invoice falling due, or grace running out on a debt over the
// floor. Nil: only a change to the customer's invoices or policy does.
func nextEvaluation(p Policy, e Exposure, state State, live liveExposure) *time.Time {
	var next *time.Time
	if live.pending {
		t := live.nextDueAt
		next = &t
	}
	if state == StateGrace && e.OverdueAmount >= p.AmountFloor {
		if t := e.OverdueStartedAt.Add(p.Grace()); next == nil || t.Before(*next) {
			next = &t
		}
	}
	return next
}

// notify tells the customer on entering and leaving delinquent. Entering grace
// is silent. The customer fact, state and existing host signal share a transaction.
func notify(ctx context.Context, database *db.DB, t Transition, exposure Exposure, now time.Time, id uuid.UUID) error {
	var eventType models.NotificationEventType
	switch {
	case t.To == StateDelinquent:
		eventType = models.NotificationAccountDelinquent
	case t.To == StateCurrent && t.From == StateDelinquent:
		eventType = models.NotificationAccountDelinquencyClosed
	default:
		return nil
	}
	overdue := exposure.OverdueAmount
	data := billing.NotificationData{
		Currency: catalog.Currency(t.Currency), OverdueAmount: &overdue, OverdueInvoices: exposure.OverdueInvoices,
		FromState: t.From.String(), ToState: t.To.String(),
	}
	if exposure.Owes() {
		since := exposure.OverdueStartedAt.UTC()
		data.OverdueStartedAt = &since
	}
	if err := subscriptions.NewNotificationService(database).CreateIfAbsent(ctx, &models.NotificationEvent{
		ID:         id,
		CustomerID: t.CustomerID,
		EventType:  eventType,
		Data:       data,
		CreatedAt:  now,
	}); err != nil {
		return fmt.Errorf("record delinquency notification: %w", err)
	}
	return nil
}

// IsDelinquent is the admission gate's question, answered conservatively: it
// reads the stored state (one primary-key lookup) and refuses only if a live
// recompute against the usage invoices agrees, so a customer who just settled or was
// never evaluated is never refused. Any error fails open: our malfunction must
// not become the customer's outage.
func (s *Service) IsDelinquent(ctx context.Context, customer identity.CustomerID, currency string) (bool, error) {
	if s == nil || s.db == nil {
		return false, nil
	}
	if customer.IsZero() || currency == "" {
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
			MerchantID: tid.UUID(), CustomerID: customer.UUID(), Currency: currency,
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

		now := s.now()
		policy, exposure, _, err := readExposure(ctx, s.db, q, tid, customer.UUID(), currency, now)
		if err != nil {
			return err
		}
		delinquent = Classify(policy, exposure, now) == StateDelinquent
		return nil
	})
	return delinquent, err
}
