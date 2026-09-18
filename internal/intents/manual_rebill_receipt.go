package intents

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/integrations/nmi"
)

const (
	rebillCandidateKey = "candidate_transaction_id"
	rebillReceiptKey   = "qualified_rebill_receipt"
)

// manualRebillReceipt can authorize local effects only after exact NMI
// qualification or validated reload of that persisted qualification. Neither
// a classic approval nor a bare provider transaction ID constructs one.
type manualRebillReceipt struct {
	transactionID string
	binding       string
}

type storedRebillReceipt struct {
	TransactionID string `json:"transaction_id"`
	Binding       string `json:"binding"`
}

// Bind the receipt to the operation, provider account and complete frozen
// request. Reload never borrows terms or instrument coordinates from live rows.
func rebillReceiptBinding(intent gen.OpenrailsRailIntent, p ManualRebillPayload, transactionID string) string {
	b, _ := json.Marshal(struct {
		Operation, Kind, Merchant, Provider, Rail, Transaction string
		Payload                                                ManualRebillPayload
	}{intent.ID.String(), intent.IntentType, intent.MerchantID.String(), fmt.Sprint(intent.PspID), intent.Rail, transactionID, p})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (r manualRebillReceipt) matches(intent gen.OpenrailsRailIntent, p ManualRebillPayload) error {
	if intent.ID == uuid.Nil || intent.MerchantID == uuid.Nil || intent.PspID == nil || *intent.PspID == uuid.Nil ||
		intent.IntentType != TypeManualRebill || intent.SubscriptionID == nil || *intent.SubscriptionID != p.SubscriptionID ||
		p.Instrument.PSPID != *intent.PspID || intent.Rail == "" || intent.Rail != p.Rail || r.transactionID == "" || r.binding == "" ||
		r.binding != rebillReceiptBinding(intent, p, r.transactionID) {
		return errors.New("qualified rebill receipt does not bind this operation and its frozen terms")
	}
	return nil
}

func loadRebillReceipt(intent gen.OpenrailsRailIntent, p ManualRebillPayload) (manualRebillReceipt, bool, error) {
	var evidence map[string]json.RawMessage
	if len(intent.ResultEvidence) != 0 {
		if err := json.Unmarshal(intent.ResultEvidence, &evidence); err != nil {
			return manualRebillReceipt{}, false, err
		}
	}
	raw, found := evidence[rebillReceiptKey]
	if !found {
		return manualRebillReceipt{}, false, nil
	}
	var saved storedRebillReceipt
	if err := json.Unmarshal(raw, &saved); err != nil {
		return manualRebillReceipt{}, true, fmt.Errorf("decode qualified rebill receipt: %w", err)
	}
	r := manualRebillReceipt{transactionID: strings.TrimSpace(saved.TransactionID), binding: saved.Binding}
	return r, true, r.matches(intent, p)
}

func (h *ManualRebillHandler) saveRebillCandidate(ctx context.Context, intent gen.OpenrailsRailIntent, transactionID string) error {
	transactionID = strings.TrimSpace(transactionID)
	if transactionID == "" {
		return errors.New("rebill approval has no candidate transaction ID")
	}
	writeCtx, cancel := LedgerWriteContext(ctx)
	defer cancel()
	store := NewStore(h.DB)
	inserted, err := store.RecordProgressIfAbsent(writeCtx, intent.ID, rebillCandidateKey, transactionID)
	if err != nil || inserted {
		return err
	}
	current, err := store.Get(writeCtx, intent.ID)
	if err != nil {
		return err
	}
	if EvidenceString(current, rebillCandidateKey) != transactionID {
		return errors.New("operation did not retain the same rebill candidate")
	}
	return nil
}

// saveRebillReceipt takes custody before any local financial effects. The
// insert-once key cannot replace an existing receipt with contradictory facts;
// a failed write/reload keeps the operation unresolved, without resubmission.
func (h *ManualRebillHandler) saveRebillReceipt(ctx context.Context, intent gen.OpenrailsRailIntent, p ManualRebillPayload, receipt manualRebillReceipt) error {
	if err := receipt.matches(intent, p); err != nil {
		return err
	}
	writeCtx, cancel := LedgerWriteContext(ctx)
	defer cancel()
	store := NewStore(h.DB)
	inserted, err := store.RecordProgressIfAbsent(writeCtx, intent.ID, rebillReceiptKey, storedRebillReceipt{receipt.transactionID, receipt.binding})
	if err != nil || inserted {
		return err
	}
	current, err := store.Get(writeCtx, intent.ID)
	if err != nil {
		return err
	}
	saved, found, err := loadRebillReceipt(current, p)
	if err != nil {
		return err
	}
	if !found || saved != receipt {
		return errors.New("operation did not retain the same qualified rebill receipt")
	}
	return nil
}

func (h *ManualRebillHandler) confirmAndFinalizeRebill(ctx context.Context, intent gen.OpenrailsRailIntent, p ManualRebillPayload, client *nmi.NMIClient, candidate string) Outcome {
	transactionID, found, err := client.ConfirmOrderSale(ctx, p.Receipt(), candidate)
	if errors.Is(err, nmi.ErrReceiptMismatch) {
		return rebillContradicted(err)
	}
	if err != nil {
		return Ambiguous("provider read failed: " + err.Error())
	}
	if !found {
		return Ambiguous("submitted rebill has no exact provider receipt; no automatic resend")
	}
	receipt := manualRebillReceipt{transactionID: transactionID, binding: rebillReceiptBinding(intent, p, transactionID)}
	if err := h.saveRebillReceipt(ctx, intent, p, receipt); err != nil {
		return Ambiguous("qualified rebill receipt custody failed: " + err.Error())
	}
	return h.completeRebill(ctx, intent, p, receipt)
}

func (h *ManualRebillHandler) completeRebill(ctx context.Context, intent gen.OpenrailsRailIntent, p ManualRebillPayload, receipt manualRebillReceipt) Outcome {
	evidence := map[string]any{
		"transaction_id": receipt.transactionID,
		rebillReceiptKey: storedRebillReceipt{receipt.transactionID, receipt.binding},
	}
	if err := h.finalizeSuccess(ctx, intent, p, receipt); err != nil {
		return AmbiguousWithEvidence("rebill receipt retained, but local lifecycle repair failed: "+err.Error(), evidence)
	}
	return Succeeded(evidence)
}
