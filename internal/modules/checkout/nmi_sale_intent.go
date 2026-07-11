package checkout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/payments/rails/nmidirect"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// TypeNMISale is the synchronous checkout one-time sale (#674): the durable
// write-ahead intent posted BEFORE the NMI charge and executed inline in the
// same request (write-through). The NMI order id is derived from the intent id,
// so every re-execution of one intent reuses one order reference and the
// verify leg (FindSuccessfulSaleByOrderID) answers "was THIS sale charged?"
// regardless of where a crash/timeout hit.
const TypeNMISale = "nmi_sale"

// NMISaleIdempotencyKey addresses one logical checkout sale by the client's
// checkout idempotency key: a client retry maps onto the SAME intent (and so
// the same order id) instead of minting a fresh charge.
func NMISaleIdempotencyKey(checkoutIdempotencyKey string) string {
	return TypeNMISale + ":" + strings.TrimSpace(checkoutIdempotencyKey)
}

// NMISalePayload is the stored payload for TypeNMISale — everything Execute
// and the async verifier need to charge AND locally register the purchase
// without the originating HTTP request.
type NMISalePayload struct {
	// Provider is the RAIL ("nmi") — local row vocabulary.
	Provider string `json:"provider"`
	// PSP is the payment provider (account key, e.g. "mobius")
	// this charge runs through; "" resolves the rail's active account.
	PSP             string `json:"psp,omitempty"`
	CustomerVaultID string `json:"customer_vault_id"`
	// BillingID targets the exact stored card in the vault (#682); "" charges
	// the vault's priority-1 entry (always correct for one-card-per-vault).
	BillingID    string    `json:"billing_id,omitempty"`
	AmountMicros int64     `json:"amount_micros"`
	Currency     string    `json:"currency"`
	Description  string    `json:"description"`
	UserID       string    `json:"user_id"`
	PriceID      uuid.UUID `json:"price_id"`
	// StoredCredentialRef is the instrument's unscheduled-sequence
	// stored-credential anchor at enqueue (#297). "" = this charge is the
	// sequence's initial CIT (indicator=stored) and finalize captures the
	// returned transaction id as the anchor (write-once).
	StoredCredentialRef string `json:"stored_credential_ref,omitempty"`
	E2ERunID            string `json:"e2e_run_id,omitempty"`
}

// Evidence keys the producer reads back off a succeeded intent.
const (
	nmiSaleEvidenceTransactionID = "transaction_id"
	nmiSaleEvidencePaymentID     = "payment_id"
	nmiSaleEvidenceDelayedStart  = "delayed_start"
)

// nmiSaleIntentOrderID derives the NMI order id from the intent id (#674):
// stable across every execution attempt of the intent, unique per intent.
// 36 chars (+ optional e2e suffix, ≤ 49) fits NMI's 50-char order-id cap.
func nmiSaleIntentOrderID(intentID uuid.UUID, e2eRunID string) string {
	orderID := intentID.String()
	if runID := strings.TrimSpace(e2eRunID); runID != "" {
		orderID = fmt.Sprintf("%s_e2e_%s", orderID, nmiOrderIDSuffix(runID))
	}
	return orderID
}

// NMISaleIntentHandler implements the money-mover semantics for checkout
// sales: attempts > 1 verify-at-provider before sending; transport-ambiguous
// outcomes park as unknown_needs_verify (never treated as declines); parsed
// declines are terminal with the response code as evidence; a charge whose
// local registration failed routes through the verifier until the purchase is
// recorded — never charged-but-unrecorded.
type NMISaleIntentHandler struct {
	Sale   *CheckoutNMISaleService
	Policy intents.BackoffPolicy
}

func NewNMISaleIntentHandler(sale *CheckoutNMISaleService) *NMISaleIntentHandler {
	return &NMISaleIntentHandler{Sale: sale, Policy: intents.DefaultBackoff}
}

func (h *NMISaleIntentHandler) Type() string                         { return TypeNMISale }
func (h *NMISaleIntentHandler) Backoff(attempts int32) time.Duration { return h.Policy.Delay(attempts) }

// PrunePolicy keeps result_evidence on the succeeded tombstone: the checkout
// producer reads payment_id/transaction_id/delayed_start back off the durable
// row (both inline and when answering a replayed request).
func (h *NMISaleIntentHandler) PrunePolicy() (keepPayload, keepEvidence bool) { return false, true }

func decodeNMISalePayload(intent gen.OpenrailsRailIntent) (NMISalePayload, error) {
	var p NMISalePayload
	if len(intent.Payload) == 0 {
		return p, errors.New("nmi sale intent has no payload")
	}
	if err := json.Unmarshal(intent.Payload, &p); err != nil {
		return p, fmt.Errorf("decode nmi sale payload: %w", err)
	}
	if p.CustomerVaultID == "" || p.AmountMicros <= 0 || p.Currency == "" || p.UserID == "" || p.PriceID == uuid.Nil {
		return p, errors.New("nmi sale payload is incomplete")
	}
	return p, nil
}

// CheckRelevance: a checkout sale never goes stale on its own — a decline is
// terminal, success is success. (Expiry, when set, is enforced by the sweep.)
func (h *NMISaleIntentHandler) CheckRelevance(context.Context, gen.OpenrailsRailIntent) (intents.Relevance, error) {
	return intents.StillRelevant(), nil
}

func (h *NMISaleIntentHandler) Execute(ctx context.Context, intent gen.OpenrailsRailIntent) intents.Outcome {
	if h.Sale == nil || h.Sale.PurchaseService == nil {
		return intents.Parked("checkout sale service not wired")
	}
	p, err := decodeNMISalePayload(intent)
	if err != nil {
		return intents.Terminal(err.Error())
	}
	client, err := h.Sale.nmiClient(ctx, nmiIntentClientName(p.PSP, intent.Rail))
	if err != nil {
		return intents.Parked(fmt.Sprintf("nmi client not configured for provider %q: %v", intent.Rail, err))
	}
	if client.ReadOnly {
		return intents.Parked("nmi client is read-only (mode=readonly)")
	}
	orderID := nmiSaleIntentOrderID(intent.ID, p.E2ERunID)

	// Money mover: any re-execution verifies by reading before sending again.
	if intent.Attempts > 1 {
		txnID, found, verr := client.FindSuccessfulSaleByOrderID(orderID)
		if verr != nil {
			return intents.Ambiguous("pre-send verification read failed: " + verr.Error())
		}
		if found {
			return h.finalize(ctx, intent.MerchantID, p, orderID, txnID, true)
		}
	}

	amountCents, err := moneyutil.MicrosToCentsExact(p.AmountMicros)
	if err != nil {
		return intents.Terminal("sale amount must be representable in whole cents: " + err.Error())
	}
	// #297: a checkout sale is a cardholder-initiated unscheduled CoF charge —
	// the sequence's initial CIT when the instrument has no anchor yet, a
	// reuse (indicator=used + initial_transaction_id) when it does.
	citContext := charge.InitialOneTime()
	if p.StoredCredentialRef != "" {
		citContext = charge.OneTimeReuse(p.StoredCredentialRef)
	}
	saleResp, err := client.RunSale(nmi.SaleParams{
		CustomerVaultID:  p.CustomerVaultID,
		BillingID:        p.BillingID,
		Amount:           moneyutil.Cents(amountCents),
		Currency:         p.Currency,
		OrderDescription: p.Description,
		OrderID:          orderID,
		StoredCredential: nmidirect.StoredCredentialFor(citContext),
	})
	if err != nil {
		if errors.Is(err, nmi.ErrProviderReadOnly) {
			return intents.Parked("nmi provider writes blocked (mode=readonly)")
		}
		if nmi.IsTransportAmbiguous(err) {
			// The charge may have landed; the verifier resolves via reads.
			return intents.Ambiguous("sale outcome unknown: " + err.Error())
		}
		var vaultErr *nmi.CustomerVaultError
		if errors.As(err, &vaultErr) {
			return intents.TerminalWithEvidence(vaultErr.Error(), map[string]any{
				"declined":        true,
				"response_code":   vaultErr.ResponseCode,
				"localization_id": vaultErr.LocalizationID,
			})
		}
		// Request-level rejection (validation, config): nothing was charged.
		return intents.Terminal("sale request rejected: " + err.Error())
	}
	return h.finalize(ctx, intent.MerchantID, p, orderID, saleResp.TransactionID, false)
}

// Verify resolves an ambiguous sale via the Query API: a successful sale for
// the intent's order id means the charge landed (finalize registers it); a
// clean read with no sale means no money moved and the executor may resend
// with the SAME order id.
func (h *NMISaleIntentHandler) Verify(ctx context.Context, intent gen.OpenrailsRailIntent) intents.Outcome {
	p, err := decodeNMISalePayload(intent)
	if err != nil {
		return intents.Terminal(err.Error())
	}
	client, err := h.Sale.nmiClient(ctx, nmiIntentClientName(p.PSP, intent.Rail))
	if err != nil {
		return intents.Ambiguous(fmt.Sprintf("nmi client not configured for provider %q; cannot verify", intent.Rail))
	}
	orderID := nmiSaleIntentOrderID(intent.ID, p.E2ERunID)
	txnID, found, err := client.FindSuccessfulSaleByOrderID(orderID)
	if err != nil {
		return intents.Ambiguous("provider read failed: " + err.Error())
	}
	if !found {
		return intents.Retryable("no successful sale found for order id; charge verified not executed")
	}
	return h.finalize(ctx, intent.MerchantID, p, orderID, txnID, true)
}

// finalize registers the confirmed charge locally (payment row, entitlements,
// credits — RegisterPurchase is idempotent on (rail, transaction_id)) and
// returns the evidence the producer builds its response from.
func (h *NMISaleIntentHandler) finalize(ctx context.Context, merchantID uuid.UUID, p NMISalePayload, orderID, transactionID string, verified bool) intents.Outcome {
	// #297: an initial CIT anchors the instrument's unscheduled sequence —
	// persist the returned transaction id write-once. Best-effort: the charge
	// happened and registration must never fail on this.
	if p.StoredCredentialRef == "" && strings.TrimSpace(transactionID) != "" {
		h.captureStoredCredentialRef(ctx, merchantID, p, strings.TrimSpace(transactionID))
	}

	var metadata map[string]any
	if p.E2ERunID != "" {
		metadata = map[string]any{"e2e_run_id": p.E2ERunID, "order_id": orderID}
	}
	result, err := h.Sale.PurchaseService.RegisterPurchase(ctx, &payments.RegisterPurchaseRequest{
		UserID:        p.UserID,
		PriceID:       p.PriceID,
		Rail:          strings.ToLower(p.Provider),
		TransactionID: transactionID,
		Amount:        p.AmountMicros,
		Currency:      p.Currency,
		Metadata:      metadata,
		AttemptKind:   payments.AttemptInitial,
	})
	if err != nil {
		// The charge DID happen; keep resolving through the verifier until the
		// purchase is registered — never charged-but-unrecorded.
		return intents.Ambiguous("sale charged, but purchase registration failed: " + err.Error())
	}
	evidence := map[string]any{
		nmiSaleEvidenceTransactionID: transactionID,
		nmiSaleEvidencePaymentID:     result.PaymentID.String(),
	}
	if result.DelayedStart != nil {
		evidence[nmiSaleEvidenceDelayedStart] = result.DelayedStart.UTC().Format(time.RFC3339)
	}
	if verified {
		evidence["verified_existing"] = true
	}
	return intents.Succeeded(evidence)
}

// captureStoredCredentialRef persists the unscheduled-sequence anchor for the
// charged instrument (#297), keyed by the rail handles the payload carries.
// Best-effort by design: a miss just means the next successful charge
// re-captures (the query is write-once either way).
func (h *NMISaleIntentHandler) captureStoredCredentialRef(ctx context.Context, merchantID uuid.UUID, p NMISalePayload, transactionID string) {
	if h.Sale == nil || h.Sale.VaultService == nil || h.Sale.VaultService.DB == nil {
		log.WithContext(ctx).Warn("nmi sale finalize: no DB handle to persist stored-credential reference (#297)")
		return
	}
	if _, err := h.Sale.VaultService.DB.Gen(ctx).CaptureStoredCredentialRefByRailInstrument(ctx, gen.CaptureStoredCredentialRefByRailInstrumentParams{
		MerchantID:      merchantID,
		Rail:            strings.ToLower(strings.TrimSpace(p.Provider)),
		RailCustomerRef: strings.TrimSpace(p.CustomerVaultID),
		RailMethodRef:   strings.TrimSpace(p.BillingID),
		Agreement:       string(charge.AgreementUnscheduled),
		Ref:             transactionID,
	}); err != nil {
		log.WithContext(ctx).WithError(err).WithField("customer_vault_id", p.CustomerVaultID).
			Warn("nmi sale finalize: failed to persist stored-credential reference (#297); next charge re-captures")
	}
}
