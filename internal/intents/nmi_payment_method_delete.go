package intents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/pkg/merchant"
)

// TypeNMIPaymentMethodDelete is the durable NMI customer-vault delete (#674 tail): a
// user-initiated payment-method delete posts this write-ahead intent and
// executes it inline. Once accepted, the delete can never be lost — a crash or
// transport-ambiguous response routes through the verifier, whose read answers
// "vault (or billing entry) absent at the provider ⇒ done" (the
// tombstone-reads-as-gone pattern from nmi_delete.go).
//
// or#870 — THE STANDING RULE, and why this type is USER-ONLY.
// OpenRails never deletes a stored payment method. Not on expiry, not on a
// stolen card, not on cancellation, not ever. Only the end user does. This
// intent has exactly ONE producer — PaymentMethodDeleteThrough, reached only from the
// authenticated DELETE /payment-methods/:id route after an ownership check —
// and it must keep exactly one. If you are adding a caller because a
// subscription died, you are looking for TypeNMIDeleteSubscription, which
// cancels the recurring SCHEDULE at the rail and leaves the instrument alone so
// the customer can update or remove it themselves.
//
// The only other remote-vault deletes in the codebase roll back a vault minted
// MILLISECONDS earlier in the same request — creation that failed to persist
// locally, or a checkout whose very first charge was declined. They are gated
// on `createdVault` and can never reach an instrument the customer has saved.
// See paymentmethods.CleanupPaymentMethodBestEffort.
// The VALUE is deliberately unchanged: it is persisted in
// rail_intents.intent_type, and renaming a symbol must never rewrite history
// (or#871 — `vault` is reserved for HashiCorp, but a stored row is evidence).
const TypeNMIPaymentMethodDelete = "nmi_vault_delete"

// NMIPaymentMethodDeleteIdempotencyKey is the logical identity of "the delete of this
// stored payment method": repeated delete requests map onto one intent.
func NMIPaymentMethodDeleteIdempotencyKey(paymentMethodID uuid.UUID) string {
	return TypeNMIPaymentMethodDelete + ":" + paymentMethodID.String()
}

// NMIPaymentMethodDeletePayload is the stored payload. The payment-method row is
// re-read at execution time when it still exists; the ref copies let the
// handler finish the remote delete even if the local row vanished out-of-band.
type NMIPaymentMethodDeletePayload struct {
	UserID          string    `json:"user_id"`
	PaymentMethodID uuid.UUID `json:"payment_method_id"`
	RailCustomerRef string    `json:"rail_customer_ref,omitempty"`
	RailMethodRef   string    `json:"rail_method_ref,omitempty"`
}

// RailClientResolver resolves the per-merchant NMI client for a payment
// method (implemented by paymentmethods.RailPaymentMethodService).
type RailClientResolver interface {
	ResolveClientForPaymentMethod(ctx context.Context, pm *models.PaymentMethod) (*nmi.NMIClient, error)
}

// NMIPaymentMethodDeleteHandler implements verify-then-execute deletion of an NMI
// customer vault (or, for #682 shared vaults, of ONE billing entry):
//
//   - relevance: superseded only when the payment method is back in use by an
//     active, pending, or past-due subscription (never destroy billing state
//     in use). A missing local row does NOT supersede — the remote delete may
//     still be pending and the payload refs carry everything needed to finish it.
//   - execute: read the vault first — absent IS success (deletes are
//     idempotent by observation); present -> delete. Transport-ambiguous
//     outcomes go to the verifier; parsed clean rejections retry.
//   - finalize: remove the local row (idempotent — already-gone is fine).
type NMIPaymentMethodDeleteHandler struct {
	DB     *db.DB
	Rails  RailClientResolver
	Policy BackoffPolicy
}

func NewNMIPaymentMethodDeleteHandler(d *db.DB, rails RailClientResolver) *NMIPaymentMethodDeleteHandler {
	return &NMIPaymentMethodDeleteHandler{DB: d, Rails: rails, Policy: DefaultBackoff}
}

func (h *NMIPaymentMethodDeleteHandler) Type() string { return TypeNMIPaymentMethodDelete }
func (h *NMIPaymentMethodDeleteHandler) Backoff(attempts int32) time.Duration {
	return h.Policy.Delay(attempts)
}

func decodeNMIVaultDeletePayload(intent gen.OpenrailsRailIntent) (NMIPaymentMethodDeletePayload, error) {
	var p NMIPaymentMethodDeletePayload
	if len(intent.Payload) == 0 {
		return p, errors.New("nmi vault delete intent has no payload")
	}
	if err := json.Unmarshal(intent.Payload, &p); err != nil {
		return p, fmt.Errorf("decode nmi vault delete payload: %w", err)
	}
	if p.PaymentMethodID == uuid.Nil {
		return p, errors.New("nmi vault delete payload is incomplete")
	}
	return p, nil
}

// CheckRelevance: the delete stays applicable unless the payment method is
// back in use by a live subscription (enqueue-to-execute race with a new
// checkout picking the stored card). Missing rows stay relevant — Execute
// resolves them off the payload refs.
func (h *NMIPaymentMethodDeleteHandler) CheckRelevance(ctx context.Context, intent gen.OpenrailsRailIntent) (Relevance, error) {
	p, err := decodeNMIVaultDeletePayload(intent)
	if err != nil {
		return StillRelevant(), nil // Execute reports the terminal payload error
	}
	subs, err := h.DB.Gen(ctx).ListSubscriptionsByPaymentMethodIDs(ctx, []uuid.UUID{p.PaymentMethodID})
	if err != nil {
		return Relevance{}, err
	}
	for _, sub := range subs {
		status := string(sub.Status)
		if status == string(models.StatusActive) || status == string(models.StatusPending) || status == string(models.StatusPastDue) {
			return SupersededBy(fmt.Sprintf("payment method back in use by subscription %s (status=%s); delete no longer applies", sub.ID, status)), nil
		}
	}
	return StillRelevant(), nil
}

func (h *NMIPaymentMethodDeleteHandler) Execute(ctx context.Context, intent gen.OpenrailsRailIntent) Outcome {
	p, err := decodeNMIVaultDeletePayload(intent)
	if err != nil {
		return Terminal(err.Error())
	}
	pm, err := h.loadPaymentMethod(ctx, intent, p)
	if err != nil {
		return Retryable("load payment method: " + err.Error())
	}
	if h.Rails == nil {
		return Parked("rail client resolver not wired")
	}
	client, err := h.Rails.ResolveClientForPaymentMethod(ctx, pm)
	if err != nil {
		return Parked(fmt.Sprintf("nmi client not configured for provider %q: %v", intent.Rail, err))
	}
	if client.ReadOnly {
		return Parked("nmi client is read-only (mode=readonly)")
	}

	vaultID := strings.TrimSpace(pm.RailCustomerRef)
	if vaultID == "" {
		// Nothing exists remotely; finalize locally.
		if err := h.finalize(ctx, p.PaymentMethodID); err != nil {
			return Ambiguous("no rail customer ref, but local finalize failed: " + err.Error())
		}
		return Succeeded(map[string]any{"no_rail_customer_ref": true})
	}

	shared, err := h.sharedVault(ctx, pm)
	if err != nil {
		return Retryable("check vault sharing: " + err.Error())
	}
	if shared && strings.TrimSpace(pm.RailMethodRef) == "" {
		// Cannot identify WHICH billing entry is this card — refuse rather than
		// destroy the siblings (#682 shared-vault contract).
		return Terminal(fmt.Sprintf("vault %s is shared by other stored payment methods and this row carries no billing id to scope the delete", vaultID))
	}

	// Verify-then-execute: absent at the provider IS success.
	customer, present, err := h.vaultCustomer(ctx, client, vaultID)
	if err != nil {
		return Retryable("provider read before delete failed: " + err.Error())
	}
	if !present {
		if err := h.finalize(ctx, p.PaymentMethodID); err != nil {
			return Ambiguous("verified absent at provider, but local finalize failed: " + err.Error())
		}
		return Succeeded(map[string]any{"verified_absent": true, "vault_id": vaultID})
	}
	if shared && !billingEntryPresent(customer, pm.RailMethodRef) {
		if err := h.finalize(ctx, p.PaymentMethodID); err != nil {
			return Ambiguous("verified billing entry absent at provider, but local finalize failed: " + err.Error())
		}
		return Succeeded(map[string]any{"verified_entry_absent": true, "vault_id": vaultID, "billing_id": pm.RailMethodRef})
	}

	if shared {
		err = client.DeleteCustomerBillingEntry(ctx, vaultID, pm.RailMethodRef)
	} else {
		err = client.DeleteCustomerVault(ctx, nmi.DeleteCustomerVaultData{CustomerVaultID: vaultID})
	}
	if err != nil {
		switch {
		case errors.Is(err, nmi.ErrProviderReadOnly):
			return Parked("nmi provider writes blocked (mode=readonly)")
		case errors.Is(err, nmi.ErrV5NotFound):
			// Already gone (raced delete): absent is success.
			if ferr := h.finalize(ctx, p.PaymentMethodID); ferr != nil {
				return Ambiguous("already absent at provider, but local finalize failed: " + ferr.Error())
			}
			return Succeeded(map[string]any{"already_absent": true, "vault_id": vaultID})
		case nmi.IsTransportAmbiguous(err):
			// The delete MAY have landed; the verifier resolves via reads.
			return Ambiguous("vault delete outcome unknown: " + err.Error())
		default:
			// Parsed clean rejection: the delete definitely did not execute.
			return Retryable("vault delete rejected cleanly: " + err.Error())
		}
	}

	if err := h.finalize(ctx, p.PaymentMethodID); err != nil {
		// The provider delete DID happen; the verifier retries finalize (its
		// read will see the vault/entry absent).
		return Ambiguous("deleted at provider, but local finalize failed: " + err.Error())
	}
	evidence := map[string]any{"deleted": true, "vault_id": vaultID}
	if shared {
		evidence["scoped_to_billing_entry"] = pm.RailMethodRef
	}
	return Succeeded(evidence)
}

// Verify resolves an ambiguous delete via provider READS: vault (or billing
// entry) absent means the delete is done; present means it verifiably did not
// happen and the executor may retry.
func (h *NMIPaymentMethodDeleteHandler) Verify(ctx context.Context, intent gen.OpenrailsRailIntent) Outcome {
	p, err := decodeNMIVaultDeletePayload(intent)
	if err != nil {
		return Terminal(err.Error())
	}
	pm, err := h.loadPaymentMethod(ctx, intent, p)
	if err != nil {
		return Ambiguous("load payment method: " + err.Error())
	}
	if h.Rails == nil {
		return Ambiguous("vault client resolver not wired; cannot verify")
	}
	client, err := h.Rails.ResolveClientForPaymentMethod(ctx, pm)
	if err != nil {
		return Ambiguous(fmt.Sprintf("nmi client not configured for provider %q; cannot verify", intent.Rail))
	}
	vaultID := strings.TrimSpace(pm.RailCustomerRef)
	if vaultID == "" {
		if err := h.finalize(ctx, p.PaymentMethodID); err != nil {
			return Ambiguous("no rail customer ref, but local finalize failed: " + err.Error())
		}
		return Succeeded(map[string]any{"no_rail_customer_ref": true})
	}
	shared, err := h.sharedVault(ctx, pm)
	if err != nil {
		return Ambiguous("check vault sharing: " + err.Error())
	}
	customer, present, err := h.vaultCustomer(ctx, client, vaultID)
	if err != nil {
		return Ambiguous("provider read failed: " + err.Error())
	}
	if !present {
		if err := h.finalize(ctx, p.PaymentMethodID); err != nil {
			return Ambiguous("verified absent at provider, but local finalize failed: " + err.Error())
		}
		return Succeeded(map[string]any{"verified_absent": true, "vault_id": vaultID})
	}
	if shared && strings.TrimSpace(pm.RailMethodRef) != "" && !billingEntryPresent(customer, pm.RailMethodRef) {
		if err := h.finalize(ctx, p.PaymentMethodID); err != nil {
			return Ambiguous("verified billing entry absent at provider, but local finalize failed: " + err.Error())
		}
		return Succeeded(map[string]any{"verified_entry_absent": true, "vault_id": vaultID, "billing_id": pm.RailMethodRef})
	}
	return Retryable("vault still present at provider; delete verified not executed")
}

// loadPaymentMethod re-reads the row when it exists; a row deleted out-of-band
// falls back to a synthetic method built from the immutable payload refs so
// the remote delete can still be finished.
func (h *NMIPaymentMethodDeleteHandler) loadPaymentMethod(ctx context.Context, intent gen.OpenrailsRailIntent, p NMIPaymentMethodDeletePayload) (*models.PaymentMethod, error) {
	pm, err := paymentmethods.NewPaymentMethodRepo(h.DB).GetByID(ctx, p.PaymentMethodID)
	if err == nil {
		return pm, nil
	}
	if !errors.Is(err, paymentmethods.ErrPaymentMethodNotFound) {
		return nil, err
	}
	return &models.PaymentMethod{
		ID:              p.PaymentMethodID,
		Rail:            models.Rail(strings.ToLower(intent.Rail)),
		RailCustomerRef: p.RailCustomerRef,
		RailMethodRef:   p.RailMethodRef,
		PspID:           derefUUID(intent.PspID),
	}, nil
}

// sharedVault reports whether OTHER local payment-method rows still share the
// vault id (#682) — recomputed at execution time, never trusted from enqueue.
func (h *NMIPaymentMethodDeleteHandler) sharedVault(ctx context.Context, pm *models.PaymentMethod) (bool, error) {
	if strings.TrimSpace(pm.RailCustomerRef) == "" {
		return false, nil
	}
	n, err := paymentmethods.NewPaymentMethodRepo(h.DB).CountSharingCustomerRef(ctx, strings.ToLower(string(pm.Rail)), pm.RailCustomerRef, pm.ID)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// vaultCustomer reads the id-filtered v5 customer roster: absent means the
// vault customer is gone at NMI.
func (h *NMIPaymentMethodDeleteHandler) vaultCustomer(ctx context.Context, client *nmi.NMIClient, vaultID string) (*nmi.V5Customer, bool, error) {
	page, err := client.ListCustomersPage(ctx, "", 0, vaultID)
	if err != nil {
		return nil, false, err
	}
	for i := range page.Customers {
		if strings.TrimSpace(page.Customers[i].ID) == vaultID {
			return &page.Customers[i], true, nil
		}
	}
	return nil, false, nil
}

func billingEntryPresent(customer *nmi.V5Customer, billingID string) bool {
	if customer == nil {
		return false
	}
	billingID = strings.TrimSpace(billingID)
	for i := range customer.Billing {
		if strings.TrimSpace(customer.Billing[i].ID) == billingID {
			return true
		}
	}
	return false
}

// finalize removes the local payment-method row. Idempotent — already-gone is
// success.
func (h *NMIPaymentMethodDeleteHandler) finalize(ctx context.Context, paymentMethodID uuid.UUID) error {
	err := paymentmethods.NewPaymentMethodRepo(h.DB).Delete(ctx, paymentMethodID)
	if errors.Is(err, paymentmethods.ErrPaymentMethodNotFound) {
		return nil
	}
	return err
}

// PaymentMethodDeleteThrough adapts the write-through Runner to the producer surface
// paymentmethods.PaymentMethodDeleteExecutor (paymentmethods cannot import intents —
// import cycle via subscriptions — so the adapter lives here).
type PaymentMethodDeleteThrough struct {
	Runner *Runner
}

func (t *PaymentMethodDeleteThrough) ExecutePaymentMethodDelete(ctx context.Context, pm *models.PaymentMethod) (paymentmethods.PaymentMethodDeleteOutcome, error) {
	if t == nil || t.Runner == nil {
		return paymentmethods.PaymentMethodDeleteOutcome{}, errors.New("vault delete intent runner not wired")
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return paymentmethods.PaymentMethodDeleteOutcome{}, err
	}
	row, err := t.Runner.EnqueueAndExecute(ctx, EnqueueParams{
		MerchantID: tid.UUID(),
		Provider:   strings.ToLower(string(pm.Rail)),
		IntentType: TypeNMIPaymentMethodDelete,
		PspID:      pm.PspID,
		Payload: NMIPaymentMethodDeletePayload{
			UserID:          pm.CustomerID.String(),
			PaymentMethodID: pm.ID,
			RailCustomerRef: pm.RailCustomerRef,
			RailMethodRef:   pm.RailMethodRef,
		},
		IdempotencyKey: NMIPaymentMethodDeleteIdempotencyKey(pm.ID),
		NextAttemptAt:  time.Now().UTC(),
		Origin:         OriginUser,
		OriginReason:   "user payment-method delete",
	})
	if err != nil {
		return paymentmethods.PaymentMethodDeleteOutcome{}, err
	}
	out := paymentmethods.PaymentMethodDeleteOutcome{}
	if row.LastFailureReason != nil {
		out.Reason = *row.LastFailureReason
	}
	switch row.Status {
	case StatusSucceeded:
		out.Done = true
	case StatusSuperseded:
		out.InUse = true
	case StatusFailedTerminal, StatusExpired:
		out.Terminal = true
	}
	return out, nil
}
