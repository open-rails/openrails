package intents

import (
	"context"
	"fmt"
	"github.com/open-rails/openrails/internal/db/gen"
)

func (h *ManualRebillHandler) Resolve(ctx context.Context, in gen.OpenrailsRailIntent, resolution Resolution) (Outcome, error) {
	ctx = pinIntentAddress(ctx, in)
	p, err := DecodeManualRebillPayload(in)
	if err != nil {
		return Outcome{}, err
	}
	if resolution.Step != "" {
		return Outcome{}, fmt.Errorf("%w: rebill resolution has no external step selector", ErrResolutionInvalid)
	}
	if _, found, err := LoadCollectedReceipt(in); err != nil {
		return Outcome{}, RejectResolution("retained receipt is invalid: %v", err)
	} else if found {
		return Outcome{}, RejectResolution("collected rebill must complete from retained receipt")
	}
	if resolution.NotExecuted {
		if EvidenceString(in, rebillSubmittedAt) != "" {
			return Outcome{}, RejectResolution("NMI absence cannot prove a submitted rebill did not execute")
		}
		return h.finalizeNotExecuted(ctx, in, p, "operator released unsubmitted rebill"), nil
	}
	reference := resolution.ProviderReference
	receipt, found, err := ReadNMICollectionReceipt(ctx, in, h.Resolver, reference)
	if err != nil {
		return Outcome{}, RejectResolution("rebill receipt did not qualify: %v", err)
	}
	if !found {
		return Outcome{}, RejectResolution("provider reference is not this accepted rebill")
	}
	return h.finalizeSuccess(ctx, in, p, receipt), nil
}
