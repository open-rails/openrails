package checkout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// TypeStripeTierChange is one Stripe subscription tier change (an immediate
// upgrade or a scheduled downgrade) as a durable operation. The operation id
// is the provider identity: every Stripe request carries an idempotency key
// rooted in it and the mutated object is stamped with it, so a replay under
// the client's key, a restart or a reclaimed lease never mints a second
// identity and every receipt is attributable to exactly this operation.
const TypeStripeTierChange = "stripe_tier_change"

// stripeTierChangeReplayWindow bounds idempotent replay to well inside
// Stripe's 24h idempotency-key retention; after it only an exact receipt or
// operator resolution closes the operation.
const stripeTierChangeReplayWindow = 23 * time.Hour

// stripePaymentBehaviorPaidOrRefused is the upgrade's payment_behavior: a
// 2xx means the change applied with its invoice paid, a 402 that nothing
// changed (https://docs.stripe.com/api/subscriptions/update). Stripe's default,
// allow_incomplete, would apply the change with an unpaid invoice.
const stripePaymentBehaviorPaidOrRefused = "error_if_incomplete"

// StripeTierChangePayload freezes the commercial decision before the first
// provider request. Replays never recalculate proration, prices or dates.
type StripeTierChangePayload struct {
	RequestedPrice       string    `json:"requested_price"`
	UserID               string    `json:"user_id"`
	SubscriptionID       uuid.UUID `json:"subscription_id"`
	StripeSubscriptionID string    `json:"stripe_subscription_id"`
	StripeItemID         string    `json:"stripe_item_id"`
	Action               string    `json:"action"`
	OldPriceID           uuid.UUID `json:"old_price_id"`
	OldStripePriceID     string    `json:"old_stripe_price_id"`
	PriceID              uuid.UUID `json:"price_id"`
	ProductID            uuid.UUID `json:"product_id"`
	ProductName          string    `json:"product_name"`
	StripePriceID        string    `json:"stripe_price_id"`
	Currency             string    `json:"currency"`
	RecurringAmount      int64     `json:"recurring_amount"`
	// AmountDueNow is the local Model B estimate for an upgrade (Stripe
	// finalizes the exact proration) and 0 for a downgrade.
	AmountDueNow       int64  `json:"amount_due_now"`
	ProrationBehavior  string `json:"proration_behavior"`
	BillingCycleAnchor string `json:"billing_cycle_anchor,omitempty"`
	// PaymentBehavior is error_if_incomplete for an upgrade: Stripe either
	// applies the change with its invoice paid or refuses it (402) unchanged.
	PaymentBehavior string `json:"payment_behavior,omitempty"`
	// PeriodStart/PeriodEnd: the fresh period an upgrade opens; the current
	// period a downgrade keeps until the switch.
	PeriodStart      time.Time `json:"period_start"`
	PeriodEnd        time.Time `json:"period_end"`
	BillingCycleDays *int      `json:"billing_cycle_days,omitempty"`
}

func (p StripeTierChangePayload) validate() error {
	if p.SubscriptionID == uuid.Nil || p.StripeSubscriptionID == "" || p.OldPriceID == uuid.Nil || p.OldStripePriceID == "" || p.PriceID == uuid.Nil || p.StripePriceID == "" || p.UserID == "" || !p.PeriodEnd.After(p.PeriodStart) {
		return errors.New("incomplete frozen tier change payload")
	}
	switch p.Action {
	case "upgrade":
		if p.StripeItemID == "" {
			return errors.New("frozen upgrade has no subscription item")
		}
		if p.PaymentBehavior != stripePaymentBehaviorPaidOrRefused {
			return fmt.Errorf("frozen upgrade payment behavior %q is not %s", p.PaymentBehavior, stripePaymentBehaviorPaidOrRefused)
		}
	case "downgrade":
	default:
		return fmt.Errorf("unknown tier change action %q", p.Action)
	}
	return nil
}

// stripeTierChangeStep is one provider request: its write-ahead fence, then
// exactly one of a matched receipt or a definitive refusal.
type stripeTierChangeStep struct {
	SubmittedAt   time.Time                              `json:"submitted_at"`
	Subscription  *subscriptions.StripeSubscriptionState `json:"subscription,omitempty"`
	Schedule      *subscriptions.StripeScheduleState     `json:"schedule,omitempty"`
	Refusal       string                                 `json:"refusal,omitempty"`
	RefusalCode   string                                 `json:"refusal_code,omitempty"`
	RefusalStatus int                                    `json:"refusal_status,omitempty"`
	Resolution    map[string]any                         `json:"resolution,omitempty"`
}

func (s *stripeTierChangeStep) settled() bool {
	return s != nil && (s.Subscription != nil || s.Schedule != nil || s.Refusal != "")
}

const (
	stripeStepUpdate   = "update"
	stripeStepSchedule = "schedule"
	stripeStepPhases   = "phases"
)

type stripeTierChangeProgress struct {
	Update   *stripeTierChangeStep `json:"update,omitempty"`
	Schedule *stripeTierChangeStep `json:"schedule,omitempty"`
	Phases   *stripeTierChangeStep `json:"phases,omitempty"`
}

func (p *stripeTierChangeProgress) step(name string) **stripeTierChangeStep {
	switch name {
	case stripeStepUpdate:
		return &p.Update
	case stripeStepSchedule:
		return &p.Schedule
	case stripeStepPhases:
		return &p.Phases
	}
	return nil
}

func (p stripeTierChangeProgress) evidence() map[string]any {
	out := map[string]any{}
	for name, step := range map[string]*stripeTierChangeStep{stripeStepUpdate: p.Update, stripeStepSchedule: p.Schedule, stripeStepPhases: p.Phases} {
		if step != nil {
			out[name] = step
		}
	}
	return out
}

func (p stripeTierChangeProgress) refused() *stripeTierChangeStep {
	for _, step := range []*stripeTierChangeStep{p.Update, p.Schedule, p.Phases} {
		if step != nil && step.Refusal != "" {
			return step
		}
	}
	return nil
}

func decodeStripeTierChange(in gen.OpenrailsRailIntent) (StripeTierChangePayload, stripeTierChangeProgress, error) {
	var p StripeTierChangePayload
	if err := json.Unmarshal(in.Payload, &p); err != nil {
		return p, stripeTierChangeProgress{}, fmt.Errorf("invalid tier change payload: %w", err)
	}
	if err := p.validate(); err != nil {
		return p, stripeTierChangeProgress{}, err
	}
	var progress stripeTierChangeProgress
	if len(in.ResultEvidence) > 0 {
		if err := json.Unmarshal(in.ResultEvidence, &progress); err != nil {
			return p, progress, fmt.Errorf("read tier change receipts: %w", err)
		}
	}
	return p, progress, nil
}

// StripeTierChangeIntentHandler submits each Stripe step behind a write-ahead
// fence, keeps its exact receipt or definitive refusal, replays a lost
// response only through Stripe's idempotency key inside the retention window,
// and commits the local subscription change only from a matched receipt.
type StripeTierChangeIntentHandler struct{ Checkout *CheckoutService }

func NewStripeTierChangeIntentHandler(s *CheckoutService) *StripeTierChangeIntentHandler {
	return &StripeTierChangeIntentHandler{Checkout: s}
}
func (*StripeTierChangeIntentHandler) Type() string { return TypeStripeTierChange }
func (*StripeTierChangeIntentHandler) Backoff(attempts int32) time.Duration {
	return intents.DefaultBackoff.Delay(attempts)
}

// PrunePolicy keeps payload and receipts: a client replay renders the stored
// result from them.
func (*StripeTierChangeIntentHandler) PrunePolicy() (bool, bool) { return true, true }
func (*StripeTierChangeIntentHandler) CheckRelevance(context.Context, gen.OpenrailsRailIntent) (intents.Relevance, error) {
	return intents.StillRelevant(), nil
}
func (h *StripeTierChangeIntentHandler) Execute(ctx context.Context, in gen.OpenrailsRailIntent) intents.Outcome {
	return h.advance(ctx, in, true)
}
func (h *StripeTierChangeIntentHandler) Verify(ctx context.Context, in gen.OpenrailsRailIntent) intents.Outcome {
	return h.advance(ctx, in, false)
}

func (h *StripeTierChangeIntentHandler) stripe() *subscriptions.StripeService {
	return &subscriptions.StripeService{StripeClients: h.Checkout.StripeClients, Config: h.Checkout.Config, Rails: h.Checkout.Rails}
}

func (h *StripeTierChangeIntentHandler) replayable(step *stripeTierChangeStep) bool {
	return h.Checkout.now().Before(step.SubmittedAt.Add(stripeTierChangeReplayWindow))
}

// stripeRefusalIsDefinitive: a parsed 4xx is Stripe rejecting the request
// without executing it, except the idempotency-key-in-use and rate-limit
// answers, which say nothing about the original request.
func stripeRefusalIsDefinitive(err error) (*subscriptions.StripeAPIError, bool) {
	var apiErr *subscriptions.StripeAPIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode < 400 || apiErr.StatusCode >= 500 {
		return nil, false
	}
	return apiErr, apiErr.StatusCode != http.StatusConflict && apiErr.StatusCode != http.StatusTooManyRequests
}

type stripeStepRun struct {
	ctx      context.Context
	in       gen.OpenrailsRailIntent
	p        StripeTierChangePayload
	progress *stripeTierChangeProgress
	store    *intents.Store
	stripe   *subscriptions.StripeService
	send     bool
}

func (r *stripeStepRun) save(name string, step *stripeTierChangeStep) error {
	return r.store.RecordProgress(r.ctx, r.in.ID, map[string]any{name: step})
}

func (*StripeTierChangeIntentHandler) CommitsTerminalOutcome() bool { return true }

func (h *StripeTierChangeIntentHandler) advance(ctx context.Context, in gen.OpenrailsRailIntent, send bool) intents.Outcome {
	if h.Checkout == nil || h.Checkout.SubscriptionService == nil {
		return intents.Parked("tier change service unavailable")
	}
	p, _, err := decodeStripeTierChange(in)
	if err != nil {
		return commitTierRefusal(ctx, h.Checkout.SubscriptionService.Database(), in, intents.Terminal(err.Error()), h.Checkout.now())
	}
	store := intents.NewStore(h.Checkout.SubscriptionService.Database())
	// Always reload progress: a preceding submission may have committed its
	// receipt immediately before this lease was reclaimed.
	current, err := store.Get(ctx, in.ID)
	if err != nil {
		return intents.Ambiguous("read tier change progress: " + err.Error())
	}
	_, progress, err := decodeStripeTierChange(current)
	if err != nil {
		return intents.Ambiguous(err.Error())
	}
	run := &stripeStepRun{ctx: ctx, in: in, p: p, progress: &progress, store: store, stripe: h.stripe(), send: send}
	if step := progress.refused(); step != nil {
		return h.refusedOutcome(ctx, in, progress, step)
	}
	if p.Action == "downgrade" {
		return h.advanceDowngrade(run)
	}
	return h.advanceUpgrade(run)
}

func (h *StripeTierChangeIntentHandler) refusedOutcome(ctx context.Context, in gen.OpenrailsRailIntent, progress stripeTierChangeProgress, step *stripeTierChangeStep) intents.Outcome {
	reason := "stripe refused the tier change: " + step.Refusal
	if step.Resolution != nil {
		reason = "tier change closed by operator: " + step.Refusal
	}
	if progress.Schedule != nil && progress.Schedule.Schedule != nil && step == progress.Phases {
		reason += " (schedule " + progress.Schedule.Schedule.ID + " keeps the current price and releases at period end)"
	}
	return commitTierRefusal(ctx, h.Checkout.SubscriptionService.Database(), in, intents.TerminalWithEvidence(reason, progress.evidence()), h.Checkout.now())
}

// fence records the step's submission marker; false means another executor
// owns the submission and this attempt must reconcile instead of sending.
func (h *StripeTierChangeIntentHandler) fence(r *stripeStepRun, name string) (*stripeTierChangeStep, intents.Outcome, bool) {
	step := &stripeTierChangeStep{SubmittedAt: h.Checkout.now().UTC()}
	claimed, err := r.store.RecordProgressIfAbsent(r.ctx, r.in.ID, name, step)
	if err != nil {
		return nil, intents.Parked("persist " + name + " submission: " + err.Error()), false
	}
	if !claimed {
		return nil, intents.Ambiguous(name + " submission already owned; reconcile receipt"), false
	}
	*r.progress.step(name) = step
	return step, intents.Outcome{}, true
}

// classify persists one submission answer: a definitive refusal terminates,
// a matched object is the receipt, anything else is a possible submission.
func (h *StripeTierChangeIntentHandler) classify(r *stripeStepRun, name string, step *stripeTierChangeStep, err error, match func() error) (intents.Outcome, bool) {
	if apiErr, definitive := stripeRefusalIsDefinitive(err); definitive {
		step.Refusal, step.RefusalCode, step.RefusalStatus = apiErr.Message, apiErr.FailureCode(), apiErr.StatusCode
		if serr := r.save(name, step); serr != nil {
			return intents.Ambiguous("persist " + name + " refusal: " + serr.Error()), false
		}
		return h.refusedOutcome(r.ctx, r.in, *r.progress, step), false
	}
	if err != nil {
		return intents.Ambiguous(name + " outcome unknown: " + err.Error()), false
	}
	if merr := match(); merr != nil {
		// Stripe answered 2xx with an object that is not the frozen outcome:
		// nothing commits from it and nothing is resent.
		return intents.AmbiguousWithEvidence("stripe answered with an object that is not the frozen tier change: "+merr.Error(), map[string]any{"provider_contradiction": merr.Error()}), false
	}
	if serr := r.save(name, step); serr != nil {
		return intents.Ambiguous("persist " + name + " receipt: " + serr.Error()), false
	}
	return intents.Outcome{}, true
}

func (h *StripeTierChangeIntentHandler) submitUpdate(r *stripeStepRun, step *stripeTierChangeStep) (intents.Outcome, bool) {
	state, err := r.stripe.ChangeSubscriptionPrice(r.ctx, subscriptions.StripePriceChangeParams{
		SubscriptionID: r.p.StripeSubscriptionID, ItemID: r.p.StripeItemID, StripePriceID: r.p.StripePriceID,
		InternalPriceID: r.p.PriceID.String(), Key: r.in.ID.String(),
		ProrationBehavior: r.p.ProrationBehavior, BillingCycleAnchor: r.p.BillingCycleAnchor, PaymentBehavior: r.p.PaymentBehavior,
	})
	return h.classify(r, stripeStepUpdate, step, err, func() error {
		if err := state.MatchesPriceChange(r.p.StripeSubscriptionID, r.in.ID.String(), r.p.StripePriceID, r.p.PriceID.String()); err != nil {
			return err
		}
		step.Subscription = &state
		return nil
	})
}

// reconcile settles a fenced step without a receipt: an exact provider read
// first; then, while Stripe still holds the key, the executor replays the
// identical request; after that only operator resolution.
func (h *StripeTierChangeIntentHandler) reconcile(r *stripeStepRun, name string, step *stripeTierChangeStep, read func() (bool, error), replay func() (intents.Outcome, bool)) (intents.Outcome, bool) {
	matched, err := read()
	if err != nil {
		return intents.Ambiguous("read " + name + " receipt: " + err.Error()), false
	}
	if matched {
		if serr := r.save(name, step); serr != nil {
			return intents.Ambiguous("persist " + name + " readback: " + serr.Error()), false
		}
		return intents.Outcome{}, true
	}
	if !h.replayable(step) {
		return intents.Ambiguous("stripe idempotency window elapsed for " + name + "; resolve with the exact stripe object or provider-confirmed non-execution"), false
	}
	if !r.send {
		return intents.Retryable("stripe " + name + " replay through provider idempotency key"), false
	}
	return replay()
}

func (h *StripeTierChangeIntentHandler) advanceUpgrade(r *stripeStepRun) intents.Outcome {
	step := r.progress.Update
	if step == nil {
		if !r.send {
			return intents.Retryable("price change was not submitted; execute under provider write gates")
		}
		if err := h.requireFrozenSubscription(r.ctx, r.p); err != nil {
			return commitTierRefusal(r.ctx, h.Checkout.SubscriptionService.Database(), r.in, intents.Terminal(err.Error()), h.Checkout.now())
		}
		var outcome intents.Outcome
		var ok bool
		if step, outcome, ok = h.fence(r, stripeStepUpdate); !ok {
			return outcome
		}
		if outcome, ok = h.submitUpdate(r, step); !ok {
			return outcome
		}
	}
	if step.Subscription == nil {
		outcome, ok := h.reconcile(r, stripeStepUpdate, step, func() (bool, error) {
			state, found, err := r.stripe.GetSubscriptionState(r.ctx, r.p.StripeSubscriptionID)
			if err != nil || !found || state.MatchesPriceChange(r.p.StripeSubscriptionID, r.in.ID.String(), r.p.StripePriceID, r.p.PriceID.String()) != nil {
				return false, err
			}
			step.Subscription = &state
			return true, nil
		}, func() (intents.Outcome, bool) { return h.submitUpdate(r, step) })
		if !ok {
			return outcome
		}
	}
	outcome := intents.Succeeded(h.result(r.p, *r.progress))
	if err := h.finalizeUpgrade(r.ctx, r.in, r.p, *step.Subscription, outcome); err != nil {
		return intents.AmbiguousWithEvidence("price change receipt retained; local commit pending: "+err.Error(), r.progress.evidence())
	}
	return outcome
}

func (h *StripeTierChangeIntentHandler) submitSchedule(r *stripeStepRun, step *stripeTierChangeStep) (intents.Outcome, bool) {
	state, err := r.stripe.CreateScheduleFromSubscription(r.ctx, r.p.StripeSubscriptionID, r.in.ID.String())
	return h.classify(r, stripeStepSchedule, step, err, func() error {
		if err := state.MatchesCreation(r.p.StripeSubscriptionID, r.p.OldStripePriceID); err != nil {
			return err
		}
		step.Schedule = &state
		return nil
	})
}

// currentPhase is the phase Stripe copied from the subscription at schedule
// creation, completed from the frozen period where the receipt omits it.
func currentPhase(created *subscriptions.StripeScheduleState, p StripeTierChangePayload) subscriptions.StripeSchedulePhase {
	phase := subscriptions.StripeSchedulePhase{PriceID: p.OldStripePriceID, StartDate: p.PeriodStart.Unix(), EndDate: p.PeriodEnd.Unix(), Quantity: 1}
	if created != nil && len(created.Phases) > 0 {
		first := created.Phases[0]
		if first.StartDate > 0 {
			phase.StartDate = first.StartDate
		}
		if first.EndDate > 0 {
			phase.EndDate = first.EndDate
		}
		if first.Quantity > 0 {
			phase.Quantity = first.Quantity
		}
	}
	return phase
}

func (h *StripeTierChangeIntentHandler) submitPhases(r *stripeStepRun, step *stripeTierChangeStep) (intents.Outcome, bool) {
	created := r.progress.Schedule.Schedule
	phase := currentPhase(created, r.p)
	state, err := r.stripe.SetSchedulePhases(r.ctx, subscriptions.StripeSchedulePhasesParams{
		ScheduleID: created.ID, Key: r.in.ID.String(), Current: phase, NewPriceID: r.p.StripePriceID, BillingCycleDays: r.p.BillingCycleDays,
	})
	return h.classify(r, stripeStepPhases, step, err, func() error {
		if err := state.MatchesPhases(r.p.StripeSubscriptionID, r.in.ID.String(), r.p.OldStripePriceID, r.p.StripePriceID, phase.EndDate); err != nil {
			return err
		}
		step.Schedule = &state
		return nil
	})
}

func (h *StripeTierChangeIntentHandler) advanceDowngrade(r *stripeStepRun) intents.Outcome {
	step := r.progress.Schedule
	if step == nil {
		if !r.send {
			return intents.Retryable("schedule was not submitted; execute under provider write gates")
		}
		if err := h.requireFrozenSubscription(r.ctx, r.p); err != nil {
			return commitTierRefusal(r.ctx, h.Checkout.SubscriptionService.Database(), r.in, intents.Terminal(err.Error()), h.Checkout.now())
		}
		var outcome intents.Outcome
		var ok bool
		if step, outcome, ok = h.fence(r, stripeStepSchedule); !ok {
			return outcome
		}
		if outcome, ok = h.submitSchedule(r, step); !ok {
			return outcome
		}
	}
	if step.Schedule == nil {
		// A schedule attached to the subscription is not read as this
		// operation's: only the idempotent replay or an operator names it.
		outcome, ok := h.reconcile(r, stripeStepSchedule, step, func() (bool, error) { return false, nil },
			func() (intents.Outcome, bool) { return h.submitSchedule(r, step) })
		if !ok {
			return outcome
		}
	}
	phases := r.progress.Phases
	if phases == nil {
		if !r.send {
			return intents.Retryable("schedule created; phases have not been submitted")
		}
		var outcome intents.Outcome
		var ok bool
		if phases, outcome, ok = h.fence(r, stripeStepPhases); !ok {
			return outcome
		}
		if outcome, ok = h.submitPhases(r, phases); !ok {
			return outcome
		}
	}
	if phases.Schedule == nil {
		outcome, ok := h.reconcile(r, stripeStepPhases, phases, func() (bool, error) {
			state, found, err := r.stripe.GetScheduleState(r.ctx, step.Schedule.ID)
			if err != nil || !found || state.MatchesPhases(r.p.StripeSubscriptionID, r.in.ID.String(), r.p.OldStripePriceID, r.p.StripePriceID, currentPhase(step.Schedule, r.p).EndDate) != nil {
				return false, err
			}
			phases.Schedule = &state
			return true, nil
		}, func() (intents.Outcome, bool) { return h.submitPhases(r, phases) })
		if !ok {
			return outcome
		}
	}
	outcome := intents.Succeeded(h.result(r.p, *r.progress))
	if err := h.finalizeDowngrade(r.ctx, r.in, r.p, outcome); err != nil {
		return intents.AmbiguousWithEvidence("schedule receipt retained; local commit pending: "+err.Error(), r.progress.evidence())
	}
	return outcome
}

// requireFrozenSubscription refuses the first submission when the local
// subscription no longer matches the frozen predecessor.
func (h *StripeTierChangeIntentHandler) requireFrozenSubscription(ctx context.Context, p StripeTierChangePayload) error {
	sub, err := h.Checkout.SubscriptionService.GetByID(ctx, p.SubscriptionID)
	if err != nil {
		return fmt.Errorf("load subscription before submission: %w", err)
	}
	if (sub.Status != models.StatusActive && sub.Status != models.StatusPastDue) || sub.PriceID != p.OldPriceID || sub.RailSubscriptionID != p.StripeSubscriptionID || sub.ScheduledPriceID != nil {
		return errors.New("subscription changed before submission")
	}
	return nil
}

// finalizeUpgrade commits the upgrade from its matched receipt. The period is
// the one Stripe opened (billing_cycle_anchor=now is Stripe's clock at
// execution), never the enqueue-time estimate. The webhook converger may
// have mirrored the same Stripe subscription first: a subscription already on
// the target price is complete, and only a period older than the receipt's
// is brought up to it.
func (h *StripeTierChangeIntentHandler) finalizeUpgrade(ctx context.Context, in gen.OpenrailsRailIntent, p StripeTierChangePayload, receipt subscriptions.StripeSubscriptionState, outcome intents.Outcome) error {
	start, end, ok := receipt.Period()
	if !ok {
		return fmt.Errorf("price change receipt for %s carries no billing period", receipt.ID)
	}
	database := h.Checkout.SubscriptionService.Database()
	now := h.Checkout.now().UTC()
	return database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		bound := database.NewWithPgxTx(tx)
		repo := subscriptions.NewSubscriptionRepo(bound)
		sub, err := repo.GetByIDForUpdate(ctx, p.SubscriptionID)
		if err != nil {
			return err
		}
		completion, err := prepareTierCompletion(ctx, bound, in, outcome, now)
		if err != nil {
			return err
		}
		if completion.committed {
			return nil
		}

		if in.PspID == nil || sub.PspID != *in.PspID || sub.CustomerID.String() != p.UserID || sub.RailSubscriptionID != p.StripeSubscriptionID {
			return fmt.Errorf("subscription %s no longer references stripe subscription %s", sub.ID, p.StripeSubscriptionID)
		}
		switch sub.PriceID {
		case p.PriceID:
			if sub.CurrentPeriodEndsAt != nil && !sub.CurrentPeriodEndsAt.Before(end) {
				return completion.commit(ctx)
			}
		case p.OldPriceID:
			sub.PriceID, sub.ProductID, sub.ScheduledPriceID = p.PriceID, p.ProductID, nil
		default:
			return fmt.Errorf("subscription %s is on price %s, neither the frozen predecessor %s nor the target %s", sub.ID, sub.PriceID, p.OldPriceID, p.PriceID)
		}
		sub.CurrentPeriodStartsAt, sub.CurrentPeriodEndsAt = &start, &end
		if err := repo.UpdateAt(ctx, sub, now); err != nil {
			return err
		}
		return completion.commit(ctx)
	})
}

func (h *StripeTierChangeIntentHandler) finalizeDowngrade(ctx context.Context, in gen.OpenrailsRailIntent, p StripeTierChangePayload, outcome intents.Outcome) error {
	database := h.Checkout.SubscriptionService.Database()
	now := h.Checkout.now().UTC()
	return database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		bound := database.NewWithPgxTx(tx)
		repo := subscriptions.NewSubscriptionRepo(bound)
		sub, err := repo.GetByIDForUpdate(ctx, p.SubscriptionID)
		if err != nil {
			return err
		}
		completion, err := prepareTierCompletion(ctx, bound, in, outcome, now)
		if err != nil {
			return err
		}
		if completion.committed {
			return nil
		}

		if in.PspID == nil || sub.PspID != *in.PspID || sub.CustomerID.String() != p.UserID || sub.RailSubscriptionID != p.StripeSubscriptionID {
			return errors.New("subscription no longer names accepted tier target")
		}
		if sub.ScheduledPriceID != nil {
			if *sub.ScheduledPriceID == p.PriceID {
				return completion.commit(ctx)
			}
			return fmt.Errorf("subscription %s already schedules price %s", sub.ID, *sub.ScheduledPriceID)
		}
		if sub.PriceID != p.OldPriceID {
			return fmt.Errorf("subscription %s is on price %s, not the frozen predecessor %s", sub.ID, sub.PriceID, p.OldPriceID)
		}
		scheduled := p.PriceID
		sub.ScheduledPriceID = &scheduled
		if err := repo.UpdateAt(ctx, sub, now); err != nil {
			return err
		}
		return completion.commit(ctx)
	})
}

func (h *StripeTierChangeIntentHandler) result(p StripeTierChangePayload, progress stripeTierChangeProgress) map[string]any {
	out := progress.evidence()
	out["subscription_id"] = p.SubscriptionID.String()
	if progress.Update != nil && progress.Update.Subscription != nil && progress.Update.Subscription.LatestInvoiceID != "" {
		out["transaction_id"] = progress.Update.Subscription.LatestInvoiceID
	}
	return out
}

// Resolve accepts exact provider evidence for one submitted step that has no
// receipt, or provider-confirmed non-execution of it. A receipt is read back
// by its exact id and must match the frozen operation; non-execution is
// refused while the provider shows the step's effect.
func (h *StripeTierChangeIntentHandler) Resolve(ctx context.Context, in gen.OpenrailsRailIntent, resolution intents.Resolution) (intents.Outcome, error) {
	if h.Checkout == nil || h.Checkout.SubscriptionService == nil {
		return intents.Outcome{}, errors.New("tier change service unavailable")
	}
	p, progress, err := decodeStripeTierChange(in)
	if err != nil {
		return intents.Outcome{}, err
	}
	name := resolution.Step
	if name == "" {
		switch {
		case p.Action == "upgrade":
			name = stripeStepUpdate
		case progress.Phases != nil:
			name = stripeStepPhases
		default:
			name = stripeStepSchedule
		}
	}
	slot := progress.step(name)
	if slot == nil || (p.Action == "upgrade") != (name == stripeStepUpdate) {
		expected := "schedule or phases"
		if p.Action == "upgrade" {
			expected = stripeStepUpdate
		}
		return intents.Outcome{}, fmt.Errorf("%w: %s step must be %s", intents.ErrResolutionInvalid, p.Action, expected)
	}
	step := *slot
	if step == nil {
		return intents.Outcome{}, intents.RejectResolution("%s step was never submitted", name)
	}
	if step.settled() {
		return intents.Outcome{}, intents.RejectResolution("%s step already has an outcome", name)
	}
	stripe := h.stripe()
	ref := resolution.ProviderReference
	switch {
	case name == stripeStepUpdate && resolution.NotExecuted:
		state, found, err := stripe.GetSubscriptionState(ctx, p.StripeSubscriptionID)
		if err != nil {
			return intents.Outcome{}, fmt.Errorf("read stripe subscription: %w", err)
		}
		if found && state.MatchesPriceChange(p.StripeSubscriptionID, in.ID.String(), p.StripePriceID, p.PriceID.String()) == nil {
			return intents.Outcome{}, intents.RejectResolution("provider shows the price change on subscription %s", p.StripeSubscriptionID)
		}
		step.Refusal = "provider confirmed the price change was not executed"
	case name == stripeStepUpdate:
		if ref != p.StripeSubscriptionID {
			return intents.Outcome{}, intents.RejectResolution("receipt %s is not the frozen subscription %s", ref, p.StripeSubscriptionID)
		}
		state, found, err := stripe.GetSubscriptionState(ctx, ref)
		if err != nil {
			return intents.Outcome{}, fmt.Errorf("read stripe subscription: %w", err)
		}
		if !found {
			return intents.Outcome{}, intents.RejectResolution("subscription %s does not exist at stripe", ref)
		}
		if err := state.MatchesPriceChange(p.StripeSubscriptionID, in.ID.String(), p.StripePriceID, p.PriceID.String()); err != nil {
			return intents.Outcome{}, intents.RejectResolution("%v", err)
		}
		step.Subscription = &state
	case name == stripeStepSchedule && resolution.NotExecuted:
		state, found, err := stripe.GetSubscriptionState(ctx, p.StripeSubscriptionID)
		if err != nil {
			return intents.Outcome{}, fmt.Errorf("read stripe subscription: %w", err)
		}
		if found && state.ScheduleID != "" {
			return intents.Outcome{}, intents.RejectResolution("subscription %s carries schedule %s; name it with --receipt or release it at stripe first", p.StripeSubscriptionID, state.ScheduleID)
		}
		step.Refusal = "provider confirmed the schedule was not created"
	case name == stripeStepSchedule:
		state, found, err := stripe.GetScheduleState(ctx, ref)
		if err != nil {
			return intents.Outcome{}, fmt.Errorf("read stripe schedule: %w", err)
		}
		if !found {
			return intents.Outcome{}, intents.RejectResolution("schedule %s does not exist at stripe", ref)
		}
		if err := state.MatchesCreation(p.StripeSubscriptionID, p.OldStripePriceID); err != nil {
			return intents.Outcome{}, intents.RejectResolution("%v", err)
		}
		step.Schedule = &state
	default:
		created := progress.Schedule.Schedule
		state, found, err := stripe.GetScheduleState(ctx, created.ID)
		if err != nil {
			return intents.Outcome{}, fmt.Errorf("read stripe schedule: %w", err)
		}
		matched := found && state.MatchesPhases(p.StripeSubscriptionID, in.ID.String(), p.OldStripePriceID, p.StripePriceID, currentPhase(created, p).EndDate) == nil
		if resolution.NotExecuted {
			if matched {
				return intents.Outcome{}, intents.RejectResolution("provider shows the phases on schedule %s", created.ID)
			}
			step.Refusal = "provider confirmed the phases were not written"
			break
		}
		if ref != created.ID {
			return intents.Outcome{}, intents.RejectResolution("receipt %s is not the operation's schedule %s", ref, created.ID)
		}
		if !matched {
			return intents.Outcome{}, intents.RejectResolution("schedule %s does not show the frozen phases", created.ID)
		}
		step.Schedule = &state
	}
	step.Resolution = resolution.Record(h.Checkout.now())
	if err := intents.NewStore(h.Checkout.SubscriptionService.Database()).RecordProgress(ctx, in.ID, map[string]any{name: step}); err != nil {
		return intents.Outcome{}, fmt.Errorf("persist resolved %s step: %w", name, err)
	}
	return h.advance(ctx, in, false), nil
}

// ResolveUnsent releases an operation that never crossed a submission fence.
func (h *StripeTierChangeIntentHandler) ResolveUnsent(ctx context.Context, in gen.OpenrailsRailIntent, resolution intents.Resolution) (intents.Outcome, error) {
	_, progress, err := decodeStripeTierChange(in)
	if err != nil {
		return intents.Outcome{}, err
	}
	if progress.Update != nil || progress.Schedule != nil || progress.Phases != nil {
		return intents.Outcome{}, intents.RejectResolution("a submission fence exists; only the verifier or an exact receipt can close this operation")
	}
	return commitTierRefusal(ctx, h.Checkout.SubscriptionService.Database(), in, intents.Terminal("operator released the never-submitted tier change: "+resolution.Reason), h.Checkout.now()), nil
}

// enqueueStripeTierChange records the frozen operation and runs it inline.
// A conflict on the subject index means another unresolved tier change owns
// the subscription; the caller is pointed at it.
func (s *CheckoutService) enqueueStripeTierChange(ctx context.Context, existingSub *models.Subscription, payload StripeTierChangePayload, key string) (*TierChangeResponse, error) {
	if s.Intents == nil {
		return nil, errors.New("durable tier change service unavailable")
	}
	ctx = db.WithPSPID(ctx, existingSub.PspID)
	intent, err := s.Intents.EnqueueOwnedAndExecute(ctx, intents.EnqueueParams{
		MerchantID: existingSub.MerchantID, Provider: string(models.RailStripe), PspID: existingSub.PspID,
		IntentType: TypeStripeTierChange, SubscriptionID: &existingSub.ID, PriceID: &payload.PriceID,
		Payload: payload, IdempotencyKey: key, NextAttemptAt: s.now(), Origin: intents.OriginUser,
		OriginReason: "customer tier " + payload.Action,
	}, func(row gen.OpenrailsRailIntent) error { return tierChangeOwnedBy(row, payload.subject()) })
	var conflict *pgconn.PgError
	if errors.As(err, &conflict) && conflict.Code == "23505" && conflict.ConstraintName == tierChangeSubjectConstraint {
		return nil, s.tierChangeInFlight(ctx, existingSub.ID)
	}
	if err != nil {
		return nil, err
	}
	return tierChangeResponse(intent)
}

// stripeTierChangeResponse renders a Stripe tier change (tierChangeResponse).
func stripeTierChangeResponse(in gen.OpenrailsRailIntent) (*TierChangeResponse, error) {
	var p StripeTierChangePayload
	if err := json.Unmarshal(in.Payload, &p); err != nil {
		return nil, err
	}
	subID := openrails.SubscriptionID(p.SubscriptionID)
	end := p.PeriodEnd
	resp := &TierChangeResponse{
		Object: "tier_change", Mode: "tier_change", Action: p.Action, PriceID: (openrails.PriceID(p.PriceID)).String(),
		Payment: CheckoutSessionPaymentResponse{Rail: string(models.RailStripe)}, SubscriptionID: &subID,
		Currency: p.Currency, AmountDueNow: p.AmountDueNow, NextChargeAmount: p.RecurringAmount, NextChargeDate: &end,
		OperationID: in.ID.String(), Effective: effectiveOf(p.Action),
	}
	switch in.Status {
	case intents.StatusSucceeded:
		resp.Status = "succeeded"
		resp.Payment.TransactionID = intents.EvidenceString(in, "transaction_id")
		// Dates come from the receipt Stripe answered with, not the estimate.
		var progress stripeTierChangeProgress
		_ = json.Unmarshal(in.ResultEvidence, &progress)
		if progress.Update != nil && progress.Update.Subscription != nil {
			if _, periodEnd, ok := progress.Update.Subscription.Period(); ok {
				end = periodEnd
			}
		}
		if progress.Phases != nil && progress.Phases.Schedule != nil && len(progress.Phases.Schedule.Phases) > 0 && progress.Phases.Schedule.Phases[0].EndDate > 0 {
			end = time.Unix(progress.Phases.Schedule.Phases[0].EndDate, 0).UTC()
		}
		if p.Action == "downgrade" {
			resp.DelayedStart = &end
			resp.Message = fmt.Sprintf("Downgrade to %s scheduled. Your current plan will remain active until %s.", p.ProductName, end.Format("January 2, 2006"))
		} else {
			resp.Message = "Plan updated"
		}
		return resp, nil
	case intents.StatusFailedTerminal:
		// Stripe's own 402 keeps its decline code; another Stripe refusal is
		// a 400; an operator closure is a 409.
		var progress stripeTierChangeProgress
		_ = json.Unmarshal(in.ResultEvidence, &progress)
		if step := progress.refused(); step != nil {
			return nil, tierChangeRefused(in, step.RefusalStatus, step.RefusalCode)
		}
		return nil, tierChangeRefused(in, 0, "")
	default:
		return tierChangeProcessing(resp)
	}
}
