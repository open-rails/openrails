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

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
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

// PassBatch caps how many (payer, currency) pairs ONE merchant's evaluation
// pass examines per leg. Both legs are already activity-shaped — overdue
// receivables and already-parked payers, never the customer table — and this is
// the second bound on top: a pass is bounded work regardless. The enter leg is
// ordered oldest-debt-first, so a merchant beyond the cap still gets its most
// urgent payers evaluated, and the rest on the next 15-minute pass.
const PassBatch = 5000

// Snapshot is one payer's delinquency state in one currency.
type Snapshot struct {
	CustomerID      uuid.UUID  `json:"customer_id"`
	Currency        string     `json:"currency"`
	State           State      `json:"state"`
	OverdueSince    *time.Time `json:"overdue_since,omitempty"`
	OverdueAmount   int64      `json:"overdue_amount"`
	OverdueInvoices int        `json:"overdue_invoices"`
	EnteredAt       time.Time  `json:"entered_at"`
	EvaluatedAt     time.Time  `json:"evaluated_at"`
}

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

// Policy resolves the merchant's declared delinquency policy.
//
// The amount floor is DERIVED rather than asked for: a merchant that has
// already declared an invoice monthly floor has answered "how small is too
// small to chase", and asking the same question twice invites two answers that
// disagree. An explicit arrears_delinquency_floor overrides it.
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
// Split out so the derivation is testable without a database and so callers
// holding a config need not re-read it.
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

// Evaluate runs one merchant's delinquency pass: classify every payer with
// due work, record the transitions, and emit the signal for each.
//
// The candidate set is DUE WORK, never a roster. Two indexed scans:
//
//	ENTER — payers with an overdue open receivable (ix_invoices_open_due);
//	EXIT  — payers already parked non-current (ix_customer_delinquency_open),
//	        which is the only way a settled debt gets noticed, since a paid
//	        invoice simply stops appearing in the first scan.
//
// A payer with no debt and no parked row is never visited and never gets a row.
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

	// Pinned explicitly. A policied read on an UNPINNED handle matches
	// `merchant_id = NULL` and returns zero rows with no error, so an
	// unscoped work-list read is a pass that reports success and evaluates
	// nothing (or#861/or#877). Reentrant, so the worker's outer scope stands.
	var overdue []gen.ListOverdueInvoiceAggregatesRow
	var parked []gen.OpenrailsCustomerDelinquency
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

	type key struct {
		customer uuid.UUID
		currency string
	}
	candidates := make(map[key]Exposure, len(overdue)+len(parked))
	for _, r := range overdue {
		candidates[key{r.CustomerID, r.Currency}] = Exposure{
			OverdueSince:    r.OverdueSince.UTC(),
			OverdueAmount:   r.OverdueAmount,
			OverdueInvoices: int(r.OverdueInvoices),
		}
	}
	for _, r := range parked {
		// A parked payer absent from the overdue scan owes nothing any more:
		// zero exposure, which Classify reads as `current`.
		if _, ok := candidates[key{r.CustomerID, r.Currency}]; !ok {
			candidates[key{r.CustomerID, r.Currency}] = Exposure{}
		}
	}

	var errs []error
	for k, exposure := range candidates {
		transition, changed, err := s.apply(ctx, tid, policy, k.customer, k.currency, exposure, now)
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

// apply records one payer's evaluation and, if the state actually changed,
// emits the signal for it. Upsert + signal share one transaction: a transition
// that is stored but never announced is a silent shutoff instruction lost, and
// an announcement without the stored state would repeat forever.
func (s *Service) apply(ctx context.Context, tid merchant.ID, policy Policy, customerID uuid.UUID, currency string, exposure Exposure, now time.Time) (Transition, bool, error) {
	state := Classify(policy, exposure, now)

	var since *time.Time
	if state != StateCurrent {
		t := exposure.OverdueSince.UTC()
		since = &t
	}

	var transition Transition
	changed := false
	err := s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		row, err := q.UpsertCustomerDelinquency(ctx, gen.UpsertCustomerDelinquencyParams{
			MerchantID:      tid.UUID(),
			CustomerID:      customerID,
			Currency:        currency,
			State:           string(state),
			OverdueSince:    since,
			OverdueAmount:   exposure.OverdueAmount,
			OverdueInvoices: int64(exposure.OverdueInvoices),
			Now:             now,
		})
		if err != nil {
			return err
		}
		// "" = there was no row before. A first sighting that is already
		// `current` is not a transition — it is a payer who owes nothing, and
		// announcing that would be noise.
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
		if row.OverdueSince != nil {
			payload["overdue_since"] = row.OverdueSince.UTC().Format(time.RFC3339)
		}
		data, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("encode delinquency event: %w", err)
		}
		cur := currency
		// transition_seq is the idempotency coordinate: two evaluators racing
		// the same transition compute the same sequence, so the unique index
		// collapses them into one instruction to the host.
		dedupe := fmt.Sprintf("delinquency:%s:%s:%d", customerID, currency, row.TransitionSeq)
		if _, err := q.EnqueueHostLifecycleEvent(ctx, gen.EnqueueHostLifecycleEventParams{
			MerchantID:  tid.UUID(),
			EventType:   eventTypeFor(ParseState(row.State)),
			SubjectType: subjectCustomer,
			SubjectID:   customerID,
			Currency:    &cur,
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

// notify tells the PAYER, on the two rungs a payer can act on: you are now
// delinquent, and you are no longer delinquent. Entering grace is deliberately
// silent — the or#870 collection ladder already told them the charge failed,
// and a second message for the same event is noise.
//
// Never fatal: the state and the host signal are already durable, and a lost
// in-app notification is not a reason to re-run a money decision.
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
	data := map[string]any{
		"currency":         t.Currency,
		"overdue_amount":   exposure.OverdueAmount,
		"overdue_invoices": exposure.OverdueInvoices,
		"from_state":       t.From.String(),
		"to_state":         t.To.String(),
	}
	if exposure.Owes() {
		data["overdue_since"] = exposure.OverdueSince.UTC().Format(time.RFC3339)
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

// IsDelinquent is the admission gate's question, and it answers conservatively
// on purpose.
//
// It reads the stored state first (one primary-key lookup, so admission stays
// O(1) for the overwhelming majority of payers who are current), and refuses
// only if a LIVE recompute against the invoices agrees. That ordering matters
// in both directions:
//
//   - a payer who has just settled is never refused on a projection the
//     evaluator has not caught up with — the outage risk lands on our side of
//     the ledger, not theirs;
//   - a payer the evaluator has not yet visited is never refused either, since
//     enforcement follows a recorded transition rather than a read.
//
// Any error fails OPEN. A gate that denies because it could not read is a gate
// that turns our malfunction into the customer's outage.
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
	// Pinned: admission may be reached from a seam that has not already pinned
	// a merchant connection, and an unpinned read here would answer "not
	// delinquent" for everyone forever.
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
			OverdueSince:    live.OverdueSince.UTC(),
			OverdueAmount:   live.OverdueAmount,
			OverdueInvoices: int(live.OverdueInvoices),
		}, now) == StateDelinquent
		return nil
	})
	return delinquent, err
}

// ListForCustomer returns one payer's delinquency state in every currency.
func (s *Service) ListForCustomer(ctx context.Context, payer identity.CustomerID) ([]Snapshot, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("delinquency service not initialized")
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	var rows []gen.OpenrailsCustomerDelinquency
	if err := s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		var qErr error
		rows, qErr = s.db.Gen(ctx).ListCustomerDelinquency(ctx, gen.ListCustomerDelinquencyParams{
			MerchantID: tid.UUID(), CustomerID: payer.UUID(),
		})
		return qErr
	}); err != nil {
		return nil, err
	}
	return snapshots(rows), nil
}

// List returns the merchant's overdue roster (grace + delinquent, oldest debt
// first), optionally filtered to one state. Payers in good standing are never
// returned — the roster is the exception list, not a customer directory.
func (s *Service) List(ctx context.Context, state State, limit int) ([]Snapshot, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("delinquency service not initialized")
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var filter *string
	if state != "" {
		if !state.Valid() || state == StateCurrent {
			return nil, fmt.Errorf("delinquency: state filter must be grace or delinquent, got %q", state)
		}
		v := string(state)
		filter = &v
	}
	var rows []gen.OpenrailsCustomerDelinquency
	if err := s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		var qErr error
		rows, qErr = s.db.Gen(ctx).ListDelinquentCustomers(ctx, gen.ListDelinquentCustomersParams{
			MerchantID: tid.UUID(), State: filter, RowLimit: int64(limit),
		})
		return qErr
	}); err != nil {
		return nil, err
	}
	return snapshots(rows), nil
}

func snapshots(rows []gen.OpenrailsCustomerDelinquency) []Snapshot {
	out := make([]Snapshot, 0, len(rows))
	for _, r := range rows {
		out = append(out, Snapshot{
			CustomerID:      r.CustomerID,
			Currency:        r.Currency,
			State:           ParseState(r.State),
			OverdueSince:    r.OverdueSince,
			OverdueAmount:   r.OverdueAmount,
			OverdueInvoices: int(r.OverdueInvoices),
			EnteredAt:       r.EnteredAt,
			EvaluatedAt:     r.EvaluatedAt,
		})
	}
	return out
}
