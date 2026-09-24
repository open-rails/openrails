package intents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/providerqualification"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/pkg/merchant"
)

const TypeNMIProviderCutover = "nmi_provider_cutover"

var ErrProviderCutoverConflict = errors.New("provider cutover conflict")

type nmiCutoverRequest struct {
	TargetPaymentMethodID uuid.UUID `json:"target_payment_method_id"`
	ExpectedSourcePSPID   uuid.UUID `json:"expected_source_psp_id"`
	ExpectedTargetPSPID   uuid.UUID `json:"expected_target_psp_id"`
}

func normalizeCutoverRequest(body openrails.ProviderCutoverRequest) (nmiCutoverRequest, error) {
	id := body.TargetPaymentMethodID.UUID()
	if id == uuid.Nil || body.ExpectedSourcePSPID == uuid.Nil || body.ExpectedTargetPSPID == uuid.Nil {
		return nmiCutoverRequest{}, openrails.ErrInvalid
	}
	return nmiCutoverRequest{id, body.ExpectedSourcePSPID, body.ExpectedTargetPSPID}, nil
}

type nmiCutoverPayload struct {
	SourceQualification         providerqualification.BoundRecord `json:"source_qualification"`
	TargetQualification         providerqualification.BoundRecord `json:"target_qualification"`
	Request                     nmiCutoverRequest                 `json:"request"`
	CustomerID                  uuid.UUID                         `json:"customer_id"`
	SubscriptionID              uuid.UUID                         `json:"subscription_id"`
	SourceSubscriptionID        string                            `json:"source_subscription_id"`
	SourcePaymentMethodID       uuid.UUID                         `json:"source_payment_method_id"`
	SourceInstrument            charge.FrozenInstrument           `json:"source_instrument"`
	PriceID                     uuid.UUID                         `json:"price_id"`
	PlanID                      string                            `json:"plan_id"`
	TargetInstrument            charge.FrozenInstrument           `json:"target_instrument"`
	Currency                    string                            `json:"currency"`
	Amount                      int64                             `json:"amount"`
	CycleHours                  int32                             `json:"cycle_hours"`
	PeriodStart                 time.Time                         `json:"period_start"`
	PeriodEnd                   time.Time                         `json:"period_end"`
	Anchor                      time.Time                         `json:"anchor"`
	SourceCredentialFingerprint string                            `json:"source_credential_fingerprint"`
	TargetCredentialFingerprint string                            `json:"target_credential_fingerprint"`
}

type nmiCutoverDecision struct {
	Action        string         `json:"action"`
	Authorization map[string]any `json:"authorization,omitempty"`
}

type nmiCutoverProgress struct {
	Decision              *nmiCutoverDecision `json:"decision,omitempty"`
	TargetCancelSubmitted bool                `json:"target_cancel_submitted,omitempty"`
	TargetCancelReceipt   *nmi.V5Subscription `json:"target_cancel_receipt,omitempty"`
	Abandoned             bool                `json:"abandoned,omitempty"`
	CreateSubmitted       bool                `json:"create_submitted,omitempty"`
	Target                *nmi.V5Subscription `json:"target,omitempty"`
	SourceCancelSubmitted bool                `json:"source_cancel_submitted,omitempty"`
	SourceReceipt         *nmi.V5Subscription `json:"source_receipt,omitempty"`
	SourceAbsentAt        time.Time           `json:"source_absent_at,omitzero"`
	ActivatedTarget       *nmi.V5Subscription `json:"activated_target,omitempty"`
	SourceCanceled        bool                `json:"source_canceled,omitempty"`
	ActivationSubmitted   bool                `json:"activation_submitted,omitempty"`
	ActivationAnchor      time.Time           `json:"activation_anchor,omitzero"`
	TargetActive          bool                `json:"target_active,omitempty"`
	Resolution            map[string]any      `json:"resolution,omitempty"`
	BillingAnchor         time.Time           `json:"billing_anchor,omitzero"`
	PausedAnchor          time.Time           `json:"paused_anchor,omitzero"`
	AnchorResolutions     []map[string]any    `json:"anchor_resolutions,omitempty"`
	NotExecuted           bool                `json:"not_executed,omitempty"`
	NotExecutedCode       string              `json:"not_executed_code,omitempty"`
}

// NMIProviderCutover uses the existing intent ledger. It retains the complete
// frozen operation and receipts after success for exact replay and forensics.
type NMIProviderCutover struct {
	DB       *db.DB
	Resolver NMIClientResolver
	Clock    clockwork.Clock
}

func (*NMIProviderCutover) Type() string                  { return TypeNMIProviderCutover }
func (*NMIProviderCutover) Backoff(n int32) time.Duration { return DefaultBackoff.Delay(n) }
func (*NMIProviderCutover) PrunePolicy() (bool, bool)     { return true, true }
func (*NMIProviderCutover) CheckRelevance(context.Context, gen.OpenrailsRailIntent) (Relevance, error) {
	return StillRelevant(), nil
}
func (h *NMIProviderCutover) now() time.Time {
	return h.Clock.Now().UTC()
}
func cutoverConflict(reason string) error {
	return fmt.Errorf("%w: %s", ErrProviderCutoverConflict, reason)
}
func cutoverKey(key string) (string, error) {
	if key == "" || key != strings.TrimSpace(key) || len(key) > 255 || strings.ContainsAny(key, "\r\n\t") {
		return "", openrails.ErrInvalid
	}
	return TypeNMIProviderCutover + ":" + key, nil
}

// freeze runs under the subscription lock for admission; later checks compare
// these terms to the durable decision without recalculating an expired anchor.
func (h *NMIProviderCutover) freeze(ctx context.Context, d *db.DB, id uuid.UUID, req nmiCutoverRequest) (nmiCutoverPayload, error) {
	p := nmiCutoverPayload{Request: req, SubscriptionID: id}
	if id == uuid.Nil || req.TargetPaymentMethodID == uuid.Nil || req.ExpectedSourcePSPID == uuid.Nil || req.ExpectedTargetPSPID == uuid.Nil || req.ExpectedSourcePSPID == req.ExpectedTargetPSPID {
		return p, cutoverConflict("distinct nonzero source and target PSPs and replacement card required; to move NMI billing to OpenRails on the same account use the engine takeover (POST .../engine-takeover)")
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return p, err
	}
	snapshot, err := d.Gen(ctx).GetNMIProviderCutoverSnapshot(ctx, gen.GetNMIProviderCutoverSnapshotParams{
		SubscriptionID: id, MerchantID: mid.UUID(), TargetPaymentMethodID: req.TargetPaymentMethodID,
		SourcePspID: req.ExpectedSourcePSPID, TargetPspID: req.ExpectedTargetPSPID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return p, cutoverConflict("subscription, accounts, target card or target price binding is not eligible")
	}
	if err != nil {
		return p, err
	}
	if snapshot.PaymentMethodID == nil || snapshot.PriceID == nil || snapshot.PlanID == nil ||
		snapshot.AccessDurationHours == nil || snapshot.CurrentPeriodStartsAt == nil || snapshot.CurrentPeriodEndsAt == nil {
		return p, errors.New("cutover snapshot is missing required commercial terms")
	}
	p.CustomerID, p.SourceSubscriptionID = snapshot.CustomerID, snapshot.RailSubscriptionID
	p.SourcePaymentMethodID, p.PriceID, p.PlanID = *snapshot.PaymentMethodID, *snapshot.PriceID, *snapshot.PlanID
	p.Currency, p.Amount, p.CycleHours = snapshot.Currency, snapshot.Amount, *snapshot.AccessDurationHours
	p.PeriodStart, p.PeriodEnd = *snapshot.CurrentPeriodStartsAt, *snapshot.CurrentPeriodEndsAt
	sourceMethod, err := d.Gen(ctx).GetPaymentMethodByID(ctx, gen.GetPaymentMethodByIDParams{MerchantID: mid.UUID(), ID: p.SourcePaymentMethodID})
	if err != nil {
		return p, err
	}
	targetMethod, err := d.Gen(ctx).GetPaymentMethodByID(ctx, gen.GetPaymentMethodByIDParams{MerchantID: mid.UUID(), ID: req.TargetPaymentMethodID})
	if err != nil {
		return p, err
	}
	p.SourceInstrument = charge.FreezeInstrument(sourceMethod)
	p.TargetInstrument = charge.FreezeInstrument(targetMethod)
	if p.SourceInstrument.Validate() != nil || p.TargetInstrument.Validate() != nil ||
		p.SourceInstrument.CustodianHeld() || p.TargetInstrument.CustodianHeld() ||
		p.SourceInstrument.PSPID != req.ExpectedSourcePSPID || p.TargetInstrument.PSPID != req.ExpectedTargetPSPID ||
		sourceMethod.CustomerID != p.CustomerID || targetMethod.CustomerID != p.CustomerID {
		return p, cutoverConflict("source or target instrument does not belong to the admitted customer and account")
	}
	p.PeriodStart = p.PeriodStart.UTC()
	p.PeriodEnd = p.PeriodEnd.UTC()
	p.Currency = strings.ToLower(p.Currency)
	p.Anchor = p.PeriodEnd.UTC().Truncate(time.Second)
	if p.Anchor.Before(p.PeriodEnd) {
		p.Anchor = p.Anchor.Add(time.Second)
	}
	if p.SourceSubscriptionID == "" || p.SourceInstrument.RailCustomerRef == "" || p.PlanID == "" || p.TargetInstrument.RailCustomerRef == "" || p.TargetInstrument.RailMethodRef == "" || p.Amount <= 0 || p.CycleHours <= 0 || p.CycleHours%24 != 0 || !p.Anchor.After(p.PeriodStart) {
		return p, cutoverConflict("incomplete or unsupported commercial terms")
	}
	source, ok, err := h.Resolver.ResolveNMIClient(ctx, mid.UUID(), &req.ExpectedSourcePSPID)
	if err != nil || !ok || source == nil || source.SecurityKey == "" || !cutoverAccountMatches(source, mid.UUID(), req.ExpectedSourcePSPID) {
		return p, cutoverConflict("source credential unavailable")
	}
	target, ok, err := h.Resolver.ResolveNMIClient(ctx, mid.UUID(), &req.ExpectedTargetPSPID)
	if err != nil || !ok || target == nil || target.SecurityKey == "" || !cutoverAccountMatches(target, mid.UUID(), req.ExpectedTargetPSPID) {
		return p, cutoverConflict("target credential unavailable")
	}
	p.SourceCredentialFingerprint = cutoverCredentialFingerprint(source)
	p.TargetCredentialFingerprint = cutoverCredentialFingerprint(target)
	return p, nil
}

func cutoverAccountMatches(client *nmi.NMIClient, merchantID, pspID uuid.UUID) bool {
	boundMerchant, boundPSP := client.AccountIdentity()
	return boundMerchant == merchantID && boundPSP == pspID
}

func sameCutoverLocal(live, frozen nmiCutoverPayload) bool {
	// Qualification is authority for admission and each write, not mutable
	// commercial data. Current authority is checked separately under a row lock.
	live.SourceQualification, live.TargetQualification = frozen.SourceQualification, frozen.TargetQualification
	live.SourceCredentialFingerprint, live.TargetCredentialFingerprint = frozen.SourceCredentialFingerprint, frozen.TargetCredentialFingerprint
	return reflect.DeepEqual(live, frozen)
}

func (h *NMIProviderCutover) qualifyAdmission(ctx context.Context, database *db.DB, p *nmiCutoverPayload) error {
	source, err := providerqualification.Read(ctx, database, p.Request.ExpectedSourcePSPID)
	if err != nil {
		return err
	}
	target, err := providerqualification.Read(ctx, database, p.Request.ExpectedTargetPSPID)
	if err != nil {
		return err
	}
	if source.CredentialFingerprint != p.SourceCredentialFingerprint || target.CredentialFingerprint != p.TargetCredentialFingerprint {
		return providerqualification.ErrUnqualified
	}
	p.SourceQualification, p.TargetQualification = *source, *target
	return nil
}

func cutoverCredentialFingerprint(client *nmi.NMIClient) string {
	return providerqualification.Fingerprint(client.SecurityKey)
}

func (h *NMIProviderCutover) Preview(ctx context.Context, id uuid.UUID, body openrails.ProviderCutoverRequest) (*openrails.ProviderCutover, error) {
	req, err := normalizeCutoverRequest(body)
	if err != nil {
		return nil, err
	}
	p, err := h.freeze(ctx, h.DB, id, req)
	if err != nil {
		return nil, err
	}
	if err := h.qualifyAdmission(ctx, h.DB, &p); err != nil {
		return nil, err
	}
	if !p.Anchor.After(h.now()) {
		return nil, cutoverConflict("paid-through anchor must be in the future")
	}
	mid, _ := merchant.Require(ctx)
	return cutoverResult(gen.OpenrailsRailIntent{MerchantID: mid.UUID(), Status: "ready"}, p, nmiCutoverProgress{}), nil
}

func (h *NMIProviderCutover) Get(ctx context.Context, id uuid.UUID, key string) (*openrails.ProviderCutover, error) {
	k, err := cutoverKey(key)
	if err != nil {
		return nil, err
	}
	in, err := NewStore(h.DB).GetByIdempotencyKey(ctx, k)
	if err != nil {
		return nil, err
	}
	p, progress, err := decodeCutover(in)
	if err != nil {
		return nil, err
	}
	if p.SubscriptionID != id {
		return nil, cutoverConflict("idempotency key belongs to another subscription")
	}
	return cutoverResult(in, p, progress), nil
}

func (h *NMIProviderCutover) Submit(ctx context.Context, runner *Runner, id uuid.UUID, key string, body openrails.ProviderCutoverRequest, origin Origin) (*openrails.ProviderCutover, error) {
	req, err := normalizeCutoverRequest(body)
	if err != nil {
		return nil, err
	}
	k, err := cutoverKey(key)
	if err != nil {
		return nil, err
	}
	var in gen.OpenrailsRailIntent
	err = h.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := h.DB.NewWithPgxTx(tx)
		store := NewStore(d)
		if base, ok := runner.Store.(*Store); ok {
			store = base.withTxDB(d)
		}
		// Serialize request identity first, then subject. Replay never runs mutable
		// eligibility checks: a completed cutover already points at its target.
		if e := d.Gen(ctx).LockProviderCutoverRequest(ctx, k); e != nil {
			return e
		}
		old, e := store.GetByIdempotencyKey(ctx, k)
		if e == nil {
			p, _, e := decodeCutover(old)
			if e != nil {
				return e
			}
			if p.SubscriptionID != id || p.Request != req {
				return cutoverConflict("idempotency key was used with different inputs")
			}
			in = old
			return nil
		}
		if !errors.Is(e, pgx.ErrNoRows) {
			return e
		}
		scopeMerchantID, scopeErr := merchant.Require(ctx)
		if scopeErr != nil {
			return scopeErr
		}
		if _, e = d.Gen(ctx).GetSubscriptionByIDForUpdate(ctx, gen.GetSubscriptionByIDForUpdateParams{MerchantID: scopeMerchantID.UUID(), ID: id}); e != nil {
			return e
		}
		if e = d.Gen(ctx).LockProviderCutoverPaymentMethods(ctx, gen.LockProviderCutoverPaymentMethodsParams{
			MerchantID: pMerchant(ctx), TargetPaymentMethodID: req.TargetPaymentMethodID, SubscriptionID: id,
		}); e != nil {
			return e
		}
		p, e := h.freeze(ctx, d, id, req)
		if e != nil {
			return e
		}
		if e := h.qualifyAdmission(ctx, d, &p); e != nil {
			return e
		}
		if !p.Anchor.After(h.now()) {
			return cutoverConflict("paid-through anchor must be in the future")
		}
		busy, e := d.Gen(ctx).HasOpenProviderCutoverSubscriptionIntent(ctx, gen.HasOpenProviderCutoverSubscriptionIntentParams{
			MerchantID: pMerchant(ctx), SubscriptionID: id,
		})
		if e != nil {
			return e
		}
		if busy {
			return cutoverConflict("subscription has an unresolved provider operation")
		}
		busy, e = d.Gen(ctx).HasOpenProviderCutoverPaymentMethodIntent(ctx, gen.HasOpenProviderCutoverPaymentMethodIntentParams{
			MerchantID: pMerchant(ctx), PaymentMethodIds: []string{p.SourcePaymentMethodID.String(), req.TargetPaymentMethodID.String()},
		})
		if e != nil {
			return e
		}
		if busy {
			return cutoverConflict("payment method has an unresolved provider operation")
		}

		in, e = store.Enqueue(ctx, EnqueueParams{MerchantID: pMerchant(ctx), Provider: "nmi", IntentType: TypeNMIProviderCutover, SubscriptionID: &id, PspID: req.ExpectedTargetPSPID, Payload: p, IdempotencyKey: k, NextAttemptAt: h.now(), Origin: origin, OriginReason: "explicit provider account cutover"})
		return e
	})
	if err != nil {
		return nil, err
	}
	if in.Status == StatusUnknownNeedsVerify {
		in, err = runner.VerifyByID(ctx, in.ID)
		if err != nil {
			return nil, err
		}
	}
	in, err = runner.ExecuteByID(ctx, in.ID)
	if err != nil {
		return nil, err
	}
	p, progress, err := decodeCutover(in)
	if err != nil {
		return nil, err
	}
	return cutoverResult(in, p, progress), nil
}
func pMerchant(ctx context.Context) uuid.UUID { m, _ := merchant.Require(ctx); return m.UUID() }
func decodeCutover(in gen.OpenrailsRailIntent) (nmiCutoverPayload, nmiCutoverProgress, error) {
	var p nmiCutoverPayload
	var progress nmiCutoverProgress
	if in.IntentType != "" && in.IntentType != TypeNMIProviderCutover {
		return p, progress, cutoverConflict("wrong operation type")
	}
	if err := json.Unmarshal(in.Payload, &p); err != nil {
		return p, progress, err
	}
	if len(in.ResultEvidence) > 0 {
		if err := json.Unmarshal(in.ResultEvidence, &progress); err != nil {
			return p, progress, err
		}
	}
	return p, progress, nil
}
func cutoverResult(in gen.OpenrailsRailIntent, p nmiCutoverPayload, g nmiCutoverProgress) *openrails.ProviderCutover {
	r := &openrails.ProviderCutover{ID: in.ID, MerchantID: in.MerchantID, SubscriptionID: openrails.SubscriptionID(p.SubscriptionID), SourcePSPID: p.Request.ExpectedSourcePSPID, TargetPSPID: p.Request.ExpectedTargetPSPID, TargetPaymentMethodID: openrails.PaymentMethodID(p.Request.TargetPaymentMethodID), Anchor: p.Anchor, Status: in.Status, Stage: "pending"}
	if !g.BillingAnchor.IsZero() {
		r.Anchor = g.BillingAnchor
	}
	if in.LastFailureReason != nil {
		r.Reason = *in.LastFailureReason
	}
	if g.CreateSubmitted {
		r.Stage = "target_create_submitted"
	}
	if g.Target != nil {
		r.TargetSubscriptionID = g.Target.ID
		r.Stage = "target_paused"
	}
	if g.SourceCancelSubmitted {
		r.Stage = "source_cancel_submitted"
	}
	if g.SourceCanceled {
		r.Stage = "source_canceled"
	}
	if g.ActivationSubmitted {
		r.Stage = "target_activation_submitted"
	}
	if g.TargetActive {
		r.Stage = "target_active"
	}
	if in.Status == StatusSucceeded {
		r.Stage = "completed"
	}
	if g.NotExecuted {
		r.Stage = "not_executed"
	}
	if g.Decision != nil && g.Decision.Action == "abandon" {
		r.Stage = "abandon_requested"
		if g.TargetCancelSubmitted {
			r.Stage = "target_cancel_submitted"
		}
		if g.Abandoned {
			r.Stage = "abandoned"
		}
	}
	return r
}
func (h *NMIProviderCutover) Execute(ctx context.Context, in gen.OpenrailsRailIntent) Outcome {
	return h.advance(ctx, in, true)
}
func (h *NMIProviderCutover) Verify(ctx context.Context, in gen.OpenrailsRailIntent) Outcome {
	return h.advance(ctx, in, false)
}

func (h *NMIProviderCutover) advance(ctx context.Context, in gen.OpenrailsRailIntent, send bool) Outcome {
	store := NewStore(h.DB)
	current, err := store.Get(ctx, in.ID)
	if err != nil {
		return Ambiguous("read cutover: " + err.Error())
	}
	p, g, err := decodeCutover(current)
	if err != nil {
		return Ambiguous("invalid cutover: " + err.Error())
	}
	evidence := func() map[string]any {
		b, _ := json.Marshal(g)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		return m
	}
	uncertain := func(reason string) Outcome { return AmbiguousWithEvidence(reason, evidence()) }
	save := func() error {
		wctx, cancel := LedgerWriteContext(ctx)
		defer cancel()
		return store.RecordProgress(wctx, in.ID, evidence())
	}
	if in.PspID == nil || *in.PspID != p.Request.ExpectedTargetPSPID || in.SubscriptionID == nil || *in.SubscriptionID != p.SubscriptionID {
		return uncertain("frozen intent address mismatch")
	}
	if g.Decision != nil && g.Decision.Action == "abandon" {
		return h.advanceAbandon(ctx, current, p, g, send)
	}
	if !g.CreateSubmitted && !p.Anchor.After(h.now()) {
		g.NotExecuted, g.NotExecutedCode = true, "anchor_expired"
		return TerminalWithEvidence("billing anchor expired before any provider submission", evidence())
	}
	// After local commit only the durable receipt is needed to mark success.
	if g.TargetActive && g.SourceCanceled {
		done, e := h.isRepointed(ctx, p, g.Target)
		if e != nil {
			return uncertain(e.Error())
		}
		if done {
			return Succeeded(evidence())
		}
	}
	live, e := h.freeze(ctx, h.DB, p.SubscriptionID, p.Request)
	if e != nil {
		return uncertain("frozen local state unavailable: " + e.Error())
	}
	if !sameCutoverLocal(live, p) {
		return uncertain("local commercial terms changed since cutover admission")
	}
	terms := p
	if !g.BillingAnchor.IsZero() {
		terms.Anchor = g.BillingAnchor
	}
	target, ok, err := h.Resolver.ResolveNMIClient(ctx, in.MerchantID, &p.Request.ExpectedTargetPSPID)
	if err != nil || !ok || target == nil {
		return uncertain("target credentials unavailable")
	}
	source, ok, err := h.Resolver.ResolveNMIClient(ctx, in.MerchantID, &p.Request.ExpectedSourcePSPID)
	if err != nil || !ok || source == nil {
		return uncertain("source credentials unavailable; cancellation is unproven")
	}
	bindings, _, bindingErr := cutoverAccountBindings(current, p)
	if bindingErr != nil || !h.boundCutoverClient(ctx, source, in.MerchantID, bindings["source"]) || !h.boundCutoverClient(ctx, target, in.MerchantID, bindings["target"]) {
		return uncertain("provider credential changed; account identity must be requalified")
	}
	if e := target.ConfirmCutoverVault(ctx, p.TargetInstrument.RailCustomerRef, p.TargetInstrument.RailMethodRef); e != nil {
		return uncertain("target vault is not qualified: " + e.Error())
	}
	if send && (target.ReadOnly || source.ReadOnly) {
		return Parked("provider writes are disabled")
	}
	if !g.CreateSubmitted {
		// Do not enroll until the exact source and target plan can be read.
		old, found, e := source.GetCutoverSubscription(ctx, p.SourceSubscriptionID)
		if e != nil || !found || !cutoverSourceMatches(old, p) {
			return uncertain("source obligation could not be verified")
		}
		plan, found, e := target.GetCutoverPlan(ctx, p.PlanID)
		if e != nil || !found || !cutoverPlanMatches(plan, p) {
			return uncertain("target plan could not be verified")
		}
		if !send {
			return Retryable("target enrollment has not been submitted")
		}
		claimed, e := store.RecordProgressIfAbsent(ctx, in.ID, "create_submitted", true)
		if e != nil {
			return Parked("persist target submission: " + e.Error())
		}
		if !claimed {
			return uncertain("target enrollment already submitted")
		}
		g.CreateSubmitted = true
		entered, writeErr := h.writeCutover(ctx, current, p, "target", target, func() error {
			var err error
			g.Target, err = target.CreatePausedSubscription(ctx, p.PlanID, p.TargetInstrument.RailCustomerRef, p.TargetInstrument.RailMethodRef, p.Anchor)
			return err
		})
		if !entered {
			// Only this branch owns a newly inserted create marker. No other
			// executor may create while it exists, and our HTTP call never began.
			g.NotExecuted, g.NotExecutedCode = true, "target_enrollment_not_admitted"
			return TerminalWithEvidence("target enrollment never acquired a qualified provider write; no request was sent", evidence())
		}
		e = writeErr
		if e != nil {
			return uncertain("target enrollment has no exact receipt; never recreate: " + e.Error())
		}
		if e = save(); e != nil {
			return uncertain("persist target receipt: " + e.Error())
		}
	}
	if g.Target == nil || g.Target.ID == "" {
		return uncertain("target enrollment has no exact receipt; operator resolution required")
	}
	observed, found, e := target.GetCutoverSubscription(ctx, g.Target.ID)
	if e != nil || !found {
		return uncertain("target subscription readback unavailable")
	}
	pausedTerms := terms
	if !g.PausedAnchor.IsZero() {
		pausedTerms.Anchor = g.PausedAnchor
	}
	paused, known := cutoverPaused(observed.PausedSubscription)
	if (!paused && !cutoverSubscriptionMatches(observed, terms, g.Target.ID)) || (paused && !cutoverSubscriptionMatches(observed, pausedTerms, g.Target.ID)) {
		return uncertain("target subscription differs from frozen terms")
	}
	if !known {
		return uncertain("target pause state is unrecognized")
	}
	if !g.ActivationSubmitted && !paused {
		return uncertain("target became active before verified source cancellation")
	}
	if paused && !terms.Anchor.After(h.now()) {
		return uncertain("billing anchor elapsed; operator resolution required")
	}
	if !g.SourceCanceled {
		old, found, e := source.GetCutoverSubscription(ctx, p.SourceSubscriptionID)
		// A bare 404 has no cancellation receipt. Even after a submitted DELETE,
		// it cannot distinguish cancellation from a stale or wrong-scope lookup.
		if e != nil || old.ID != p.SourceSubscriptionID || old.CustomerVaultID != p.SourceInstrument.RailCustomerRef {
			return uncertain("source cancellation readback unavailable or wrong identity")
		}
		if found {
			// A paused target can survive an outage while the source renews or
			// changes terms. Recheck the complete frozen obligation before DELETE.
			if !cutoverSourceMatches(old, p) {
				return uncertain("source commercial terms changed before cancellation")
			}
			if !send {
				return Retryable("source is still active; cancellation requires executor")
			}
			if !g.SourceCancelSubmitted {
				if _, err := providerqualification.Read(ctx, h.DB, p.Request.ExpectedSourcePSPID); err != nil {
					return Parked("source cutover qualification is unavailable")
				}
				decision, err := h.claimCutoverDecision(ctx, in.ID, nmiCutoverDecision{Action: "complete"})
				if err != nil || decision.Action != "complete" {
					return uncertain("source cancellation lost the cutover direction claim")
				}
				g.Decision = decision
				claimed, e := store.RecordProgressIfAbsent(ctx, in.ID, "source_cancel_submitted", true)
				if e != nil || !claimed {
					return uncertain("source cancellation submission already owned")
				}
				g.SourceCancelSubmitted = true
			}
			entered, writeErr := h.writeCutover(ctx, current, p, "source", source, func() error {
				return source.DeleteRecurringSubscription(ctx, p.SourceSubscriptionID)
			})
			if !entered {
				return Parked("source cutover qualification is unavailable")
			}
			if writeErr != nil {
				return uncertain("source cancellation unresolved: " + writeErr.Error())
			}
			old, found, e = source.GetCutoverSubscription(ctx, p.SourceSubscriptionID)
			if e != nil || found || old.ID != p.SourceSubscriptionID || old.CustomerVaultID != p.SourceInstrument.RailCustomerRef {
				return uncertain("source cancellation is not verified")
			}
		}
		decision, err := h.claimCutoverDecision(ctx, in.ID, nmiCutoverDecision{Action: "complete"})
		if err != nil || decision.Action != "complete" {
			return uncertain("forward completion lost the cutover direction claim")
		}
		g.Decision = decision
		g.SourceCanceled = true
		g.SourceAbsentAt = h.now()
		if old.ID != "" {
			g.SourceReceipt = &old
		}
		if e = save(); e != nil {
			return uncertain("persist source cancellation: " + e.Error())
		}
	}
	if paused {
		if !send {
			return Retryable("source canceled; target activation requires executor")
		}
		if !terms.Anchor.After(h.now()) {
			return uncertain("billing anchor elapsed before target activation")
		}
		if !g.ActivationSubmitted {
			claimed, e := store.RecordProgressIfAbsent(ctx, in.ID, "activation_submitted", true)
			if e != nil || !claimed {
				return uncertain("target activation submission already owned")
			}
			g.ActivationSubmitted = true
		}
		if !g.ActivationAnchor.Equal(terms.Anchor) {
			g.ActivationAnchor = terms.Anchor
			if e = save(); e != nil {
				return uncertain("persist target activation anchor: " + e.Error())
			}
		}
		entered, writeErr := h.writeCutover(ctx, current, p, "target", target, func() error {
			return target.ActivateSubscription(ctx, g.Target.ID, terms.Anchor)
		})
		if !entered {
			return Parked("target cutover qualification is unavailable")
		}
		if writeErr != nil {
			return uncertain("target activation unresolved: " + writeErr.Error())
		}
		observed, found, e = target.GetCutoverSubscription(ctx, g.Target.ID)
		if e != nil || !found || !cutoverSubscriptionMatches(observed, terms, g.Target.ID) {
			return uncertain("target activation readback mismatch")
		}
		paused, known = cutoverPaused(observed.PausedSubscription)
		if !known || paused {
			return uncertain("target activation is unproven")
		}
	}
	g.TargetActive = true
	g.ActivatedTarget = &observed
	if e = save(); e != nil {
		return uncertain("persist target activation: " + e.Error())
	}
	if e = h.repoint(ctx, p, g.Target); e != nil {
		return uncertain("local repoint failed: " + e.Error())
	}
	return Succeeded(evidence())
}

func cutoverPaused(v any) (bool, bool) {
	// NMI emits the flag as a boolean, JSON number, or quoted 0/1. Compare
	// the scalar's JSON spelling; it is never a money or arithmetic value.
	raw, err := json.Marshal(v)
	if err != nil {
		return false, false
	}
	switch string(raw) {
	case "true", "1", `"1"`:
		return true, true
	case "false", "0", `"0"`:
		return false, true
	}
	return false, false
}
func cutoverPlanMatches(plan nmi.V5Plan, p nmiCutoverPayload) bool {
	minor, e := moneyutil.NativeToRailMinorExact(p.Currency, p.Amount)
	if e != nil {
		return false
	}
	amount := fmt.Sprintf("%d.%02d", minor/100, minor%100)
	// This first provider contract covers fixed-day plans only. Calendar plans
	// need their own anchor qualification rather than duration approximations.
	return plan.ID == p.PlanID && plan.PlanAmount == amount && plan.PlanPayments == "0" && plan.DayFrequency == fmt.Sprint(p.CycleHours/24) && (plan.MonthFrequency == "0" || plan.MonthFrequency == "")
}
func cutoverSubscriptionMatches(s nmi.V5Subscription, p nmiCutoverPayload, id string) bool {
	return s.DelayedCondition == "active" && cutoverSubscriptionTermsMatch(s, p, id)
}

func cutoverSubscriptionTermsMatch(s nmi.V5Subscription, p nmiCutoverPayload, id string) bool {
	if s.Object != "subscription" || s.ID != id || s.CustomerVaultID != p.TargetInstrument.RailCustomerRef || s.Plan == nil || !cutoverPlanMatches(*s.Plan, p) || s.Amount != s.Plan.PlanAmount {
		return false
	}
	next, e := time.Parse(time.RFC3339, s.NextBillingDate)
	return e == nil && next.Equal(p.Anchor)
}

func cutoverSourceMatches(s nmi.V5Subscription, p nmiCutoverPayload) bool {
	if s.Plan == nil || s.Plan.ID == "" {
		return false
	}
	// Different accounts may name the same commercial plan differently.
	p.PlanID = s.Plan.ID
	p.TargetInstrument.RailCustomerRef = p.SourceInstrument.RailCustomerRef
	paused, known := cutoverPaused(s.PausedSubscription)
	return known && !paused && cutoverSubscriptionMatches(s, p, p.SourceSubscriptionID)
}
func (h *NMIProviderCutover) isRepointed(ctx context.Context, p nmiCutoverPayload, target *nmi.V5Subscription) (bool, error) {
	if target == nil {
		return false, nil
	}
	return h.DB.Gen(ctx).IsProviderCutoverRepointed(ctx, gen.IsProviderCutoverRepointedParams{
		SubscriptionID: p.SubscriptionID, MerchantID: pMerchant(ctx), TargetPspID: p.Request.ExpectedTargetPSPID,
		TargetSubscriptionID: target.ID, TargetPaymentMethodID: p.Request.TargetPaymentMethodID,
		CustomerID: p.CustomerID, PriceID: p.PriceID, PeriodStart: p.PeriodStart, PeriodEnd: p.PeriodEnd,
	})
}
func (h *NMIProviderCutover) repoint(ctx context.Context, p nmiCutoverPayload, target *nmi.V5Subscription) error {
	wctx, cancel := LedgerWriteContext(ctx)
	defer cancel()
	return h.DB.MerchantTx(wctx, func(ctx context.Context, tx pgx.Tx) error {
		d := h.DB.NewWithPgxTx(tx)
		scopeMerchantID, scopeErr := merchant.Require(ctx)
		if scopeErr != nil {
			return scopeErr
		}
		if _, e := d.Gen(ctx).GetSubscriptionByIDForUpdate(ctx, gen.GetSubscriptionByIDForUpdateParams{MerchantID: scopeMerchantID.UUID(), ID: p.SubscriptionID}); e != nil {
			return e
		}
		live, e := h.freeze(ctx, d, p.SubscriptionID, p.Request)
		if e != nil {
			return e
		}
		if !sameCutoverLocal(live, p) {
			return cutoverConflict("local terms changed")
		}
		updated, e := d.Gen(ctx).RepointProviderCutoverSubscription(ctx, gen.RepointProviderCutoverSubscriptionParams{
			SubscriptionID: p.SubscriptionID, MerchantID: pMerchant(ctx), TargetPspID: p.Request.ExpectedTargetPSPID,
			TargetSubscriptionID: target.ID, TargetPaymentMethodID: p.Request.TargetPaymentMethodID, Now: h.now(),
		})
		if e != nil {
			return e
		}
		if updated != 1 {
			return errors.New("subscription disappeared")
		}
		return nil
	})
}

// Resolve accepts an operator-attested target reference after validating its
// frozen terms and paused state. NMI GET cannot prove correlation to a lost
// create: the operator's reason must identify provider evidence linking this
// ID to the original submission. This is never automatic tuple matching and
// never authorizes another create.
func (h *NMIProviderCutover) Resolve(ctx context.Context, in gen.OpenrailsRailIntent, r Resolution) (Outcome, error) {
	p, g, e := decodeCutover(in)
	if e != nil {
		return Outcome{}, e
	}
	if r.RequalifyAccount != "" {
		return h.requalifyAccount(ctx, in, p, r)
	}
	if r.Abandon {
		return h.requestAbandon(ctx, in, p, g, r)
	}
	if !r.BillingAnchor.IsZero() {
		return h.resolveAnchor(ctx, in, p, g, r)
	}
	if r.Step != "target" || r.NotExecuted || r.ProviderReference == "" || !g.CreateSubmitted || g.Target != nil {
		return Outcome{}, ErrResolutionRejected
	}
	client, ok, e := h.Resolver.ResolveNMIClient(ctx, in.MerchantID, &p.Request.ExpectedTargetPSPID)
	if e != nil || !ok || client == nil {
		return Outcome{}, ErrResolutionRejected
	}
	bindings, _, bindingErr := cutoverAccountBindings(in, p)
	if bindingErr != nil || !h.boundCutoverClient(ctx, client, in.MerchantID, bindings["target"]) {
		return Outcome{}, ErrResolutionRejected
	}
	target, found, e := client.GetCutoverSubscription(ctx, r.ProviderReference)
	paused, known := cutoverPaused(target.PausedSubscription)
	if e != nil || !found || !known || !paused || !cutoverSubscriptionMatches(target, p, r.ProviderReference) {
		return Outcome{}, ErrResolutionRejected
	}
	g.Target = &target
	g.Resolution = r.Record(h.now())
	b, _ := json.Marshal(g)
	var evidence map[string]any
	_ = json.Unmarshal(b, &evidence)
	if e = NewStore(h.DB).RecordProgress(ctx, in.ID, evidence); e != nil {
		return Outcome{}, e
	}
	return Retryable("operator supplied verified paused target; continue under executor gates"), nil
}

func cutoverAnchorResolutionMatches(in gen.OpenrailsRailIntent, r Resolution) bool {
	_, g, err := decodeCutover(in)
	if err != nil || r.BillingAnchor.IsZero() || !g.BillingAnchor.Equal(r.BillingAnchor) || len(g.AnchorResolutions) == 0 {
		return false
	}
	last := g.AnchorResolutions[len(g.AnchorResolutions)-1]
	return last["actor"] == r.Actor && last["reason"] == r.Reason
}

// An expired anchor can only be replaced by explicit operator authorization.
// Resolve reads the exact paused target and canceled source; the ordinary
// executor performs the update under the same mode and destructive gates.
func (h *NMIProviderCutover) resolveAnchor(ctx context.Context, in gen.OpenrailsRailIntent, p nmiCutoverPayload, g nmiCutoverProgress, r Resolution) (Outcome, error) {
	if r.Step != "anchor" || r.ProviderReference != "" || r.NotExecuted || r.Actor == "" || r.Reason == "" || r.BillingAnchor.Nanosecond() != 0 || !r.BillingAnchor.After(h.now()) || g.Target == nil || (!g.SourceCancelSubmitted && !g.SourceCanceled) || g.TargetActive {
		return Outcome{}, ErrResolutionRejected
	}
	previous := p.Anchor
	if !g.BillingAnchor.IsZero() {
		previous = g.BillingAnchor
	}
	if previous.After(h.now()) || !r.BillingAnchor.After(previous) {
		return Outcome{}, ErrResolutionRejected
	}
	live, e := h.freeze(ctx, h.DB, p.SubscriptionID, p.Request)
	if e != nil || !sameCutoverLocal(live, p) {
		return Outcome{}, ErrResolutionRejected
	}
	target, ok, e := h.Resolver.ResolveNMIClient(ctx, in.MerchantID, &p.Request.ExpectedTargetPSPID)
	if e != nil || !ok || target == nil {
		return Outcome{}, ErrResolutionRejected
	}
	bindings, _, bindingErr := cutoverAccountBindings(in, p)
	if bindingErr != nil || !h.boundCutoverClient(ctx, target, in.MerchantID, bindings["target"]) {
		return Outcome{}, ErrResolutionRejected
	}
	if e = target.ConfirmCutoverVault(ctx, p.TargetInstrument.RailCustomerRef, p.TargetInstrument.RailMethodRef); e != nil {
		return Outcome{}, ErrResolutionRejected
	}
	observed, found, e := target.GetCutoverSubscription(ctx, g.Target.ID)
	paused, known := cutoverPaused(observed.PausedSubscription)
	terms := p
	if !g.PausedAnchor.IsZero() {
		terms.Anchor = g.PausedAnchor
	}
	if e != nil || !found || !known || !paused || !cutoverSubscriptionMatches(observed, terms, g.Target.ID) {
		return Outcome{}, ErrResolutionRejected
	}
	source, ok, e := h.Resolver.ResolveNMIClient(ctx, in.MerchantID, &p.Request.ExpectedSourcePSPID)
	if e != nil || !ok || source == nil {
		return Outcome{}, ErrResolutionRejected
	}
	if !h.boundCutoverClient(ctx, source, in.MerchantID, bindings["source"]) {
		return Outcome{}, ErrResolutionRejected
	}
	old, active, e := source.GetCutoverSubscription(ctx, p.SourceSubscriptionID)
	if e != nil || active || old.ID != p.SourceSubscriptionID || old.CustomerVaultID != p.SourceInstrument.RailCustomerRef {
		return Outcome{}, ErrResolutionRejected
	}
	g.SourceCanceled = true
	g.SourceAbsentAt = h.now()
	if old.ID != "" {
		g.SourceReceipt = &old
	}
	g.BillingAnchor = r.BillingAnchor.UTC()
	g.PausedAnchor = terms.Anchor
	record := r.Record(h.now())
	record["previous_anchor"] = previous.Format(time.RFC3339)
	g.AnchorResolutions = append(g.AnchorResolutions, record)
	data, _ := json.Marshal(g)
	var evidence map[string]any
	_ = json.Unmarshal(data, &evidence)
	if e = NewStore(h.DB).RecordProgress(ctx, in.ID, evidence); e != nil {
		return Outcome{}, e
	}
	return Retryable("operator authorized a future first charge; continue under executor gates"), nil
}
