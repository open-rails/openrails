package intents

import (
	"context"
	"fmt"

	"github.com/open-rails/openrails/internal/db/gen"
)

// Resolve accepts only provider-confirmed non-execution for a rebill. A
// successful rebill is correlated exactly by the period's order reference,
// which the verifier already reads; an operator-supplied transaction cannot
// add stronger evidence without a frozen amount.
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
	if !resolution.NotExecuted {
		return Outcome{}, RejectResolution("rebill receipts converge through the exact order-reference search, not an operator reference")
	}
	client, err := h.railClient(ctx, intent)
	if err != nil {
		return Outcome{}, err
	}
	txn, found, err := h.findSuccessfulSale(ctx, client, p)
	if err != nil {
		return Outcome{}, fmt.Errorf("read provider order before accepting non-execution: %w", err)
	}
	if found {
		return Outcome{}, RejectResolution("provider shows successful sale %s for order %s", txn, p.OrderReference)
	}
	return TerminalWithEvidence("provider confirmed the rebill was not executed", map[string]any{"declined": false}), nil
}
