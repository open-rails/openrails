package intents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/merchant"
)

// TypeNMIEngineTakeover moves one NMI-owned (provider or provider_dunning)
// subscription to OpenRails engine billing at its next period boundary E:
// the NMI schedule is deleted at least EngineTakeoverMargin before E (NMI
// bills by date, so it cannot have billed E), the legacy row ends with access
// to E, and an engine successor opens [E-cycle, E] whose first charge is the
// ordinary engine renewal at E on the same vaulted card and recurring
// agreement. The succeeded operation is the successor's paid agreement.
const TypeNMIEngineTakeover = "nmi_engine_takeover"

// EngineTakeoverMargin is how long before E the NMI schedule must be gone.
const EngineTakeoverMargin = 24 * time.Hour

// EngineTakeoverDriftFinding records a schedule that no longer matches the
// local obligation, found when a takeover read it.
const EngineTakeoverDriftFinding = "life.engine_takeover.schedule_drift"

// EngineTakeoverBatchMax caps one bulk admission; it stays within the #732
// system ceiling so a full batch is never partially refused by it.
const EngineTakeoverBatchMax = PerMerchantSystemHourlyCeiling

// EngineTakeoverRefusal is a typed admission refusal; nothing was changed.
type EngineTakeoverRefusal struct {
	Status  int
	Code    string
	Message string
}

func (e *EngineTakeoverRefusal) Error() string { return e.Code + ": " + e.Message }

func takeoverRefusal(status int, code, format string, args ...any) error {
	return &EngineTakeoverRefusal{Status: status, Code: code, Message: fmt.Sprintf(format, args...)}
}

func ineligible(format string, args ...any) error {
	return takeoverRefusal(http.StatusConflict, openrails.CodeEngineTakeoverIneligible, format, args...)
}

type NMIEngineTakeoverPayload struct {
	LegacySubscriptionID uuid.UUID               `json:"legacy_subscription_id"`
	LegacyPolicy         models.CollectionPolicy `json:"legacy_policy"`
	RailSubscriptionID   string                  `json:"rail_subscription_id"`
	PaymentMethodID      uuid.UUID               `json:"payment_method_id"`
	Instrument           charge.FrozenInstrument `json:"instrument"`
	LegacyPaymentID      uuid.UUID               `json:"legacy_payment_id"`
	AmountMinor          int64                   `json:"amount_minor,string"`
	Anchor               time.Time               `json:"anchor"`
	Cutoff               time.Time               `json:"cutoff"`
	// Agreement is the successor's accepted renewal terms at the boundary:
	// subscription_id is the successor, period [anchor-cycle, anchor].
	Agreement subscriptions.RenewalTerms `json:"agreement"`
}

type engineTakeoverProgress struct {
	DeleteSubmitted bool   `json:"delete_submitted,omitempty"`
	NotExecuted     string `json:"not_executed,omitempty"`
	Abandoned       bool   `json:"abandoned,omitempty"`
	Completed       bool   `json:"completed,omitempty"`
}

func DecodeNMIEngineTakeover(in gen.OpenrailsRailIntent) (NMIEngineTakeoverPayload, engineTakeoverProgress, error) {
	var p NMIEngineTakeoverPayload
	var g engineTakeoverProgress
	if in.IntentType != TypeNMIEngineTakeover {
		return p, g, errors.New("not an engine takeover operation")
	}
	if err := json.Unmarshal(in.Payload, &p); err != nil {
		return p, g, err
	}
	if len(in.ResultEvidence) > 0 {
		if err := json.Unmarshal(in.ResultEvidence, &g); err != nil {
			return p, g, err
		}
	}
	return p, g, nil
}

// NMIEngineTakeover is the intent handler and its admission surface.
type NMIEngineTakeover struct {
	DB       *db.DB
	Resolver NMIClientResolver
	Clock    clockwork.Clock
}

func (*NMIEngineTakeover) Type() string                  { return TypeNMIEngineTakeover }
func (*NMIEngineTakeover) Backoff(n int32) time.Duration { return DefaultBackoff.Delay(n) }
func (*NMIEngineTakeover) PrunePolicy() (bool, bool)     { return true, true }
func (*NMIEngineTakeover) CommitsTerminalOutcome() bool  { return true }
func (h *NMIEngineTakeover) now() time.Time              { return h.Clock.Now().UTC() }

func (h *NMIEngineTakeover) CheckRelevance(context.Context, gen.OpenrailsRailIntent) (Relevance, error) {
	return StillRelevant(), nil
}

// freeze qualifies the legacy subscription and returns the operation terms.
func (h *NMIEngineTakeover) freeze(ctx context.Context, d *db.DB, sub *models.Subscription, now time.Time) (NMIEngineTakeoverPayload, error) {
	var p NMIEngineTakeoverPayload
	switch {
	case sub.Rail != models.RailNMI:
		return p, ineligible("only NMI subscriptions can be taken over")
	case sub.CollectionPolicy == models.CollectionPolicyEngine:
		return p, ineligible("subscription is already billed by OpenRails")
	case sub.CollectionPolicy != models.CollectionPolicyProvider && sub.CollectionPolicy != models.CollectionPolicyProviderDunning:
		return p, ineligible("unsupported collection policy %q", sub.CollectionPolicy)
	case sub.Status != models.StatusActive:
		return p, ineligible("only an active subscription can be taken over (status %s)", sub.Status)
	case strings.TrimSpace(sub.RailSubscriptionID) == "":
		return p, ineligible("subscription has no NMI schedule")
	case sub.ScheduledPriceID != nil || sub.DeletionScheduledAt != nil:
		return p, ineligible("subscription has a scheduled change or cancellation")
	case sub.CurrentPeriodEndsAt == nil:
		return p, ineligible("subscription has no paid-through date")
	case sub.PaymentMethodID == nil:
		return p, takeoverRefusal(http.StatusConflict, openrails.CodeEngineTakeoverNoAgreement, "subscription has no saved payment method")
	}
	anchor := sub.CurrentPeriodEndsAt.UTC()
	cutoff := anchor.Add(-EngineTakeoverMargin)
	if !now.Before(cutoff) {
		return p, takeoverRefusal(http.StatusConflict, openrails.CodeEngineTakeoverBoundaryTooClose, "NMI bills the period starting %s; take over at least %s before it", anchor.Format(time.RFC3339), EngineTakeoverMargin)
	}
	q := d.Gen(ctx)
	method, err := q.GetPaymentMethodByID(ctx, gen.GetPaymentMethodByIDParams{MerchantID: sub.MerchantID, ID: *sub.PaymentMethodID})
	if err != nil {
		return p, err
	}
	instrument := charge.FreezeInstrument(method)
	if method.CustomerID != sub.CustomerID || method.PspID != sub.PspID || method.Rail != string(sub.Rail) || method.ParkReason != "" || method.Custodian != models.CustodianPSP {
		return p, takeoverRefusal(http.StatusConflict, openrails.CodeEngineTakeoverNoAgreement, "saved payment method is not this subscription's NMI vault card")
	}
	if err := charge.ValidateEngineInstrument(method.Rail, instrument, nil, true); err != nil {
		return p, takeoverRefusal(http.StatusConflict, openrails.CodeEngineTakeoverNoAgreement, "no verified recurring stored-credential agreement: %v", err)
	}
	price, err := catalog.NewPriceService(d).GetByID(ctx, sub.PriceID)
	if err != nil {
		return p, err
	}
	cycle := price.RecurringCycleHours()
	if cycle == nil || *cycle <= 0 {
		return p, ineligible("price has no recurring cadence")
	}
	if price.ProductID != sub.ProductID {
		return p, ineligible("subscription price and product disagree")
	}
	currency := strings.ToUpper(strings.TrimSpace(price.Currency))
	minor, err := moneyutil.NativeToRailMinorExact(currency, price.Amount)
	if err != nil || minor <= 0 {
		return p, ineligible("price amount is not an exact positive %s amount", currency)
	}
	product, err := catalog.NewProductService(d).GetByID(ctx, sub.ProductID)
	if err != nil {
		return p, err
	}
	benefits := models.CloneEntitlementsSpec(sub.EntitlementsSpecSnapshot)
	if len(benefits) == 0 {
		benefits = models.CloneEntitlementsSpec(product.EntitlementsSpec)
	}
	if benefits == nil {
		benefits = map[string]*int{}
	}
	var paid uuid.UUID
	err = d.Qx(ctx).QueryRow(ctx, `SELECT id FROM openrails.payments WHERE merchant_id=$1 AND subscription_id=$2 AND status='completed'
		AND deleted_at IS NULL AND reversal_kind IS NULL AND amount > 0 ORDER BY purchased_at DESC, id DESC LIMIT 1`, sub.MerchantID, sub.ID).Scan(&paid)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, ineligible("no completed payment pays the current period")
	}
	if err != nil {
		return p, err
	}
	p = NMIEngineTakeoverPayload{
		LegacySubscriptionID: sub.ID, LegacyPolicy: sub.CollectionPolicy, RailSubscriptionID: strings.TrimSpace(sub.RailSubscriptionID),
		PaymentMethodID: method.ID, Instrument: instrument, LegacyPaymentID: paid, AmountMinor: int64(minor), Anchor: anchor, Cutoff: cutoff,
		Agreement: subscriptions.RenewalTerms{
			PSPID: sub.PspID, SubscriptionID: uuidutil.NewV7(), CustomerID: sub.CustomerID, FromPriceID: sub.PriceID, FromProductID: sub.ProductID,
			PriceID: sub.PriceID, ProductID: sub.ProductID, ProductName: product.DisplayName, Amount: price.Amount, Currency: currency,
			PeriodStart: anchor.Add(-time.Duration(*cycle) * time.Hour), PeriodEnd: anchor,
			Entitlements: benefits, PreviousEntitlements: models.CloneEntitlementsSpec(benefits),
		},
	}
	return p, p.Agreement.Validate()
}

// sameLegacy reports whether the locked legacy row is still the obligation
// the operation froze.
func sameLegacy(sub *models.Subscription, p NMIEngineTakeoverPayload) bool {
	return sub.ID == p.LegacySubscriptionID && sub.Status == models.StatusActive && sub.CollectionPolicy == p.LegacyPolicy &&
		strings.TrimSpace(sub.RailSubscriptionID) == p.RailSubscriptionID && sub.PriceID == p.Agreement.PriceID && sub.PspID == p.Agreement.PSPID &&
		sub.PaymentMethodID != nil && *sub.PaymentMethodID == p.PaymentMethodID && sub.CurrentPeriodEndsAt != nil && sub.CurrentPeriodEndsAt.UTC().Equal(p.Anchor) &&
		sub.ScheduledPriceID == nil && sub.DeletionScheduledAt == nil
}

func (h *NMIEngineTakeover) Preview(ctx context.Context, id uuid.UUID) (*openrails.EngineTakeover, error) {
	sub, err := subscriptions.NewSubscriptionRepo(h.DB).GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	p, err := h.freeze(ctx, h.DB, sub, h.now())
	if err != nil {
		return nil, err
	}
	return takeoverResult(gen.OpenrailsRailIntent{Status: "ready"}, p, engineTakeoverProgress{}), nil
}

func takeoverKey(key string) (string, error) {
	if key == "" || key != strings.TrimSpace(key) || len(key) > 200 || strings.ContainsAny(key, "\r\n\t") {
		return "", openrails.ErrInvalid
	}
	return TypeNMIEngineTakeover + ":" + key, nil
}

// admit records one takeover operation under the subscription lock; an
// existing key replays its original operation.
func (h *NMIEngineTakeover) admit(ctx context.Context, runner *Runner, id uuid.UUID, key string, origin Origin, reason string) (gen.OpenrailsRailIntent, error) {
	var in gen.OpenrailsRailIntent
	err := h.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := h.DB.NewWithPgxTx(tx)
		store := NewStore(d)
		if runner != nil {
			if base, ok := runner.Store.(*Store); ok {
				store = base.withTxDB(d)
			}
		}
		if err := d.Gen(ctx).LockProviderCutoverRequest(ctx, key); err != nil {
			return err
		}
		old, err := store.GetByIdempotencyKey(ctx, key)
		if err == nil {
			if old.IntentType != TypeNMIEngineTakeover || old.SubscriptionID == nil || *old.SubscriptionID != id {
				return takeoverRefusal(http.StatusConflict, openrails.CodeEngineTakeoverConflict, "idempotency key was used for another operation")
			}
			in = old
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		sub, err := subscriptions.NewSubscriptionRepo(d).GetByIDForUpdate(ctx, id)
		if err != nil {
			return err
		}
		busy, err := d.Gen(ctx).HasOpenProviderCutoverSubscriptionIntent(ctx, gen.HasOpenProviderCutoverSubscriptionIntentParams{MerchantID: sub.MerchantID, SubscriptionID: id})
		if err != nil {
			return err
		}
		if busy {
			return takeoverRefusal(http.StatusConflict, openrails.CodeEngineTakeoverInFlight, "subscription has an unresolved provider operation")
		}
		p, err := h.freeze(ctx, d, sub, h.now())
		if err != nil {
			return err
		}
		in, err = store.Enqueue(ctx, EnqueueParams{MerchantID: sub.MerchantID, Provider: string(models.RailNMI), IntentType: TypeNMIEngineTakeover, SubscriptionID: &id, PriceID: &sub.PriceID,
			PspID: sub.PspID, Payload: p, IdempotencyKey: key, NextAttemptAt: h.now(), Origin: origin, OriginReason: reason})
		return err
	})
	return in, err
}

func (h *NMIEngineTakeover) result(in gen.OpenrailsRailIntent) (*openrails.EngineTakeover, error) {
	p, g, err := DecodeNMIEngineTakeover(in)
	if err != nil {
		return nil, err
	}
	return takeoverResult(in, p, g), nil
}

// Submit admits and runs one takeover now (held by the destructive switch or
// breaker, it stays pending and the executor resumes it).
func (h *NMIEngineTakeover) Submit(ctx context.Context, runner *Runner, id uuid.UUID, key string, origin Origin) (*openrails.EngineTakeover, error) {
	k, err := takeoverKey(key)
	if err != nil {
		return nil, err
	}
	in, err := h.admit(ctx, runner, id, k, origin, "engine billing takeover")
	if err != nil {
		return nil, err
	}
	if in.Status == StatusUnknownNeedsVerify {
		if in, err = runner.VerifyByID(ctx, in.ID); err != nil {
			return nil, err
		}
	}
	if in, err = runner.ExecuteByID(ctx, in.ID); err != nil {
		return nil, err
	}
	return h.result(in)
}

// Batch admits takeovers for eligible legacy NMI subscriptions, earliest
// boundary first. Admitted operations drain through the executor, whose
// destructive switch and volume breaker cap how many run.
func (h *NMIEngineTakeover) Batch(ctx context.Context, runner *Runner, req openrails.EngineTakeoverBatchRequest) (*openrails.EngineTakeoverBatchResult, error) {
	limit := req.MaxSubscriptions
	if limit <= 0 || limit > EngineTakeoverBatchMax {
		return nil, takeoverRefusal(http.StatusBadRequest, "invalid_param", "max_subscriptions must be 1..%d", EngineTakeoverBatchMax)
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	var price *uuid.UUID
	if strings.TrimSpace(req.PriceID) != "" {
		pid, err := openrails.ParsePriceID(req.PriceID)
		if err != nil || pid.IsZero() {
			return nil, takeoverRefusal(http.StatusBadRequest, "invalid_param", "invalid price_id")
		}
		u := pid.UUID()
		price = &u
	}
	now := h.now()
	rows, err := h.DB.Qx(ctx).Query(ctx, `SELECT s.id, s.current_period_ends_at FROM openrails.subscriptions s
		WHERE s.merchant_id=$1 AND s.rail='nmi' AND s.collection_policy IN ('provider','provider_dunning') AND s.status='active'
		  AND s.deleted_at IS NULL AND s.rail_subscription_id<>'' AND s.scheduled_price_id IS NULL AND s.deletion_scheduled_at IS NULL
		  AND s.current_period_ends_at > $2 AND ($3::uuid IS NULL OR s.price_id=$3::uuid)
		  AND NOT EXISTS (SELECT 1 FROM openrails.rail_intents i WHERE i.merchant_id=s.merchant_id AND i.subscription_id=s.id
		      AND i.status NOT IN ('succeeded','failed_terminal','superseded','expired'))
		ORDER BY s.current_period_ends_at, s.id LIMIT $4`, mid.UUID(), now.Add(EngineTakeoverMargin), price, limit)
	if err != nil {
		return nil, err
	}
	type candidate struct {
		id  uuid.UUID
		end time.Time
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.end); err != nil {
			rows.Close()
			return nil, err
		}
		candidates = append(candidates, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := &openrails.EngineTakeoverBatchResult{Admitted: []openrails.EngineTakeover{}, Refused: []openrails.EngineTakeoverRefusal{}}
	for _, c := range candidates {
		key := fmt.Sprintf("%s:batch:%s:%d", TypeNMIEngineTakeover, c.id, c.end.UTC().Unix())
		in, err := h.admit(ctx, runner, c.id, key, OriginSystem, "bulk engine billing takeover")
		var refusal *EngineTakeoverRefusal
		switch {
		case errors.As(err, &refusal):
			out.Refused = append(out.Refused, openrails.EngineTakeoverRefusal{SubscriptionID: openrails.SubscriptionID(c.id), Code: refusal.Code, Reason: refusal.Message})
			continue
		case errors.Is(err, ErrRateCeilingTripped):
			out.Refused = append(out.Refused, openrails.EngineTakeoverRefusal{SubscriptionID: openrails.SubscriptionID(c.id), Code: openrails.CodeEngineTakeoverRateLimited, Reason: err.Error()})
			return out, nil
		case err != nil:
			return nil, err
		}
		r, err := h.result(in)
		if err != nil {
			return nil, err
		}
		out.Admitted = append(out.Admitted, *r)
	}
	return out, nil
}

// latest returns the newest takeover operation for the legacy subscription.
func (h *NMIEngineTakeover) latest(ctx context.Context, id uuid.UUID) (gen.OpenrailsRailIntent, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return gen.OpenrailsRailIntent{}, err
	}
	var opID uuid.UUID
	err = h.DB.Qx(ctx).QueryRow(ctx, `SELECT id FROM openrails.rail_intents WHERE merchant_id=$1 AND intent_type=$2 AND subscription_id=$3
		ORDER BY created_at DESC, id DESC LIMIT 1`, mid.UUID(), TypeNMIEngineTakeover, id).Scan(&opID)
	if errors.Is(err, pgx.ErrNoRows) {
		return gen.OpenrailsRailIntent{}, takeoverRefusal(http.StatusNotFound, openrails.CodeEngineTakeoverNotFound, "no engine takeover for this subscription")
	}
	if err != nil {
		return gen.OpenrailsRailIntent{}, err
	}
	return NewStore(h.DB).Get(ctx, opID)
}

func (h *NMIEngineTakeover) Get(ctx context.Context, id uuid.UUID) (*openrails.EngineTakeover, error) {
	in, err := h.latest(ctx, id)
	if err != nil {
		return nil, err
	}
	return h.result(in)
}

// Abandon ends a takeover that has sent nothing to NMI; the legacy
// subscription stays NMI-billed. Once the schedule delete was submitted the
// takeover can only complete.
func (h *NMIEngineTakeover) Abandon(ctx context.Context, id uuid.UUID) (*openrails.EngineTakeover, error) {
	in, err := h.latest(ctx, id)
	if err != nil {
		return nil, err
	}
	_, g, err := DecodeNMIEngineTakeover(in)
	if err != nil {
		return nil, err
	}
	switch {
	case g.Abandoned:
		return h.result(in)
	case g.DeleteSubmitted || in.Status == StatusSucceeded:
		return nil, takeoverRefusal(http.StatusConflict, openrails.CodeEngineTakeoverCommitted, "the NMI schedule delete was already submitted; the takeover can only complete")
	case in.Status == StatusInFlight || in.Status == StatusUnknownNeedsVerify:
		return nil, takeoverRefusal(http.StatusConflict, openrails.CodeEngineTakeoverInFlight, "the takeover is executing; read it again")
	case OperationTerminal(in.Status):
		return h.result(in)
	}
	evidence, _ := json.Marshal(engineTakeoverProgress{Abandoned: true, NotExecuted: "abandoned"})
	tag, err := h.DB.Qx(ctx).Exec(ctx, `UPDATE openrails.rail_intents SET status='failed_terminal', last_failure_reason='abandoned before any NMI change',
		result_evidence=coalesce(result_evidence,'{}'::jsonb) || $3::jsonb, claimed_until=NULL, updated_at=now()
		WHERE id=$1 AND merchant_id=$2 AND intent_type='nmi_engine_takeover' AND status IN ('pending','failed_retryable')
		  AND NOT (coalesce(result_evidence,'{}'::jsonb) ? 'delete_submitted')`, in.ID, in.MerchantID, evidence)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() != 1 {
		return nil, takeoverRefusal(http.StatusConflict, openrails.CodeEngineTakeoverInFlight, "the takeover changed state; read it again")
	}
	return h.Get(ctx, id)
}

func takeoverResult(in gen.OpenrailsRailIntent, p NMIEngineTakeoverPayload, g engineTakeoverProgress) *openrails.EngineTakeover {
	r := &openrails.EngineTakeover{ID: in.ID, SubscriptionID: openrails.SubscriptionID(p.LegacySubscriptionID), RailSubscriptionID: p.RailSubscriptionID,
		Anchor: p.Anchor, Cutoff: p.Cutoff, Amount: p.Agreement.Amount, Currency: p.Agreement.Currency, Status: in.Status, Stage: "pending"}
	if in.LastFailureReason != nil {
		r.Reason = *in.LastFailureReason
	}
	switch {
	case in.Status == "ready":
		r.Stage = "ready"
	case in.Status == StatusSucceeded:
		r.Stage = "completed"
		successor := openrails.SubscriptionID(p.Agreement.SubscriptionID)
		r.SuccessorSubscriptionID = &successor
	case g.Abandoned:
		r.Stage = "abandoned"
	case g.NotExecuted != "":
		r.Stage = "not_executed"
	case g.DeleteSubmitted:
		r.Stage = "delete_submitted"
	case in.Status == StatusPending && in.LastFailureReason != nil:
		r.Stage = "held"
	}
	return r
}

func (h *NMIEngineTakeover) Execute(ctx context.Context, in gen.OpenrailsRailIntent) Outcome {
	return h.advance(ctx, in, true)
}

func (h *NMIEngineTakeover) Verify(ctx context.Context, in gen.OpenrailsRailIntent) Outcome {
	return h.advance(ctx, in, false)
}

// notExecuted ends the operation before any NMI write; the legacy
// subscription stays NMI-billed.
func (h *NMIEngineTakeover) notExecuted(ctx context.Context, in gen.OpenrailsRailIntent, code, reason string) Outcome {
	evidence := map[string]any{"not_executed": code}
	wctx, cancel := LedgerWriteContext(ctx)
	defer cancel()
	if err := NewStore(h.DB).MarkFailedTerminal(wctx, in.ID, reason, evidence); err != nil {
		return Ambiguous("record not-executed takeover: " + err.Error())
	}
	return TerminalWithEvidence(reason, evidence)
}

func (h *NMIEngineTakeover) drift(ctx context.Context, in gen.OpenrailsRailIntent, p NMIEngineTakeoverPayload, detail map[string]any) Outcome {
	detail["rail_subscription_id"] = p.RailSubscriptionID
	detail["subscription_id"] = openrails.SubscriptionID(p.LegacySubscriptionID).String()
	raw, _ := json.Marshal(detail)
	action := "The NMI schedule no longer matches the local subscription; reconcile it before taking it over"
	if _, err := h.DB.Gen(ctx).UpsertReconciliationFinding(ctx, gen.UpsertReconciliationFindingParams{MerchantID: in.MerchantID, FindingType: EngineTakeoverDriftFinding,
		SubjectKey: p.LegacySubscriptionID.String(), Severity: "high", Status: "requires_review", RecommendedAction: &action, Evidence: raw}); err != nil {
		return Retryable("record schedule drift finding: " + err.Error())
	}
	return h.notExecuted(ctx, in, "schedule_drift", fmt.Sprintf("NMI schedule drift: %v", detail))
}

func (h *NMIEngineTakeover) advance(ctx context.Context, in gen.OpenrailsRailIntent, send bool) Outcome {
	current, err := NewStore(h.DB).Get(ctx, in.ID)
	if err != nil {
		return Ambiguous("read takeover: " + err.Error())
	}
	p, g, err := DecodeNMIEngineTakeover(current)
	if err != nil {
		return Ambiguous("invalid takeover: " + err.Error())
	}
	if in.SubscriptionID == nil || *in.SubscriptionID != p.LegacySubscriptionID || in.PspID == nil || *in.PspID != p.Agreement.PSPID {
		return Ambiguous("frozen takeover address mismatch")
	}
	client, ok, err := resolveIntentNMIClient(ctx, h.Resolver, in)
	if err != nil || !ok || client == nil {
		if send && !g.DeleteSubmitted {
			return Parked("nmi rail is not armed for this account")
		}
		return Ambiguous("nmi rail is not armed; cannot read the schedule")
	}
	if !g.DeleteSubmitted {
		if !send {
			return Retryable("no NMI write was submitted")
		}
		if !h.now().Before(p.Cutoff) {
			return h.notExecuted(ctx, in, "boundary_too_close", "NMI bills the period at the boundary; the takeover cutoff passed before it ran")
		}
		sub, err := subscriptions.NewSubscriptionRepo(h.DB).GetByID(ctx, p.LegacySubscriptionID)
		if err != nil {
			return Retryable("load subscription: " + err.Error())
		}
		if !sameLegacy(sub, p) {
			return h.notExecuted(ctx, in, "subscription_changed", "the subscription changed after the takeover was admitted")
		}
		if client.ReadOnly {
			return Parked("nmi client is read-only")
		}
		schedule, found, err := client.GetSubscription(ctx, p.RailSubscriptionID)
		if err != nil {
			return Retryable("read NMI schedule: " + err.Error())
		}
		if !found {
			return h.drift(ctx, in, p, map[string]any{"schedule": "missing"})
		}
		detail := map[string]any{}
		if v := strings.TrimSpace(schedule.CustomerVaultID); v != p.Instrument.RailCustomerRef {
			detail["customer_vault_id"] = v
		}
		if amount, err := nmi.SubscriptionAmountMinor(schedule, p.Agreement.Currency); err != nil || int64(amount) != p.AmountMinor {
			detail["amount"] = formatScheduleAmount(schedule.Amount)
		}
		if next := strings.TrimSpace(schedule.NextBillingDate); len(next) < 10 || next[:10] != p.Anchor.Format("2006-01-02") {
			detail["next_billing_date"] = next
		}
		if paused := strings.TrimSpace(fmt.Sprint(schedule.PausedSubscription)); paused == "1" || paused == "true" {
			detail["paused"] = true
		}
		if len(detail) > 0 {
			return h.drift(ctx, in, p, detail)
		}
		wctx, cancel := LedgerWriteContext(ctx)
		first, err := NewStore(h.DB).RecordProgressIfAbsent(wctx, in.ID, "delete_submitted", true)
		cancel()
		if err != nil {
			return Retryable("record delete marker: " + err.Error())
		}
		if !first {
			return Ambiguous("delete marker already recorded; verify the schedule")
		}
		if err := client.DeleteRecurringSubscription(ctx, p.RailSubscriptionID); err != nil && !errors.Is(err, nmi.ErrV5NotFound) {
			return Ambiguous("delete NMI schedule: " + err.Error())
		}
	}
	_, found, err := client.GetSubscription(ctx, p.RailSubscriptionID)
	if err != nil {
		return Ambiguous("read NMI schedule after delete: " + err.Error())
	}
	if found {
		if !send {
			return Retryable("NMI schedule is still live; delete verified not executed")
		}
		return Ambiguous("NMI schedule still live after delete")
	}
	return h.commit(ctx, in, p)
}

// commit ends the legacy row and opens the engine successor, marking the
// operation succeeded in the same transaction: it is the successor's paid
// agreement at the boundary.
func (h *NMIEngineTakeover) commit(ctx context.Context, in gen.OpenrailsRailIntent, p NMIEngineTakeoverPayload) Outcome {
	now := h.now()
	evidence := map[string]any{"delete_submitted": true, "completed": true, "successor_subscription_id": p.Agreement.SubscriptionID.String()}
	err := h.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := h.DB.NewWithPgxTx(tx)
		repo := subscriptions.NewSubscriptionRepo(d)
		legacy, err := repo.GetByIDForUpdate(ctx, p.LegacySubscriptionID)
		if err != nil {
			return err
		}
		if _, err := repo.GetByID(ctx, p.Agreement.SubscriptionID); err == nil {
			return NewStore(d).MarkSucceeded(ctx, in.ID, now, evidence)
		} else if !db.IsNotFound(err) {
			return err
		}
		if !sameLegacy(legacy, p) {
			return errors.New("legacy subscription changed after its NMI schedule was deleted; operator review required")
		}
		kind := models.CancelTypeEngineTakeover
		legacy.Status, legacy.CancelType, legacy.CancelledAt = models.StatusCancelled, &kind, &now
		legacy.ClearRetrySchedule()
		if err := repo.UpdateAt(ctx, legacy, now); err != nil {
			return err
		}
		// The legacy membership's access ends where the successor's paid
		// period begins: bounded here, never left for a sweep to close.
		if err := entitlements.NewEntitlementService(d, h.Clock).BoundSubscriptionAccess(ctx, legacy.ID, p.Anchor); err != nil {
			return fmt.Errorf("bound legacy access: %w", err)
		}
		start, end, method := p.Agreement.PeriodStart, p.Agreement.PeriodEnd, p.PaymentMethodID
		successor := &models.Subscription{
			ID: p.Agreement.SubscriptionID, MerchantID: legacy.MerchantID, CustomerID: legacy.CustomerID, ProductID: p.Agreement.ProductID, PriceID: p.Agreement.PriceID,
			CollectionPolicy: models.CollectionPolicyEngine, EntitlementsSpecSnapshot: models.CloneEntitlementsSpec(p.Agreement.Entitlements),
			Status: models.StatusActive, StartedAt: now, CurrentPeriodStartsAt: &start, CurrentPeriodEndsAt: &end,
			Rail: models.RailNMI, PspID: p.Agreement.PSPID, UserEmail: legacy.UserEmail, PaymentMethodID: &method, CreatedAt: now, UpdatedAt: now,
		}
		if err := repo.Create(ctx, successor); err != nil {
			return err
		}
		return NewStore(d).MarkSucceeded(ctx, in.ID, now, evidence)
	})
	if err != nil {
		return Ambiguous("NMI schedule deleted; local takeover commit pending: " + err.Error())
	}
	return Succeeded(evidence)
}

// TakeoverAgreement qualifies a succeeded takeover as the successor's paid
// engine agreement at its boundary.
func TakeoverAgreement(ctx context.Context, d *db.DB, sub *models.Subscription, op gen.OpenrailsRailIntent) (subscriptions.RenewalTerms, error) {
	p, _, err := DecodeNMIEngineTakeover(op)
	if err != nil {
		return subscriptions.RenewalTerms{}, err
	}
	a := p.Agreement
	if op.Status != StatusSucceeded || a.SubscriptionID != sub.ID || a.CustomerID != sub.CustomerID || op.SubscriptionID == nil || *op.SubscriptionID != p.LegacySubscriptionID {
		return a, errors.New("takeover agreement does not own this obligation")
	}
	payment, err := d.Gen(ctx).GetPaymentByID(ctx, gen.GetPaymentByIDParams{MerchantID: op.MerchantID, ID: p.LegacyPaymentID})
	if err != nil {
		return a, fmt.Errorf("legacy paid period: %w", err)
	}
	if string(payment.Status) != "completed" || payment.SubscriptionID == nil || *payment.SubscriptionID != p.LegacySubscriptionID || payment.CustomerID != sub.CustomerID || payment.Amount <= 0 {
		return a, errors.New("legacy payment no longer pays the taken-over period")
	}
	return a, nil
}

// formatScheduleAmount is NMI's decimal amount text as the schedule holds it.
func formatScheduleAmount(raw string) string { return strings.TrimSpace(raw) }
