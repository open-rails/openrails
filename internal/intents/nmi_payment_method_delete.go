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
	"github.com/open-rails/openrails/internal/shared/timeutil"

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

// NMIPaymentMethodDeletePayload freezes the owned target and deletion scope.
// Recovery re-reads the accepted local fence before verifying this exact target.
type NMIPaymentMethodDeletePayload struct {
	BillingEntryOnly bool      `json:"billing_entry_only"`
	UserID           string    `json:"user_id"`
	PaymentMethodID  uuid.UUID `json:"payment_method_id"`
	RailCustomerRef  string    `json:"rail_customer_ref,omitempty"`
	RailMethodRef    string    `json:"rail_method_ref,omitempty"`
}

// RailClientResolver resolves the per-merchant NMI client for a payment
// method (implemented by paymentmethods.RailPaymentMethodService).
type RailClientResolver interface {
	ResolveClientForPaymentMethod(ctx context.Context, pm *models.PaymentMethod) (*nmi.NMIClient, error)
}

// NMIPaymentMethodDeleteHandler verifies the accepted native-vault target,
// retains its admission fence through uncertainty, and commits removal with
// the terminal receipt. Live subscriptions and unresolved operations block
// admission and execution; a missing local row cannot prove remote erasure.
type NMIPaymentMethodDeleteHandler struct {
	DB     *db.DB
	Rails  RailClientResolver
	Policy BackoffPolicy
	Clock  clockwork.Clock
}

func NewNMIPaymentMethodDeleteHandler(d *db.DB, rails RailClientResolver, clocks ...clockwork.Clock) *NMIPaymentMethodDeleteHandler {
	return &NMIPaymentMethodDeleteHandler{DB: d, Rails: rails, Policy: DefaultBackoff, Clock: timeutil.FirstClock(clocks...)}
}

func (h *NMIPaymentMethodDeleteHandler) Type() string               { return TypeNMIPaymentMethodDelete }
func (*NMIPaymentMethodDeleteHandler) CommitsTerminalOutcome() bool { return true }
func (*NMIPaymentMethodDeleteHandler) PrunePolicy() (bool, bool)    { return true, true }
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
	if p.PaymentMethodID == uuid.Nil || p.BillingEntryOnly && p.RailMethodRef == "" {
		return p, errors.New("nmi vault delete payload is incomplete")
	}
	return p, nil
}

// Accepted deletion remains pending if an external observation makes the
// method live again. Preserve its fence until use resolves; never strand a
// delete fence behind a superseded operation. Missing rows retain target refs.
func (h *NMIPaymentMethodDeleteHandler) CheckRelevance(ctx context.Context, intent gen.OpenrailsRailIntent) (Relevance, error) {
	p, err := decodeNMIVaultDeletePayload(intent)
	if err != nil {
		return StillRelevant(), nil // Execute reports the terminal payload error
	}
	_, err = h.loadPaymentMethod(ctx, intent, p)
	return StillRelevant(), err
}

func (h *NMIPaymentMethodDeleteHandler) Execute(ctx context.Context, intent gen.OpenrailsRailIntent) Outcome {
	p, err := decodeNMIVaultDeletePayload(intent)
	if err != nil {
		return Parked(err.Error())
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
		return h.complete(ctx, intent, p, map[string]any{"no_rail_customer_ref": true})
	}

	shared := p.BillingEntryOnly
	if shared && strings.TrimSpace(pm.RailMethodRef) == "" {
		// Cannot identify WHICH billing entry is this card — refuse rather than
		// destroy the siblings (#682 shared-vault contract).
		return Parked(fmt.Sprintf("vault %s is shared by other stored payment methods and this row carries no billing id to scope the delete", vaultID))
	}

	// Verify-then-execute: absent at the provider IS success.
	customer, present, err := h.vaultCustomer(ctx, client, vaultID)
	if err != nil {
		return Retryable("provider read before delete failed: " + err.Error())
	}
	if !present {
		return h.complete(ctx, intent, p, map[string]any{"verified_absent": true, "vault_id": vaultID})
	}
	if shared && !billingEntryPresent(customer, pm.RailMethodRef) {
		return h.complete(ctx, intent, p, map[string]any{"verified_entry_absent": true, "vault_id": vaultID, "billing_id": pm.RailMethodRef})
	}

	// Local aliases alone cannot authorize erasing provider-only addresses.
	// NMI's whole-customer DELETE removes every billing/shipping entry.
	if !shared && (len(customer.Billing) != 1 || pm.RailMethodRef == "" || customer.Billing[0].ID != pm.RailMethodRef) {
		return Parked("native vault contains unqualified billing entries; whole-vault deletion refused")
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
			return h.complete(ctx, intent, p, map[string]any{"already_absent": true, "vault_id": vaultID})
		case nmi.IsTransportAmbiguous(err):
			// The delete MAY have landed; the verifier resolves via reads.
			return Ambiguous("vault delete outcome unknown: " + err.Error())
		default:
			// Parsed clean rejection: the delete definitely did not execute.
			return Retryable("vault delete rejected cleanly: " + err.Error())
		}
	}

	evidence := map[string]any{"deleted": true, "vault_id": vaultID}
	if shared {
		evidence["scoped_to_billing_entry"] = pm.RailMethodRef
	}
	return h.complete(ctx, intent, p, evidence)
}

// Verify resolves an ambiguous delete via provider READS: vault (or billing
// entry) absent means the delete is done; present means it verifiably did not
// happen and the executor may retry.
func (h *NMIPaymentMethodDeleteHandler) Verify(ctx context.Context, intent gen.OpenrailsRailIntent) Outcome {
	p, err := decodeNMIVaultDeletePayload(intent)
	if err != nil {
		return Parked(err.Error())
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
		return h.complete(ctx, intent, p, map[string]any{"no_rail_customer_ref": true})
	}
	shared := p.BillingEntryOnly
	customer, present, err := h.vaultCustomer(ctx, client, vaultID)
	if err != nil {
		return Ambiguous("provider read failed: " + err.Error())
	}
	if !present {
		return h.complete(ctx, intent, p, map[string]any{"verified_absent": true, "vault_id": vaultID})
	}
	if shared && strings.TrimSpace(pm.RailMethodRef) != "" && !billingEntryPresent(customer, pm.RailMethodRef) {
		return h.complete(ctx, intent, p, map[string]any{"verified_entry_absent": true, "vault_id": vaultID, "billing_id": pm.RailMethodRef})
	}
	return Retryable("vault still present at provider; delete verified not executed")
}

// loadPaymentMethod requires the canonical accepted row and its fence. Atomic
// completion retains that row until the terminal receipt commits with removal.
func (h *NMIPaymentMethodDeleteHandler) loadPaymentMethod(ctx context.Context, intent gen.OpenrailsRailIntent, p NMIPaymentMethodDeletePayload) (*models.PaymentMethod, error) {
	scope, scopeErr := merchant.Require(ctx)
	if scopeErr != nil || scope.UUID() != intent.MerchantID || intent.IntentType != TypeNMIPaymentMethodDelete {
		return nil, paymentmethods.ErrPaymentMethodDeleteUnsafe
	}
	var pm *models.PaymentMethod
	customer, err := uuid.Parse(p.UserID)
	if err != nil {
		return nil, paymentmethods.ErrPaymentMethodDeleteUnsafe
	}
	err = h.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		if _, err := q.LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: intent.MerchantID, ID: customer}); err != nil {
			return err
		}
		if err := paymentmethods.LockNativeVault(ctx, q, intent.MerchantID, derefUUID(intent.PspID), p.RailCustomerRef); err != nil {
			return err
		}
		row, err := q.LockPaymentMethodForCustodyRemap(ctx, gen.LockPaymentMethodForCustodyRemapParams{MerchantID: intent.MerchantID, ID: p.PaymentMethodID})
		if err != nil {
			return err
		}
		if intent.PspID == nil || row.PspID != *intent.PspID || row.CustomerID != customer || row.Custodian != models.CustodianPSP || row.RailCustomerRef != p.RailCustomerRef || row.RailMethodRef != p.RailMethodRef || row.ParkReason != "delete:"+intent.ID.String() {
			return paymentmethods.ErrPaymentMethodDeleteUnsafe
		}
		if err := deletionMethodUnused(ctx, q, intent.MerchantID, p.PaymentMethodID, 1); err != nil {
			return err
		}
		pm, err = models.PaymentMethodFromGen(row)
		return err
	})
	return pm, err
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

// complete commits the exact fenced local removal and retained receipt together.
// A failed ledger write rolls back removal, so recovery can still resolve the
// canonical method and verify its accepted native-vault target.
func (h *NMIPaymentMethodDeleteHandler) complete(ctx context.Context, in gen.OpenrailsRailIntent, p NMIPaymentMethodDeletePayload, evidence map[string]any) Outcome {
	ctx, cancel := LedgerWriteContext(ctx)
	defer cancel()
	customer, err := uuid.Parse(p.UserID)
	if err != nil {
		return Ambiguous("invalid accepted deletion payer")
	}
	err = h.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := h.DB.NewWithPgxTx(tx)
		q := d.Gen(ctx)
		if _, err := q.LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: in.MerchantID, ID: customer}); err != nil {
			return err
		}
		if err := paymentmethods.LockNativeVault(ctx, q, in.MerchantID, derefUUID(in.PspID), p.RailCustomerRef); err != nil {
			return err
		}
		method, err := q.LockPaymentMethodForCustodyRemap(ctx, gen.LockPaymentMethodForCustodyRemapParams{MerchantID: in.MerchantID, ID: p.PaymentMethodID})
		methodErr := err
		current, err := q.LockNativeMethodDelete(ctx, gen.LockNativeMethodDeleteParams{MerchantID: in.MerchantID, ID: in.ID})
		if err != nil {
			return err
		}
		accepted, err := decodeNMIVaultDeletePayload(current)
		if err != nil {
			return err
		}
		if accepted != p || current.PspID == nil || in.PspID == nil || *current.PspID != *in.PspID || current.Rail != in.Rail || current.IdempotencyKey != NMIPaymentMethodDeleteIdempotencyKey(p.PaymentMethodID) {
			return paymentmethods.ErrPaymentMethodDeleteUnsafe
		}
		if current.Status == StatusSucceeded {
			return nil
		}
		if methodErr != nil {
			return methodErr
		}
		if current.Status != StatusInFlight && current.Status != StatusUnknownNeedsVerify {
			return paymentmethods.ErrPaymentMethodDeleteUnsafe
		}
		if method.CustomerID != customer || method.PspID != *current.PspID || method.Custodian != models.CustodianPSP || method.CustodianID != nil || method.Rail != current.Rail || method.RailCustomerRef != p.RailCustomerRef || method.RailMethodRef != p.RailMethodRef {
			return paymentmethods.ErrPaymentMethodDeleteUnsafe
		}
		n, err := q.DeleteFencedPaymentMethod(ctx, gen.DeleteFencedPaymentMethodParams{MerchantID: in.MerchantID, ID: p.PaymentMethodID, OperationID: in.ID})
		if err != nil {
			return err
		}
		if n != 1 {
			return paymentmethods.ErrPaymentMethodDeleteUnsafe
		}
		raw, err := json.Marshal(evidence)
		if err != nil {
			return err
		}
		n, err = q.CompleteCustodianMethodDelete(ctx, gen.CompleteCustodianMethodDeleteParams{MerchantID: in.MerchantID, ID: in.ID, Evidence: raw, Now: h.Clock.Now().UTC()})
		if err != nil {
			return err
		}
		if n != 1 {
			return paymentmethods.ErrPaymentMethodDeleteUnsafe
		}
		return nil
	})
	if err != nil {
		return Ambiguous("native deletion confirmed; atomic local completion pending: " + err.Error())
	}
	return Succeeded(evidence)
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
	if pm != nil && pm.Custodian == models.CustodianHyperSwitch {
		h, ok := t.Runner.Registry.Lookup(TypeHyperSwitchMethodDelete).(*HyperSwitchMethodDeleteHandler)
		store, stored := t.Runner.Store.(*Store)
		if !ok || !stored {
			return paymentmethods.PaymentMethodDeleteOutcome{}, errors.New("custodian delete admission is not wired")
		}
		row, err := h.admit(ctx, store, pm)
		if err != nil {
			return paymentmethods.PaymentMethodDeleteOutcome{}, err
		}
		row, err = t.Runner.ExecuteByID(ctx, row.ID)
		if err != nil {
			return paymentmethods.PaymentMethodDeleteOutcome{}, err
		}
		return paymentmethods.PaymentMethodDeleteOutcome{Done: row.Status == StatusSucceeded}, nil
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return paymentmethods.PaymentMethodDeleteOutcome{}, err
	}
	origin, actor := paymentMethodDeleteAuthority(ctx, pm.CustomerID)
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
		NextAttemptAt:  t.Runner.now(),
		Origin:         origin,
		Actor:          actor,
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
