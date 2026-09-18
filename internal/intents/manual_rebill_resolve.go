package intents

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/integrations/nmi"
)

// Resolve applies operator evidence to an unresolved rebill through the SAME
// exact-receipt path the verifier uses (nmi.ConfirmOrderSale against the
// frozen charge): a named receipt settles only when it is the order
// reference's sale, approved, on the frozen vault, for the frozen amount and
// currency; provider-confirmed non-execution is accepted only while the
// provider shows no sale under the order reference at all. It never re-sends.
func (h *ManualRebillHandler) Resolve(ctx context.Context, intent gen.OpenrailsRailIntent, resolution Resolution) (Outcome, error) {
	p, err := decodeManualRebillPayload(intent)
	if err != nil {
		return Outcome{}, err
	}
	if resolution.Step != "" {
		return Outcome{}, fmt.Errorf("%w: a rebill has no steps", ErrResolutionInvalid)
	}
	if txn := EvidenceString(intent, "transaction_id"); txn != "" {
		return Outcome{}, RejectResolution("operation already holds receipt %s; its verifier completes renewal", txn)
	}
	client, err := h.railClient(ctx, intent)
	if err != nil {
		return Outcome{}, err
	}
	if resolution.NotExecuted {
		if contradiction := EvidenceString(intent, rebillEvidenceContradiction); contradiction != "" {
			return Outcome{}, RejectResolution("provider evidence contradicts this operation (%s); non-execution cannot be attested, repair from the provider record", contradiction)
		}
		txn, found, err := client.ConfirmOrderSale(ctx, p.Receipt(), "")
		if errors.Is(err, nmi.ErrReceiptMismatch) {
			return Outcome{}, RejectResolution("provider shows a sale for order %s that contradicts the operation: %v", p.OrderReference, err)
		}
		if err != nil {
			return Outcome{}, fmt.Errorf("read provider order before accepting non-execution: %w", err)
		}
		if found {
			return Outcome{}, RejectResolution("provider shows successful sale %s for order %s", txn, p.OrderReference)
		}
		return TerminalWithEvidence("provider confirmed the rebill was not executed", map[string]any{"declined": false}), nil
	}
	reference := strings.TrimSpace(resolution.ProviderReference)
	if reference == "" {
		return Outcome{}, fmt.Errorf("%w: a receipt or --not-executed is required", ErrResolutionInvalid)
	}
	txn, found, err := client.ConfirmOrderSale(ctx, p.Receipt(), reference)
	if err != nil {
		return Outcome{}, RejectResolution("%v", err)
	}
	if !found {
		return Outcome{}, RejectResolution("provider object %s is not a settled sale for order %s", reference, p.OrderReference)
	}
	if err := h.finalizeSuccess(ctx, intent.MerchantID, p, txn); err != nil {
		return AmbiguousWithEvidence("receipt confirmed at provider, but local lifecycle repair failed: "+err.Error(), map[string]any{"transaction_id": txn}), nil
	}
	return Succeeded(map[string]any{"transaction_id": txn, "verified_existing": true}), nil
}
