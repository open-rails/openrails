package intents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/pkg/merchant"
)

// TypeNMIPaymentSourceUpdate is the durable NMI payment-source swap (#674):
// repointing a recurring subscription's billing at a different customer vault.
// A transport-ambiguous update is the nastiest split in the payment-method
// flow — local says new card, NMI keeps rebilling the old one (or vice versa)
// until dunning surfaces it weeks later — so the swap is write-through: durable
// intent → inline execute → confirm; ambiguity ⇒ pending_verify and the
// verifier converges local and remote off the recurring record's CURRENT vault.
const TypeNMIPaymentSourceUpdate = "nmi_payment_source_update"

// NMIPaymentSourceUpdateIdempotencyKey is the logical identity of "the swap of
// this subscription onto this vault". priorSwaps is the durable count of
// SUCCEEDED swap intents for the subscription (the #672/#673 attempt-count-key
// pattern): a retry while the current swap is unresolved recomputes the same
// count ⇒ same key ⇒ maps onto the same intent, while a NEW wish after any
// completed swap advances the count — so a later A→B→A cycle can never be
// falsely answered from an old succeeded tombstone.
func NMIPaymentSourceUpdateIdempotencyKey(subscriptionID uuid.UUID, newVaultID string, priorSwaps int64) string {
	return fmt.Sprintf("%s:%s:%s:swap%d", TypeNMIPaymentSourceUpdate, subscriptionID, newVaultID, priorSwaps)
}

// NMIPaymentSourceUpdatePayload is the stored payload. The subscription and
// target payment method are re-read at execution time; the vault-id copies are
// forensics plus the verifier's old/new comparison anchors.
type NMIPaymentSourceUpdatePayload struct {
	UserID             string     `json:"user_id"`
	RailSubscriptionID string     `json:"rail_subscription_id,omitempty"`
	NewPaymentMethodID uuid.UUID  `json:"new_payment_method_id"`
	NewVaultID         string     `json:"new_vault_id"`
	OldPaymentMethodID *uuid.UUID `json:"old_payment_method_id,omitempty"`
	OldVaultID         string     `json:"old_vault_id,omitempty"`
}

// NMIPaymentSourceUpdateHandler implements the effectively-once swap:
//
//   - relevance: applicable while the subscription still rebills (active/
//     past_due) and still points at the intent's old (or already new) payment
//     method; a later swap that moved the row elsewhere supersedes it.
//   - execute: read the recurring record first — already billing the new vault
//     IS success (crash-after-write recovery costs one read, zero writes);
//     otherwise send the update. Transport-ambiguous outcomes go to the
//     verifier; parsed clean rejections are TERMINAL (immediate user-facing
//     failure — a definite refusal never re-pushes in the background). The update is an absolute set
//     (customer_vault_id=<new>), so a re-send after a lost-response attempt is
//     harmless by construction.
//   - verify: read the record's CURRENT vault — new ⇒ done (finalize local),
//     old ⇒ verified not executed (executor re-sends), record gone or a vault
//     matching neither ⇒ terminal with a repair note (never stomp out-of-band
//     provider state from the verifier).
//   - finalize: point the local row at the new payment method — local commits
//     only AFTER the provider is confirmed, and the intent row is the durable
//     source of truth for repairing every crash/timeout ordering in between.
type NMIPaymentSourceUpdateHandler struct {
	DB      *db.DB
	Clients map[string]*nmi.NMIClient
	Clock   clockwork.Clock
	Policy  BackoffPolicy
}

func NewNMIPaymentSourceUpdateHandler(d *db.DB, clients map[string]*nmi.NMIClient, clock clockwork.Clock) *NMIPaymentSourceUpdateHandler {
	return &NMIPaymentSourceUpdateHandler{DB: d, Clients: clients, Clock: clock, Policy: DefaultBackoff}
}

func (h *NMIPaymentSourceUpdateHandler) Type() string { return TypeNMIPaymentSourceUpdate }
func (h *NMIPaymentSourceUpdateHandler) Backoff(attempts int32) time.Duration {
	return h.Policy.Delay(attempts)
}

func (h *NMIPaymentSourceUpdateHandler) now() time.Time {
	if h.Clock != nil {
		return h.Clock.Now().UTC()
	}
	return time.Now().UTC()
}

func decodeNMIPaymentSourceUpdatePayload(intent gen.OpenrailsRailIntent) (NMIPaymentSourceUpdatePayload, error) {
	var p NMIPaymentSourceUpdatePayload
	if len(intent.Payload) == 0 {
		return p, errors.New("nmi payment source update intent has no payload")
	}
	if err := json.Unmarshal(intent.Payload, &p); err != nil {
		return p, fmt.Errorf("decode nmi payment source update payload: %w", err)
	}
	if p.NewPaymentMethodID == uuid.Nil || strings.TrimSpace(p.NewVaultID) == "" {
		return p, errors.New("nmi payment source update payload is incomplete")
	}
	return p, nil
}

// CheckRelevance: the swap applies while the subscription still rebills and
// still points at the intent's old (or already new) payment method. A row
// moved to a THIRD method means a newer swap won — superseded, never re-fought.
func (h *NMIPaymentSourceUpdateHandler) CheckRelevance(ctx context.Context, intent gen.OpenrailsRailIntent) (Relevance, error) {
	p, err := decodeNMIPaymentSourceUpdatePayload(intent)
	if err != nil {
		return StillRelevant(), nil // Execute reports the terminal payload error
	}
	sub, err := h.loadSubscription(ctx, intent)
	if err != nil {
		if db.IsNotFound(err) {
			return SupersededBy("subscription row no longer exists"), nil
		}
		return Relevance{}, err
	}
	if sub.Status != models.StatusActive && sub.Status != models.StatusPastDue {
		return SupersededBy(fmt.Sprintf("subscription no longer rebilling (status=%s); payment-source update moot", sub.Status)), nil
	}
	cur := sub.PaymentMethodID
	switch {
	case cur == nil:
		return StillRelevant(), nil
	case *cur == p.NewPaymentMethodID:
		return StillRelevant(), nil // finalize done or pending; Execute/Verify confirm the provider side
	case p.OldPaymentMethodID != nil && *cur == *p.OldPaymentMethodID:
		return StillRelevant(), nil
	default:
		return SupersededBy(fmt.Sprintf("subscription now bills payment method %s (neither this swap's old nor new); a newer update won", cur)), nil
	}
}

func (h *NMIPaymentSourceUpdateHandler) Execute(ctx context.Context, intent gen.OpenrailsRailIntent) Outcome {
	p, err := decodeNMIPaymentSourceUpdatePayload(intent)
	if err != nil {
		return Terminal(err.Error())
	}
	sub, err := h.loadSubscription(ctx, intent)
	if err != nil {
		return Retryable("load subscription: " + err.Error())
	}
	psid := strings.TrimSpace(sub.RailSubscriptionID)
	if psid == "" {
		return Terminal("subscription has no rail subscription id; nothing exists at the provider to repoint")
	}
	client, outcome, ok := h.resolveClient(ctx, intent, sub)
	if !ok {
		return outcome
	}
	if client.ReadOnly {
		return Parked("nmi client is read-only (mode=readonly)")
	}
	newVault, outcome, ok := h.targetVault(ctx, p)
	if !ok {
		return outcome
	}

	// Read-first: already billing the new vault IS success (a prior attempt's
	// write landed, or an interrupted finalize) — zero provider writes.
	remote, found, err := client.GetSubscription(psid)
	if err != nil {
		return Retryable("provider read before update failed: " + err.Error())
	}
	if !found {
		return Terminal(fmt.Sprintf("recurring record %s gone at provider (cancelled/tombstoned); payment-source update cannot apply — repair: subscription lifecycle owns this, no local change made", psid))
	}
	if strings.TrimSpace(remote.CustomerVaultID) == newVault {
		if err := h.finalize(ctx, intent, p); err != nil {
			return Ambiguous("provider already bills the new vault, but local finalize failed: " + err.Error())
		}
		return Succeeded(map[string]any{"verified_already_pointing": true, "rail_subscription_id": psid, "new_vault_id": newVault})
	}

	if err := client.UpdateSubscriptionPaymentSource(psid, newVault); err != nil {
		switch {
		case errors.Is(err, nmi.ErrProviderReadOnly):
			return Parked("nmi provider writes blocked (mode=readonly)")
		case nmi.IsTransportAmbiguous(err):
			// The update MAY have landed; the verifier resolves via reads.
			return Ambiguous("payment-source update outcome unknown: " + err.Error())
		default:
			// Parsed clean rejection: NMI understood the request and refused
			// (bad vault id, dead subscription). It will not fix itself — the
			// user gets an immediate terminal error, never a background
			// re-push (Paul 2026-07-02: definite failures fail NOW; only
			// ambiguity earns system-driven repair).
			return Terminal("payment-source update rejected cleanly: " + err.Error())
		}
	}

	if err := h.finalize(ctx, intent, p); err != nil {
		// The provider update DID happen; the verifier retries finalize (its
		// read will see the new vault).
		return Ambiguous("updated at provider, but local finalize failed: " + err.Error())
	}
	return Succeeded(map[string]any{
		"updated":              true,
		"rail_subscription_id": psid,
		"new_vault_id":         newVault,
		"old_vault_id":         p.OldVaultID,
	})
}

// Verify resolves an ambiguous swap via the recurring record's CURRENT vault:
// new ⇒ the update landed (finalize local, done); old ⇒ it verifiably did not
// (the executor re-sends); record gone or a vault matching neither ⇒ terminal
// with a repair note — the verifier never writes provider state.
func (h *NMIPaymentSourceUpdateHandler) Verify(ctx context.Context, intent gen.OpenrailsRailIntent) Outcome {
	p, err := decodeNMIPaymentSourceUpdatePayload(intent)
	if err != nil {
		return Terminal(err.Error())
	}
	sub, err := h.loadSubscription(ctx, intent)
	if err != nil {
		return Ambiguous("load subscription: " + err.Error())
	}
	psid := strings.TrimSpace(sub.RailSubscriptionID)
	if psid == "" {
		return Terminal("subscription has no rail subscription id; nothing exists at the provider to verify")
	}
	client, _, ok := h.resolveClient(ctx, intent, sub)
	if !ok {
		return Ambiguous(fmt.Sprintf("nmi client not configured for provider %q; cannot verify", intent.Provider))
	}
	newVault, outcome, ok := h.targetVault(ctx, p)
	if !ok {
		return outcome
	}
	remote, found, err := client.GetSubscription(psid)
	if err != nil {
		return Ambiguous("provider read failed: " + err.Error())
	}
	if !found {
		return Terminal(fmt.Sprintf("recurring record %s gone at provider (cancelled/tombstoned) while a payment-source update was unresolved — repair: subscription lifecycle owns this, local payment-method link left untouched", psid))
	}
	cur := strings.TrimSpace(remote.CustomerVaultID)
	switch {
	case cur == newVault:
		if err := h.finalize(ctx, intent, p); err != nil {
			return Ambiguous("verified new vault at provider, but local finalize failed: " + err.Error())
		}
		return Succeeded(map[string]any{"verified_new_vault": true, "rail_subscription_id": psid, "new_vault_id": newVault})
	case strings.TrimSpace(p.OldVaultID) != "" && cur == strings.TrimSpace(p.OldVaultID):
		return Retryable("provider still bills the old vault; update verified not executed")
	case strings.TrimSpace(p.OldVaultID) == "":
		// Old side unknown locally (legacy row without a payment-method link):
		// any non-new vault means the update did not land — re-execute.
		return Retryable(fmt.Sprintf("provider bills vault %s, not the requested one; update verified not executed", cur))
	default:
		return TerminalWithEvidence(
			fmt.Sprintf("provider bills vault %s — neither this swap's old (%s) nor new (%s); out-of-band change, not overwriting — repair: re-issue the swap if still wanted", cur, p.OldVaultID, newVault),
			map[string]any{"provider_vault_id": cur, "old_vault_id": p.OldVaultID, "new_vault_id": newVault, "rail_subscription_id": psid},
		)
	}
}

func (h *NMIPaymentSourceUpdateHandler) loadSubscription(ctx context.Context, intent gen.OpenrailsRailIntent) (*models.Subscription, error) {
	if intent.SubscriptionID == nil {
		return nil, errors.New("intent has no subscription_id")
	}
	return subscriptions.NewSubscriptionRepo(h.DB).GetByID(ctx, *intent.SubscriptionID)
}

// resolveClient resolves the account-aware NMI client for the subscription
// (rows pinned to a provider account resolve by account key, #641/#655).
// ok=false carries the Parked outcome to return.
func (h *NMIPaymentSourceUpdateHandler) resolveClient(ctx context.Context, intent gen.OpenrailsRailIntent, sub *models.Subscription) (*nmi.NMIClient, Outcome, bool) {
	client, key, ok, err := subscriptions.NMIClientForExistingSubscription(ctx, h.DB, h.Clients, sub)
	if err != nil {
		return nil, Parked(fmt.Sprintf("resolve nmi client for provider %q: %v", intent.Provider, err)), false
	}
	if !ok {
		return nil, Parked(fmt.Sprintf("nmi client not configured for provider account %q", key)), false
	}
	return client, Outcome{}, true
}

// targetVault re-reads the target payment method for a fresh vault ref; the
// payload copy backstops a row deleted out-of-band AFTER the provider write
// may already have landed (the swap must still converge). ok=false carries the
// outcome to return.
func (h *NMIPaymentSourceUpdateHandler) targetVault(ctx context.Context, p NMIPaymentSourceUpdatePayload) (string, Outcome, bool) {
	pm, err := paymentmethods.NewPaymentMethodRepo(h.DB).GetByID(ctx, p.NewPaymentMethodID)
	if err != nil {
		if errors.Is(err, paymentmethods.ErrPaymentMethodNotFound) {
			return strings.TrimSpace(p.NewVaultID), Outcome{}, true
		}
		return "", Retryable("load target payment method: " + err.Error()), false
	}
	if v := strings.TrimSpace(pm.RailCustomerRef); v != "" {
		return v, Outcome{}, true
	}
	return "", Terminal("target payment method has no rail customer ref; cannot repoint billing"), false
}

// finalize points the local subscription at the new payment method — only
// ever called AFTER the provider side is confirmed. Idempotent; a subscription
// row gone out-of-band leaves nothing to finalize.
func (h *NMIPaymentSourceUpdateHandler) finalize(ctx context.Context, intent gen.OpenrailsRailIntent, p NMIPaymentSourceUpdatePayload) error {
	repo := subscriptions.NewSubscriptionRepo(h.DB)
	sub, err := repo.GetByID(ctx, *intent.SubscriptionID)
	if err != nil {
		if db.IsNotFound(err) {
			return nil
		}
		return err
	}
	if sub.PaymentMethodID != nil && *sub.PaymentMethodID == p.NewPaymentMethodID {
		return nil
	}
	newID := p.NewPaymentMethodID
	sub.PaymentMethodID = &newID
	return repo.UpdateAt(ctx, sub, h.now())
}

// ErrPaymentSourceUpdateProcessing: the durable swap intent could not confirm
// the provider update inline (transport-ambiguous outcome, parked provider).
// The intent ledger converges local and remote out-of-band; a retried request
// maps onto the SAME intent (and an inline retry resolves it via the read-first
// execute). Never a silent split, never a lost swap.
var ErrPaymentSourceUpdateProcessing = errors.New("payment method update is processing; retry the same request to check the result")

// PaymentSourceUpdateOutcome mirrors the durable intent's post-execution state
// for the two swap call sites. Neither Done nor Terminal = still resolving
// out-of-band (surface ErrPaymentSourceUpdateProcessing, not success/decline).
type PaymentSourceUpdateOutcome struct {
	// Done: the provider bills the new vault and the local row points at it.
	Done bool
	// Terminal: the swap failed permanently; Reason says why.
	Terminal bool
	Reason   string
}

// PaymentSourceUpdateThrough posts the durable nmi_payment_source_update
// intent and executes it inline (#674 write-through). Wired by the composition
// root; both the HTTP handler and the embedded pkg/service twin route through
// it.
type PaymentSourceUpdateThrough struct {
	Runner *Runner
	DB     *db.DB
}

func (t *PaymentSourceUpdateThrough) ExecutePaymentSourceUpdate(ctx context.Context, sub *models.Subscription, newPM *models.PaymentMethod, origin Origin, originReason string) (PaymentSourceUpdateOutcome, error) {
	if t == nil || t.Runner == nil || t.DB == nil {
		return PaymentSourceUpdateOutcome{}, errors.New("payment-source update intent runner not wired")
	}
	if sub == nil || newPM == nil {
		return PaymentSourceUpdateOutcome{}, errors.New("subscription and payment method are required")
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return PaymentSourceUpdateOutcome{}, err
	}
	newVault := strings.TrimSpace(newPM.RailCustomerRef)
	if newVault == "" {
		return PaymentSourceUpdateOutcome{}, errors.New("target payment method has no rail customer ref")
	}

	// Old side: forensics + the verifier's old-vault comparison anchor. A
	// missing/unlinked old method degrades to "old unknown" (verify re-executes
	// on any non-new vault).
	var oldPMID *uuid.UUID
	var oldVault string
	if sub.PaymentMethodID != nil {
		id := *sub.PaymentMethodID
		oldPMID = &id
		old, err := paymentmethods.NewPaymentMethodRepo(t.DB).GetByID(ctx, id)
		switch {
		case err == nil:
			oldVault = strings.TrimSpace(old.RailCustomerRef)
		case errors.Is(err, paymentmethods.ErrPaymentMethodNotFound):
			// linked row gone; old vault stays unknown
		default:
			return PaymentSourceUpdateOutcome{}, fmt.Errorf("load current payment method: %w", err)
		}
	}

	subID := sub.ID
	priorSwaps, err := t.DB.Gen(ctx).CountRailIntents(ctx, gen.CountRailIntentsParams{
		Status:         ptr(StatusSucceeded),
		IntentType:     ptr(TypeNMIPaymentSourceUpdate),
		SubscriptionID: &subID,
	})
	if err != nil {
		return PaymentSourceUpdateOutcome{}, fmt.Errorf("count prior payment-source updates: %w", err)
	}

	row, err := t.Runner.EnqueueAndExecute(ctx, EnqueueParams{
		MerchantID:            tid.UUID(),
		Provider:              strings.ToLower(string(sub.Rail)),
		IntentType:            TypeNMIPaymentSourceUpdate,
		SubscriptionID:        &subID,
		RailMerchantAccountID: sub.RailMerchantAccountID,
		Payload: NMIPaymentSourceUpdatePayload{
			UserID:             sub.CustomerID.String(),
			RailSubscriptionID: sub.RailSubscriptionID,
			NewPaymentMethodID: newPM.ID,
			NewVaultID:         newVault,
			OldPaymentMethodID: oldPMID,
			OldVaultID:         oldVault,
		},
		IdempotencyKey: NMIPaymentSourceUpdateIdempotencyKey(sub.ID, newVault, priorSwaps),
		NextAttemptAt:  time.Now().UTC(),
		Origin:         origin,
		OriginReason:   originReason,
	})
	if err != nil {
		return PaymentSourceUpdateOutcome{}, err
	}
	out := PaymentSourceUpdateOutcome{}
	if row.LastFailureReason != nil {
		out.Reason = *row.LastFailureReason
	}
	switch row.Status {
	case StatusSucceeded:
		out.Done = true
	case StatusFailedTerminal, StatusSuperseded, StatusExpired:
		out.Terminal = true
	}
	return out, nil
}

func ptr[T any](v T) *T { return &v }
