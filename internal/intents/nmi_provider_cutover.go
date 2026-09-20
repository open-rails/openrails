package intents

import (
	"context"
	"crypto/sha256"
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
	Request                     nmiCutoverRequest `json:"request"`
	CustomerID                  uuid.UUID         `json:"customer_id"`
	SubscriptionID              uuid.UUID         `json:"subscription_id"`
	SourceSubscriptionID        string            `json:"source_subscription_id"`
	SourcePaymentMethodID       uuid.UUID         `json:"source_payment_method_id"`
	SourceVaultID               string            `json:"source_vault_id"`
	PriceID                     uuid.UUID         `json:"price_id"`
	PlanID                      string            `json:"plan_id"`
	BillingID                   string            `json:"billing_id"`
	VaultID                     string            `json:"vault_id"`
	Currency                    string            `json:"currency"`
	Amount                      int64             `json:"amount"`
	CycleHours                  int32             `json:"cycle_hours"`
	PeriodStart                 time.Time         `json:"period_start"`
	PeriodEnd                   time.Time         `json:"period_end"`
	Anchor                      time.Time         `json:"anchor"`
	SourceCredentialFingerprint string            `json:"source_credential_fingerprint"`
	TargetCredentialFingerprint string            `json:"target_credential_fingerprint"`
}

type nmiCutoverProgress struct {
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
		return p, cutoverConflict("distinct nonzero source and target PSPs and replacement card required")
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return p, err
	}
	err = d.Qx(ctx).QueryRow(ctx, `SELECT s.customer_id, s.rail_subscription_id, s.payment_method_id, old.rail_customer_ref,
 s.price_id,b.plan_id,pm.rail_customer_ref,pm.rail_method_ref,pr.currency,pr.amount,pr.access_duration_hours,s.current_period_starts_at,s.current_period_ends_at
 FROM openrails.subscriptions s
 JOIN openrails.payment_methods old ON old.id=s.payment_method_id AND old.merchant_id=s.merchant_id
 JOIN openrails.payment_methods pm ON pm.id=$3 AND pm.merchant_id=s.merchant_id AND pm.customer_id=s.customer_id
 JOIN openrails.psps source ON source.id=s.psp_id AND source.merchant_id=s.merchant_id
 JOIN openrails.psps target ON target.id=pm.psp_id AND target.merchant_id=s.merchant_id
 JOIN openrails.prices pr ON pr.id=s.price_id AND pr.merchant_id=s.merchant_id
 JOIN openrails.price_psp_bindings b ON b.price_id=s.price_id AND b.psp_id=target.id AND b.merchant_id=s.merchant_id
 WHERE s.id=$1 AND s.merchant_id=$2 AND s.deleted_at IS NULL
 AND s.psp_id=$4 AND pm.psp_id=$5 AND s.rail='nmi' AND pm.rail='nmi' AND old.psp_id=source.id
 AND source.rail='nmi' AND target.rail='nmi' AND source.environment=target.environment
 AND source.archived AND NOT target.archived AND source.custodian_id IS NULL AND target.custodian_id IS NULL
 AND s.status='active' AND s.scheduled_price_id IS NULL AND s.deletion_scheduled_at IS NULL
 AND pm.custodian='psp' AND COALESCE(pm.park_reason,'')='' AND pm.rebill_driver='provider'
 AND old.custodian='psp' AND old.rebill_driver='provider' AND pr.auto_renew AND NOT pr.archived AND lower(pr.currency)='usd'`,
		id, mid.UUID(), req.TargetPaymentMethodID, req.ExpectedSourcePSPID, req.ExpectedTargetPSPID).Scan(
		&p.CustomerID, &p.SourceSubscriptionID, &p.SourcePaymentMethodID, &p.SourceVaultID, &p.PriceID, &p.PlanID, &p.VaultID, &p.BillingID, &p.Currency, &p.Amount, &p.CycleHours, &p.PeriodStart, &p.PeriodEnd)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, cutoverConflict("subscription, accounts, target card or target price binding is not eligible")
	}
	if err != nil {
		return p, err
	}
	p.PeriodStart = p.PeriodStart.UTC()
	p.PeriodEnd = p.PeriodEnd.UTC()
	p.Currency = strings.ToLower(p.Currency)
	p.Anchor = p.PeriodEnd.UTC().Truncate(time.Second)
	if p.Anchor.Before(p.PeriodEnd) {
		p.Anchor = p.Anchor.Add(time.Second)
	}
	if p.SourceSubscriptionID == "" || p.SourceVaultID == "" || p.PlanID == "" || p.VaultID == "" || p.BillingID == "" || p.Amount <= 0 || p.CycleHours <= 0 || p.CycleHours%24 != 0 || !p.Anchor.After(p.PeriodStart) {
		return p, cutoverConflict("incomplete or unsupported commercial terms")
	}
	source, ok, err := h.Resolver.ResolveNMIClient(ctx, mid.UUID(), &req.ExpectedSourcePSPID)
	if err != nil || !ok || source == nil || source.SecurityKey == "" {
		return p, cutoverConflict("source credential unavailable")
	}
	target, ok, err := h.Resolver.ResolveNMIClient(ctx, mid.UUID(), &req.ExpectedTargetPSPID)
	if err != nil || !ok || target == nil || target.SecurityKey == "" {
		return p, cutoverConflict("target credential unavailable")
	}
	p.SourceCredentialFingerprint = cutoverCredentialFingerprint(source)
	p.TargetCredentialFingerprint = cutoverCredentialFingerprint(target)
	return p, nil
}

func cutoverCredentialFingerprint(client *nmi.NMIClient) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(client.SecurityKey)))
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
		if _, e := d.Qx(ctx).Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, k); e != nil {
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
		if _, e = d.Gen(ctx).GetSubscriptionByIDForUpdate(ctx, id); e != nil {
			return e
		}
		if _, e = d.Qx(ctx).Exec(ctx, `SELECT id FROM openrails.payment_methods WHERE merchant_id=$1 AND (id=$2 OR id=(SELECT payment_method_id FROM openrails.subscriptions WHERE id=$3 AND merchant_id=$1)) ORDER BY id FOR UPDATE`, pMerchant(ctx), req.TargetPaymentMethodID, id); e != nil {
			return e
		}
		p, e := h.freeze(ctx, d, id, req)
		if e != nil {
			return e
		}
		if !p.Anchor.After(h.now()) {
			return cutoverConflict("paid-through anchor must be in the future")
		}
		var busy bool
		e = d.Qx(ctx).QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM openrails.rail_intents WHERE merchant_id=$1 AND subscription_id=$2 AND status NOT IN ('succeeded','failed_terminal','superseded','expired'))`, pMerchant(ctx), id).Scan(&busy)
		if e != nil {
			return e
		}
		if busy {
			return cutoverConflict("subscription has an unresolved provider operation")
		}
		e = d.Qx(ctx).QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM openrails.rail_intents WHERE merchant_id=$1 AND intent_type IN ('nmi_vault_delete','nmi_payment_method_update') AND payload->>'payment_method_id'=ANY($2::text[]) AND status NOT IN ('succeeded','failed_terminal','superseded','expired'))`, pMerchant(ctx), []string{p.SourcePaymentMethodID.String(), req.TargetPaymentMethodID.String()}).Scan(&busy)
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
	if !reflect.DeepEqual(live, p) {
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
	if cutoverCredentialFingerprint(source) != p.SourceCredentialFingerprint || cutoverCredentialFingerprint(target) != p.TargetCredentialFingerprint {
		return uncertain("provider credential changed; account identity must be requalified")
	}
	if e := target.ConfirmCutoverVault(ctx, p.VaultID, p.BillingID); e != nil {
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
		g.Target, e = target.CreatePausedSubscription(ctx, p.PlanID, p.VaultID, p.BillingID, p.Anchor)
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
		if e != nil || old.ID != p.SourceSubscriptionID || old.CustomerVaultID != p.SourceVaultID {
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
				claimed, e := store.RecordProgressIfAbsent(ctx, in.ID, "source_cancel_submitted", true)
				if e != nil || !claimed {
					return uncertain("source cancellation submission already owned")
				}
				g.SourceCancelSubmitted = true
			}
			if e = source.DeleteRecurringSubscription(ctx, p.SourceSubscriptionID); e != nil {
				return uncertain("source cancellation unresolved: " + e.Error())
			}
			old, found, e = source.GetCutoverSubscription(ctx, p.SourceSubscriptionID)
			if e != nil || found || old.ID != p.SourceSubscriptionID || old.CustomerVaultID != p.SourceVaultID {
				return uncertain("source cancellation is not verified")
			}
		}
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
		if e = target.ActivateSubscription(ctx, g.Target.ID, terms.Anchor); e != nil {
			return uncertain("target activation unresolved: " + e.Error())
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
	if s.Object != "subscription" || s.ID != id || s.CustomerVaultID != p.VaultID || s.Plan == nil || !cutoverPlanMatches(*s.Plan, p) || s.Amount != s.Plan.PlanAmount || s.DelayedCondition != "active" {
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
	p.VaultID = p.SourceVaultID
	paused, known := cutoverPaused(s.PausedSubscription)
	return known && !paused && cutoverSubscriptionMatches(s, p, p.SourceSubscriptionID)
}
func (h *NMIProviderCutover) isRepointed(ctx context.Context, p nmiCutoverPayload, target *nmi.V5Subscription) (bool, error) {
	if target == nil {
		return false, nil
	}
	var done bool
	e := h.DB.Qx(ctx).QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM openrails.subscriptions WHERE id=$1 AND merchant_id=$2 AND psp_id=$3 AND rail_subscription_id=$4 AND payment_method_id=$5 AND customer_id=$6 AND price_id=$7 AND current_period_starts_at=$8 AND current_period_ends_at=$9 AND deleted_at IS NULL)`, p.SubscriptionID, pMerchant(ctx), p.Request.ExpectedTargetPSPID, target.ID, p.Request.TargetPaymentMethodID, p.CustomerID, p.PriceID, p.PeriodStart, p.PeriodEnd).Scan(&done)
	return done, e
}
func (h *NMIProviderCutover) repoint(ctx context.Context, p nmiCutoverPayload, target *nmi.V5Subscription) error {
	wctx, cancel := LedgerWriteContext(ctx)
	defer cancel()
	return h.DB.MerchantTx(wctx, func(ctx context.Context, tx pgx.Tx) error {
		d := h.DB.NewWithPgxTx(tx)
		if _, e := d.Gen(ctx).GetSubscriptionByIDForUpdate(ctx, p.SubscriptionID); e != nil {
			return e
		}
		live, e := h.freeze(ctx, d, p.SubscriptionID, p.Request)
		if e != nil {
			return e
		}
		if !reflect.DeepEqual(live, p) {
			return cutoverConflict("local terms changed")
		}
		tag, e := d.Qx(ctx).Exec(ctx, `UPDATE openrails.subscriptions SET psp_id=$3,rail_subscription_id=$4,payment_method_id=$5,updated_at=$6 WHERE id=$1 AND merchant_id=$2`, p.SubscriptionID, pMerchant(ctx), p.Request.ExpectedTargetPSPID, target.ID, p.Request.TargetPaymentMethodID, h.now())
		if e != nil {
			return e
		}
		if tag.RowsAffected() != 1 {
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
	if cutoverCredentialFingerprint(client) != p.TargetCredentialFingerprint {
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
	if e != nil || !reflect.DeepEqual(live, p) {
		return Outcome{}, ErrResolutionRejected
	}
	target, ok, e := h.Resolver.ResolveNMIClient(ctx, in.MerchantID, &p.Request.ExpectedTargetPSPID)
	if e != nil || !ok || target == nil {
		return Outcome{}, ErrResolutionRejected
	}
	if cutoverCredentialFingerprint(target) != p.TargetCredentialFingerprint {
		return Outcome{}, ErrResolutionRejected
	}
	if e = target.ConfirmCutoverVault(ctx, p.VaultID, p.BillingID); e != nil {
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
	if cutoverCredentialFingerprint(source) != p.SourceCredentialFingerprint {
		return Outcome{}, ErrResolutionRejected
	}
	old, active, e := source.GetCutoverSubscription(ctx, p.SourceSubscriptionID)
	if e != nil || active || old.ID != p.SourceSubscriptionID || old.CustomerVaultID != p.SourceVaultID {
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
