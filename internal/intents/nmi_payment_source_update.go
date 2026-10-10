package intents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/idguard"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/mandates"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// TypeNMIPaymentSourceUpdate is the durable NMI payment-source swap:
// repointing a recurring subscription's billing at a different customer vault.
// An ambiguous update would split local and NMI (local says new card, NMI
// rebills the old one), so the swap is write-through: durable intent, inline
// execute, confirm. Ambiguity goes to the verifier, which converges off the
// recurring record's current vault.
const TypeNMIPaymentSourceUpdate = "nmi_payment_source_update"

// NMIPaymentSourceUpdateIdempotencyKey is the logical identity of "the swap of
// this subscription onto this vault". priorSwaps is the durable count of
// SUCCEEDED swap intents for the subscription: a retry while the current swap
// is unresolved recomputes the same key, while a new wish after any completed
// swap advances it, so an A→B→A cycle is never answered from an old tombstone.
func NMIPaymentSourceUpdateIdempotencyKey(subscriptionID uuid.UUID, newRailCustomerRef string, priorSwaps int64) string {
	return fmt.Sprintf("%s:%s:%s:swap%d", TypeNMIPaymentSourceUpdate, subscriptionID, newRailCustomerRef, priorSwaps)
}

// NMIPaymentSourceUpdatePayload is the stored payload. The subscription and
// target payment method are re-read at execution time; the vault-id copies are
// forensics plus the verifier's old/new comparison anchors. NewPspID freezes
// the provider account that vaulted the target when the swap was produced, so
// a custody remap after enqueue is detectable where provider traffic is sent.
type NMIPaymentSourceUpdatePayload struct {
	UserID             string     `json:"user_id"`
	RailSubscriptionID string     `json:"rail_subscription_id,omitempty"`
	NewPaymentMethodID uuid.UUID  `json:"new_payment_method_id"`
	NewRailCustomerRef string     `json:"new_vault_id"`
	NewPspID           uuid.UUID  `json:"new_psp_id"`
	OldPaymentMethodID *uuid.UUID `json:"old_payment_method_id,omitempty"`
	OldRailCustomerRef string     `json:"old_vault_id,omitempty"`
	// Follow: once moved, the subscription follows its customer's default
	// (the target) instead of having the target as its own card.
	Follow bool `json:"follow,omitempty"`
	// VerifiedTransactionID is a recurring verification on the target, the
	// storing transaction the moved agreement cites when the card has no
	// recurring lineage of its own.
	VerifiedTransactionID string `json:"verified_transaction_id,omitempty"`
}

// EvidenceCodePSPMismatch is result_evidence["code"] of a swap the executor
// refused because the subscription, the intent and the target method no longer
// name one provider account. Terminal, never retried, zero provider writes.
const EvidenceCodePSPMismatch = "psp_mismatch"

// NMIPaymentSourceUpdateHandler implements the effectively-once swap:
//
//   - relevance: applicable while the subscription still rebills (active/
//     past_due) and still points at the intent's old (or already new) payment
//     method; a later swap that moved the row elsewhere supersedes it.
//   - provider account: before any provider traffic, the subscription, the
//     intent's PSP, the frozen target PSP and the target method's current PSP
//     must be one account, re-read under the target's shared row lock
//     (serialized against custody remap). A mismatch is terminal with evidence
//     code psp_mismatch: never sent, never retried.
//   - execute: read the recurring record first; already billing the new vault
//     is success. Otherwise send the update, an absolute set
//     (customer_vault_id=<new>) that is safe to re-send. Ambiguous outcomes go
//     to the verifier; clean rejections are terminal (no background re-push).
//   - verify: the record's current vault decides: new = done (finalize local),
//     old = not executed (executor re-sends), gone or neither = terminal with
//     a repair note (never stomp out-of-band provider state).
//   - finalize: local commits only after the provider is confirmed; the intent
//     row is the durable source of truth for repairing any crash in between.
type NMIPaymentSourceUpdateHandler struct {
	DB *db.DB
	// Resolver arms the subscription merchant's NMI client from the armed
	// rail state at drain time.
	Resolver NMIClientResolver
	Clock    clockwork.Clock
	Policy   BackoffPolicy
}

func NewNMIPaymentSourceUpdateHandler(d *db.DB, resolver NMIClientResolver, clock clockwork.Clock) *NMIPaymentSourceUpdateHandler {
	return &NMIPaymentSourceUpdateHandler{DB: d, Resolver: resolver, Clock: clock, Policy: DefaultBackoff}
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

func decodeNMIPaymentSourceUpdatePayload(intent gen.BillingProviderIntent) (NMIPaymentSourceUpdatePayload, error) {
	var p NMIPaymentSourceUpdatePayload
	if len(intent.Payload) == 0 {
		return p, errors.New("nmi payment source update intent has no payload")
	}
	if err := json.Unmarshal(intent.Payload, &p); err != nil {
		return p, fmt.Errorf("decode nmi payment source update payload: %w", err)
	}
	// A zero id in the frozen payload fails closed exactly like a missing one:
	// it can never be compared to a live PSP, so it must not reach the seam.
	if p.NewPaymentMethodID == uuid.Nil || strings.TrimSpace(p.NewRailCustomerRef) == "" || p.NewPspID == uuid.Nil {
		return p, errors.New("nmi payment source update payload is incomplete")
	}
	if p.OldPaymentMethodID != nil && *p.OldPaymentMethodID == uuid.Nil {
		return p, errors.New("nmi payment source update payload carries a zero old payment method id")
	}
	return p, nil
}

// CheckRelevance: the swap applies while the subscription still rebills and
// still points at the intent's old (or already new) payment method; a swap
// that follows the default also needs the target still to be the default. A
// row moved to a THIRD method means a newer swap won — superseded, never
// re-fought.
func (h *NMIPaymentSourceUpdateHandler) CheckRelevance(ctx context.Context, intent gen.BillingProviderIntent) (Relevance, error) {
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
	if sub.Status != models.StatusActive && sub.Status != models.StatusPastDue && sub.Status != models.StatusAwaitingMethod {
		return SupersededBy(fmt.Sprintf("subscription no longer rebilling (status=%s); payment-source update moot", sub.Status)), nil
	}
	cur := sub.PaymentMethodID
	if p.Follow {
		if cur != nil && (p.OldPaymentMethodID == nil || *cur != *p.OldPaymentMethodID) {
			return SupersededBy(fmt.Sprintf("subscription now has its own payment method %s; a newer update won", cur)), nil
		}
		following := *sub
		following.PaymentMethodID = nil
		def, err := subscriptions.PaymentMethodOf(ctx, h.DB.Gen(ctx), &following)
		if err != nil {
			return Relevance{}, err
		}
		if def == nil || *def != p.NewPaymentMethodID {
			return SupersededBy("the customer's default card changed; a newer update won"), nil
		}
		return StillRelevant(), nil
	}
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

func (h *NMIPaymentSourceUpdateHandler) Execute(ctx context.Context, intent gen.BillingProviderIntent) Outcome {
	p, err := decodeNMIPaymentSourceUpdatePayload(intent)
	if err != nil {
		return Terminal(err.Error())
	}
	pin, refused, err := h.pinProviderAccount(ctx, intent, p)
	if err != nil {
		return Retryable("pin provider account: " + err.Error())
	}
	if refused != nil {
		return *refused
	}
	sub, newRailCustomerRef := pin.sub, pin.newRailCustomerRef
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

	// Read-first: already billing the new vault IS success (a prior attempt's
	// write landed, or an interrupted finalize) — zero provider writes.
	remote, found, err := client.GetSubscription(ctx, psid)
	if err != nil {
		return Retryable("provider read before update failed: " + err.Error())
	}
	if !found {
		return Terminal(fmt.Sprintf("recurring record %s gone at provider (canceled/tombstoned); payment-source update cannot apply — repair: subscription lifecycle owns this, no local change made", psid))
	}
	if strings.TrimSpace(remote.CustomerVaultID) == newRailCustomerRef {
		if err := h.finalize(ctx, intent, p); err != nil {
			return Ambiguous("provider already bills the new vault, but local finalize failed: " + err.Error())
		}
		return Succeeded(map[string]any{"verified_already_pointing": true, "rail_subscription_id": psid, "new_vault_id": newRailCustomerRef})
	}

	if err := client.UpdateSubscriptionPaymentSource(ctx, psid, newRailCustomerRef); err != nil {
		switch {
		case errors.Is(err, nmi.ErrProviderReadOnly):
			return Parked("nmi provider writes blocked (mode=readonly)")
		case nmi.IsTransportAmbiguous(err):
			// The update MAY have landed; the verifier resolves via reads.
			return Ambiguous("payment-source update outcome unknown: " + err.Error())
		default:
			// Parsed clean rejection (bad vault id, dead subscription): it will
			// not fix itself, so the user gets an immediate terminal error, never a
			// background re-push. Only ambiguity earns system-driven repair.
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
		"new_vault_id":         newRailCustomerRef,
		"old_vault_id":         p.OldRailCustomerRef,
	})
}

// Verify resolves an ambiguous swap via the recurring record's CURRENT vault:
// new ⇒ the update landed (finalize local, done); old ⇒ it verifiably did not
// (the executor re-sends); record gone or a vault matching neither ⇒ terminal
// with a repair note — the verifier never writes provider state.
func (h *NMIPaymentSourceUpdateHandler) Verify(ctx context.Context, intent gen.BillingProviderIntent) Outcome {
	p, err := decodeNMIPaymentSourceUpdatePayload(intent)
	if err != nil {
		return Terminal(err.Error())
	}
	pin, refused, err := h.pinProviderAccount(ctx, intent, p)
	if err != nil {
		return Ambiguous("pin provider account: " + err.Error())
	}
	if refused != nil {
		return *refused
	}
	sub, newRailCustomerRef := pin.sub, pin.newRailCustomerRef
	psid := strings.TrimSpace(sub.RailSubscriptionID)
	if psid == "" {
		return Terminal("subscription has no rail subscription id; nothing exists at the provider to verify")
	}
	client, _, ok := h.resolveClient(ctx, intent, sub)
	if !ok {
		return Ambiguous(fmt.Sprintf("nmi client not configured for provider %q; cannot verify", intent.Rail))
	}
	remote, found, err := client.GetSubscription(ctx, psid)
	if err != nil {
		return Ambiguous("provider read failed: " + err.Error())
	}
	if !found {
		return Terminal(fmt.Sprintf("recurring record %s gone at provider (canceled/tombstoned) while a payment-source update was unresolved — repair: subscription lifecycle owns this, local payment-method link left untouched", psid))
	}
	cur := strings.TrimSpace(remote.CustomerVaultID)
	switch {
	case cur == newRailCustomerRef:
		if err := h.finalize(ctx, intent, p); err != nil {
			return Ambiguous("verified new vault at provider, but local finalize failed: " + err.Error())
		}
		return Succeeded(map[string]any{"verified_new_vault": true, "rail_subscription_id": psid, "new_vault_id": newRailCustomerRef})
	case strings.TrimSpace(p.OldRailCustomerRef) != "" && cur == strings.TrimSpace(p.OldRailCustomerRef):
		return Retryable("provider still bills the old vault; update verified not executed")
	case strings.TrimSpace(p.OldRailCustomerRef) == "":
		// Old side unknown locally (legacy row without a payment-method link):
		// any non-new vault means the update did not land — re-execute.
		return Retryable(fmt.Sprintf("provider bills vault %s, not the requested one; update verified not executed", cur))
	default:
		return TerminalWithEvidence(
			fmt.Sprintf("provider bills vault %s — neither this swap's old (%s) nor new (%s); out-of-band change, not overwriting — repair: re-issue the swap if still wanted", cur, p.OldRailCustomerRef, newRailCustomerRef),
			map[string]any{"provider_vault_id": cur, "old_vault_id": p.OldRailCustomerRef, "new_vault_id": newRailCustomerRef, "rail_subscription_id": psid},
		)
	}
}

func (h *NMIPaymentSourceUpdateHandler) loadSubscription(ctx context.Context, intent gen.BillingProviderIntent) (*models.Subscription, error) {
	if intent.SubscriptionID == nil {
		return nil, errors.New("intent has no subscription_id")
	}
	return subscriptions.NewSubscriptionRepo(h.DB).GetByID(ctx, *intent.SubscriptionID)
}

// resolveClient resolves the account-aware NMI client for the subscription
// (rows pinned to a PSP resolve by account key). ok=false carries the Parked
// outcome to return.
func (h *NMIPaymentSourceUpdateHandler) resolveClient(ctx context.Context, intent gen.BillingProviderIntent, sub *models.Subscription) (*nmi.NMIClient, Outcome, bool) {
	client, key, ok, err := subscriptions.NMIClientForExistingSubscription(ctx, h.Resolver, sub)
	if err != nil {
		return nil, Parked(fmt.Sprintf("resolve nmi client for provider %q: %v", intent.Rail, err)), false
	}
	if !ok {
		return nil, Parked(fmt.Sprintf("nmi rail is not armed for PSP %q", key)), false
	}
	return client, Outcome{}, true
}

// providerAccountPin is what Execute/Verify may touch the provider with: the
// subscription and the target vault ref, both re-read under the target
// instrument's shared row lock with the same-PSP invariant established.
type providerAccountPin struct {
	sub                *models.Subscription
	newRailCustomerRef string
}

// pinProviderAccount re-establishes the same-PSP invariant where provider
// traffic is sent. Under FOR SHARE on the target method (conflicting with the
// custody remap's FOR UPDATE, so the two serialize) it re-reads the
// subscription and the target and requires
//
//	subscription.psp_id == intent.psp_id == payload.new_psp_id == target.psp_id
//
// refused carries the terminal outcome (psp_mismatch evidence, or a target
// with no vault ref); err is a read failure the caller classifies. A target
// row deleted after a provider write may have landed is backstopped by the
// frozen payload: its PSP was proven at enqueue, so the swap still converges.
func (h *NMIPaymentSourceUpdateHandler) pinProviderAccount(ctx context.Context, intent gen.BillingProviderIntent, p NMIPaymentSourceUpdatePayload) (pin providerAccountPin, refused *Outcome, err error) {
	if intent.SubscriptionID == nil || *intent.SubscriptionID == uuid.Nil {
		return pin, ptr(Terminal("intent has no subscription_id")), nil
	}
	if intent.PspID == nil || *intent.PspID == uuid.Nil {
		return pin, ptr(Terminal("intent is not addressed to a PSP; the provider-account invariant cannot be established")), nil
	}
	if intent.MerchantID == uuid.Nil {
		return pin, ptr(Terminal("intent has no merchant_id; the provider-account invariant cannot be established")), nil
	}
	err = h.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		currentTargetPSP, targetFound := p.NewPspID, true
		ref := strings.TrimSpace(p.NewRailCustomerRef)
		locked, lerr := gen.New(tx).GetPaymentMethodForShare(ctx, gen.GetPaymentMethodForShareParams{
			MerchantID: intent.MerchantID, ID: p.NewPaymentMethodID,
		})
		switch {
		case lerr == nil:
			currentTargetPSP = derefUUID(locked.PspID)
			if ref = strings.TrimSpace(models.DerefStr(locked.RailCustomerRef)); ref == "" {
				refused = ptr(Terminal("target payment method has no rail customer ref; cannot repoint billing"))
				return nil
			}
		case errors.Is(lerr, pgx.ErrNoRows):
			targetFound = false
		default:
			return fmt.Errorf("lock target payment method: %w", lerr)
		}
		sub, serr := subscriptions.NewSubscriptionRepo(h.DB.NewWithPgxTx(tx)).GetByID(ctx, *intent.SubscriptionID)
		if serr != nil {
			return fmt.Errorf("load subscription: %w", serr)
		}
		if sub.PspID != *intent.PspID || *intent.PspID != p.NewPspID || p.NewPspID != currentTargetPSP {
			refused = ptr(TerminalWithEvidence(
				fmt.Sprintf("provider-account invariant violated: subscription PSP %s, intent PSP %s, target method PSP %s at enqueue / %s now — a cross-PSP payment-source update is never sent; repair: card re-entry on the subscription's active provider account (#657)",
					sub.PspID, *intent.PspID, p.NewPspID, currentTargetPSP),
				map[string]any{
					"code":                  EvidenceCodePSPMismatch,
					"subscription_psp_id":   sub.PspID.String(),
					"intent_psp_id":         intent.PspID.String(),
					"target_psp_id_frozen":  p.NewPspID.String(),
					"target_psp_id_current": currentTargetPSP.String(),
					"target_method_found":   targetFound,
				}))
			return nil
		}
		if targetFound && (locked.Custodian != models.CustodianPSP || locked.CustodianID != nil) {
			refused = ptr(TerminalWithEvidence(subscriptions.ErrPaymentMethodNotPSPVaulted.Error(), map[string]any{"code": subscriptions.ErrPaymentMethodNotPSPVaulted.Code}))
			return nil
		}
		pin = providerAccountPin{sub: sub, newRailCustomerRef: ref}
		return nil
	})
	return pin, refused, err
}

// finalize points the local subscription at the new payment method (its own
// card, or none to follow the default) — only ever called AFTER the provider
// side is confirmed. Idempotent; a subscription row gone out-of-band leaves
// nothing to finalize. A subscription waiting for a new card resumes dunning
// on it. Its recurring mandate moves with it, citing the new card's recurring
// lineage, else the verification the swap carries.
func (h *NMIPaymentSourceUpdateHandler) finalize(ctx context.Context, intent gen.BillingProviderIntent, p NMIPaymentSourceUpdatePayload) error {
	return h.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		repo := subscriptions.NewSubscriptionRepo(h.DB.NewWithPgxTx(tx))
		sub, err := repo.GetByIDForUpdate(ctx, *intent.SubscriptionID)
		if err != nil {
			if db.IsNotFound(err) {
				return nil
			}
			return err
		}
		if !p.Follow && sub.PaymentMethodID != nil && *sub.PaymentMethodID == p.NewPaymentMethodID {
			return nil
		}
		now := h.now()
		newID := p.NewPaymentMethodID
		lineage, err := mandates.Citable(ctx, q, intent.MerchantID, sub.CustomerID, newID, sub.PspID, string(sub.Rail), charge.AgreementRecurring)
		if err != nil {
			return err
		}
		if lineage == nil && p.VerifiedTransactionID != "" {
			lineage = &charge.Mandate{Kind: charge.AgreementRecurring, InitialTransactionID: p.VerifiedTransactionID}
		}
		if _, err := mandates.Replace(ctx, q, mandates.Agreement{MerchantID: intent.MerchantID, CustomerID: sub.CustomerID, PaymentMethodID: newID, PSPID: sub.PspID,
			Rail: string(sub.Rail), Kind: charge.AgreementRecurring, SubscriptionID: &sub.ID, Lineage: lineage, AcceptedAt: now}, now); err != nil {
			return err
		}
		sub.PaymentMethodID = &newID
		if p.Follow {
			sub.PaymentMethodID = nil
		}
		if err := subscriptions.ReplaceMethod(sub, now); err != nil {
			return err
		}
		return repo.UpdateAt(ctx, sub, now)
	})
}

// ErrPaymentSourceUpdateProcessing: the durable swap intent could not confirm
// the provider update inline (ambiguous outcome, parked provider). The intent
// log converges local and remote; a retried request maps onto the same intent.
var ErrPaymentSourceUpdateProcessing = errors.New("payment method update is processing; retry the same request to check the result")

// PaymentSourceUpdateOutcome mirrors the durable intent's post-execution state
// for the two swap call sites. Neither Done nor Terminal = still resolving
// out-of-band (surface ErrPaymentSourceUpdateProcessing, not success/decline).
type PaymentSourceUpdateOutcome struct {
	// Done: the provider bills the new vault and the local row points at it.
	Done bool
	// Terminal: the swap failed permanently; Reason says why and Code is the
	// intent's evidence code when the refusal is classified (psp_mismatch).
	Terminal bool
	Reason   string
	Code     string
}

// PaymentSourceUpdateThrough posts the durable nmi_payment_source_update
// intent and executes it inline (write-through).
type PaymentSourceUpdateThrough struct {
	Runner *Runner
	DB     *db.DB
}

// ExecutePaymentSourceUpdate moves sub's NMI schedule onto newPM: it enqueues
// the durable swap and executes it inline.
func (t *PaymentSourceUpdateThrough) ExecutePaymentSourceUpdate(ctx context.Context, sub *models.Subscription, newPM *models.PaymentMethod, swap subscriptions.PaymentSourceSwap, origin Origin, originReason string) (PaymentSourceUpdateOutcome, error) {
	if t == nil || t.Runner == nil || t.DB == nil {
		return PaymentSourceUpdateOutcome{}, errors.New("payment-source update intent runner not wired")
	}
	if sub == nil || newPM == nil {
		return PaymentSourceUpdateOutcome{}, errors.New("subscription and payment method are required")
	}
	// Check the account boundary here, whatever the HTTP caller checked: the
	// target is re-read under its shared row lock, so a custody remap in flight
	// commits first and is seen (a later remap is caught by the executor's
	// pin). A target on another PSP never becomes an intent.
	var params EnqueueParams
	err := t.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		row, lerr := gen.New(tx).GetPaymentMethodForShare(ctx, gen.GetPaymentMethodForShareParams{MerchantID: sub.MerchantID, ID: newPM.ID})
		if lerr != nil {
			if errors.Is(lerr, pgx.ErrNoRows) {
				return fmt.Errorf("payment method %s: %w", newPM.ID, paymentmethods.ErrPaymentMethodNotFound)
			}
			return lerr
		}
		var err error
		params, err = t.swapParams(ctx, t.DB.NewWithPgxTx(tx), sub, row, swap, origin, originReason)
		return err
	})
	if err != nil {
		return PaymentSourceUpdateOutcome{}, err
	}
	row, err := t.Runner.EnqueueAndExecute(ctx, params)
	if err != nil {
		return PaymentSourceUpdateOutcome{}, err
	}
	return swapOutcome(row), nil
}

// EnqueueSwap records the swap of sub's schedule onto target, a card the
// caller holds locked, in the caller's transaction (subscriptions.
// PaymentSourceSwapper); ExecuteSwap runs it once that commits.
func (t *PaymentSourceUpdateThrough) EnqueueSwap(ctx context.Context, tx pgx.Tx, sub *models.Subscription, target gen.BillingPaymentMethod, swap subscriptions.PaymentSourceSwap) (uuid.UUID, error) {
	if t == nil || t.Runner == nil || t.DB == nil {
		return uuid.Nil, errors.New("payment-source update intent runner not wired")
	}
	d := t.DB.NewWithPgxTx(tx)
	params, err := t.swapParams(ctx, d, sub, target, swap, OriginUser, "default payment method change")
	if err != nil {
		return uuid.Nil, err
	}
	store := NewStore(d)
	if gated, ok := t.Runner.Store.(*Store); ok {
		store = gated.withTxDB(d)
	}
	row, err := store.Enqueue(ctx, params)
	return row.ID, err
}

// ExecuteSwap runs a committed swap inline; what does not settle is the
// executor's.
func (t *PaymentSourceUpdateThrough) ExecuteSwap(ctx context.Context, intentID uuid.UUID) error {
	_, err := t.Runner.ExecuteByID(ctx, intentID)
	return err
}

// swapParams is the durable swap of sub's schedule onto target, validated
// against the provider-account boundary.
func (t *PaymentSourceUpdateThrough) swapParams(ctx context.Context, d *db.DB, sub *models.Subscription, row gen.BillingPaymentMethod, swap subscriptions.PaymentSourceSwap, origin Origin, originReason string) (EnqueueParams, error) {
	tid, err := merchant.Require(ctx)
	if err != nil {
		return EnqueueParams{}, err
	}
	target, err := models.PaymentMethodFromGen(row)
	if err != nil {
		return EnqueueParams{}, err
	}
	// Refuse zero identifiers: an unattributed subscription or instrument
	// cannot be compared to a provider account, and a zero id must never
	// become a "not found" lookup or a durable intent.
	if err := idguard.RequireMerchant("merchant_id", tid); err != nil {
		return EnqueueParams{}, err
	}
	for _, check := range []struct {
		field string
		id    uuid.UUID
	}{
		{"subscription_id", sub.ID},
		{"subscription.psp_id", sub.PspID},
		{"payment_method_id", target.ID},
		{"payment_method.psp_id", target.HoldingPSP()},
	} {
		if err := idguard.Require(check.field, check.id); err != nil {
			return EnqueueParams{}, err
		}
	}
	if err := subscriptions.ValidatePaymentMethodProviderAccount(target, sub); err != nil {
		return EnqueueParams{}, err
	}
	if err := subscriptions.ValidatePaymentMethodSourceCustody(target); err != nil {
		return EnqueueParams{}, err
	}
	newRailCustomerRef := strings.TrimSpace(target.RailCustomerRef)
	if newRailCustomerRef == "" {
		return EnqueueParams{}, errors.New("target payment method has no rail customer ref")
	}

	// Old side: forensics + the verifier's old-vault comparison anchor. A
	// missing/unlinked old method degrades to "old unknown" (verify re-executes
	// on any non-new vault).
	oldPMID := swap.Old
	if oldPMID == nil {
		if oldPMID, err = subscriptions.PaymentMethodOf(ctx, d.Gen(ctx), sub); err != nil {
			return EnqueueParams{}, err
		}
	}
	var oldRailCustomerRef string
	if oldPMID != nil {
		old, err := d.Gen(ctx).GetPaymentMethodByID(ctx, gen.GetPaymentMethodByIDParams{MerchantID: tid.UUID(), ID: *oldPMID})
		switch {
		case err == nil:
			oldRailCustomerRef = strings.TrimSpace(models.DerefStr(old.RailCustomerRef))
		case errors.Is(err, pgx.ErrNoRows):
			// linked row gone; old vault stays unknown
		default:
			return EnqueueParams{}, fmt.Errorf("load current payment method: %w", err)
		}
	}

	// NMI bills a vault's primary card: another billing entry of the same
	// vault cannot become the subscription's source (the swap would be a
	// no-op at NMI reported as success).
	if oldPMID != nil && *oldPMID != target.ID && oldRailCustomerRef == newRailCustomerRef {
		return EnqueueParams{}, subscriptions.ErrPaymentMethodSameVault
	}

	subID := sub.ID
	priorSwaps, err := d.Gen(ctx).CountProviderIntents(ctx, gen.CountProviderIntentsParams{
		MerchantID:     tid.UUID(),
		Status:         ptr(StatusSucceeded),
		IntentType:     ptr(TypeNMIPaymentSourceUpdate),
		SubscriptionID: &subID,
	})
	if err != nil {
		return EnqueueParams{}, fmt.Errorf("count prior payment-source updates: %w", err)
	}
	return EnqueueParams{
		MerchantID:     tid.UUID(),
		Provider:       strings.ToLower(string(sub.Rail)),
		IntentType:     TypeNMIPaymentSourceUpdate,
		SubscriptionID: &subID,
		PspID:          sub.PspID,
		Payload: NMIPaymentSourceUpdatePayload{
			UserID:                sub.CustomerID.String(),
			RailSubscriptionID:    sub.RailSubscriptionID,
			NewPaymentMethodID:    target.ID,
			NewRailCustomerRef:    newRailCustomerRef,
			NewPspID:              target.HoldingPSP(),
			OldPaymentMethodID:    oldPMID,
			OldRailCustomerRef:    oldRailCustomerRef,
			Follow:                swap.Follow,
			VerifiedTransactionID: swap.Verified,
		},
		IdempotencyKey: NMIPaymentSourceUpdateIdempotencyKey(sub.ID, newRailCustomerRef, priorSwaps),
		NextAttemptAt:  time.Now().UTC(),
		Origin:         origin,
		OriginReason:   originReason,
	}, nil
}

func swapOutcome(row gen.BillingProviderIntent) PaymentSourceUpdateOutcome {
	out := PaymentSourceUpdateOutcome{}
	if row.LastFailureReason != nil {
		out.Reason = *row.LastFailureReason
	}
	switch row.Status {
	case StatusSucceeded:
		out.Done = true
	case StatusFailedTerminal, StatusSuperseded, StatusExpired:
		out.Terminal = true
		out.Code = EvidenceString(row, "code")
	}
	return out
}

func ptr[T any](v T) *T { return &v }
