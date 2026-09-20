package intents

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Resolution is operator evidence for an operation the engine could not
// resolve from provider reads. Exactly one of ProviderReference, NotExecuted
// BillingAnchor or Abandon is set. BillingAnchor is accepted only by the NMI cutover
// anchor step and authorizes a later first charge on its verified paused target.
// A reference is accepted only after the handler reads that exact
// provider object and matches it to the frozen operation; NotExecuted records
// provider-confirmed non-execution. Those evidence forms never authorize another
// send of the unresolved mutation. Anchor authorization does not perform a send;
// the executor applies the new first-charge date under its normal write gates.
type Resolution struct {
	// Step names the provider step of a multi-step operation (e.g. an
	// upgrade's "successor" or "proration"); empty for single-step types.
	Step              string
	ProviderReference string
	NotExecuted       bool
	Abandon           bool
	BillingAnchor     time.Time
	Actor             string
	Reason            string
}

var (
	ErrResolutionInvalid     = errors.New("invalid operator resolution")
	ErrResolutionUnsupported = errors.New("operation type has no operator resolution")
	ErrResolutionNotUnknown  = errors.New("only an unknown_needs_verify operation can be resolved (a never-submitted operation of a type that supports it accepts --not-executed)")
	ErrResolutionBusy        = errors.New("operation is leased by another worker")
	ErrResolutionRejected    = errors.New("provider evidence does not establish this operation's outcome")
)

// OperatorResolver is implemented by handlers whose unknown operations accept
// operator evidence. A returned error rejects the evidence and leaves the
// operation unchanged; an Outcome is applied exactly like a verifier result.
type OperatorResolver interface {
	Resolve(ctx context.Context, intent gen.OpenrailsRailIntent, resolution Resolution) (Outcome, error)
}

// UnsentResolver is implemented by handlers whose pending or retryable
// operations an operator may release with NotExecuted when the operation's
// own write-ahead fence proves nothing was ever submitted (a parked-forever
// operation holding a local claim). The handler must reject any operation
// carrying a submission fence; only its verifier may close those.
type UnsentResolver interface {
	ResolveUnsent(ctx context.Context, intent gen.OpenrailsRailIntent, resolution Resolution) (Outcome, error)
}

func (r Resolution) normalized() (Resolution, error) {
	r.Step = strings.TrimSpace(r.Step)
	r.ProviderReference = strings.TrimSpace(r.ProviderReference)
	r.Actor = strings.TrimSpace(r.Actor)
	r.Reason = strings.TrimSpace(r.Reason)
	choices := 0
	if r.ProviderReference != "" {
		choices++
	}
	if r.NotExecuted {
		choices++
	}
	if r.Abandon {
		choices++
	}
	if !r.BillingAnchor.IsZero() {
		choices++
		r.BillingAnchor = r.BillingAnchor.UTC()
	}
	switch {
	case r.Actor == "":
		return r, fmt.Errorf("%w: actor is required", ErrResolutionInvalid)
	case r.Reason == "":
		return r, fmt.Errorf("%w: reason is required", ErrResolutionInvalid)
	case choices != 1:
		return r, fmt.Errorf("%w: supply exactly one of a provider reference, not-executed billing anchor or abandon", ErrResolutionInvalid)
	}
	return r, nil
}

// Record is the durable audit form stored with the operation and mutation log.
func (r Resolution) Record(at time.Time) map[string]any {
	out := map[string]any{"actor": r.Actor, "reason": r.Reason, "resolved_at": at.UTC().Format(time.RFC3339Nano)}
	if r.Step != "" {
		out["step"] = r.Step
	}
	if r.Abandon {
		out["abandon"] = true
	} else if !r.BillingAnchor.IsZero() {
		out["billing_anchor"] = r.BillingAnchor.Format(time.RFC3339)
	} else if r.NotExecuted {
		out["not_executed"] = true
	} else {
		out["provider_reference"] = r.ProviderReference
	}
	return out
}

// RejectResolution wraps a handler's reason for refusing operator evidence.
func RejectResolution(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrResolutionRejected, fmt.Sprintf(format, args...))
}

// Resolve applies operator evidence to one unknown operation in the caller's
// merchant scope. The handler validates the evidence and derives the outcome;
// local effects then commit through the same paths a provider receipt uses.
func (r *Runner) Resolve(ctx context.Context, id uuid.UUID, resolution Resolution) (gen.OpenrailsRailIntent, error) {
	resolution, err := resolution.normalized()
	if err != nil {
		return gen.OpenrailsRailIntent{}, err
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return gen.OpenrailsRailIntent{}, err
	}
	row, err := r.Store.Get(ctx, id)
	if err != nil {
		return gen.OpenrailsRailIntent{}, err
	}
	if row.MerchantID != mid.UUID() {
		return gen.OpenrailsRailIntent{}, fmt.Errorf("%w: operation belongs to another merchant", ErrResolutionInvalid)
	}
	if !resolution.BillingAnchor.IsZero() {
		if row.IntentType != TypeNMIProviderCutover || resolution.Step != "anchor" {
			return row, ErrResolutionUnsupported
		}
		if cutoverAnchorResolutionMatches(row, resolution) {
			return row, nil
		}
	}
	if resolution.Abandon {
		if row.IntentType != TypeNMIProviderCutover || resolution.Step != "target" {
			return row, ErrResolutionUnsupported
		}
		_, progress, err := decodeCutover(row)
		if err != nil {
			return row, err
		}
		if progress.Decision != nil && progress.Decision.Action == "abandon" {
			return row, nil // The original approval is immutable and already recorded.
		}
	}
	pending := row.Status == StatusPending || row.Status == StatusFailedRetryable
	if pending && !resolution.Abandon {
		return r.resolveUnsent(ctx, row, resolution)
	}
	if row.Status != StatusUnknownNeedsVerify && !(pending && resolution.Abandon) {
		return row, fmt.Errorf("%w (status=%s)", ErrResolutionNotUnknown, row.Status)
	}
	resolver, ok := r.Registry.Lookup(row.IntentType).(OperatorResolver)
	if !ok {
		return row, fmt.Errorf("%w: %s", ErrResolutionUnsupported, row.IntentType)
	}
	now := r.now()
	var claimed gen.OpenrailsRailIntent
	if pending {
		claimed, ok, err = r.Store.ClaimByID(ctx, id, now, now.Add(r.lease()))
	} else {
		claimed, ok, err = r.Store.ClaimUnknownByID(ctx, id, now, now.Add(r.lease()))
	}
	if err != nil {
		return row, err
	}
	if !ok {
		return row, ErrResolutionBusy
	}
	ctx = pinIntentAddress(ctx, claimed)
	logEntry := r.resolutionLog(ctx, claimed, resolution)
	stopBeat := r.renewClaimWhile(ctx, logEntry, claimed.ID)
	outcome, rerr := resolver.Resolve(ctx, claimed, resolution)
	stopBeat()
	if rerr != nil {
		releaseCtx, cancel := LedgerWriteContext(ctx)
		defer cancel()
		if pending {
			if err := r.Store.Park(releaseCtx, claimed.ID, now.Add(ParkRetryInterval), "operator abandonment rejected"); err != nil {
				logEntry.WithError(err).Error("operator resolution: release failed")
			}
		} else if _, err := r.Store.ReleaseUnknownClaim(releaseCtx, claimed.ID); err != nil {
			logEntry.WithError(err).Error("operator resolution: lease release failed; lease expiry will re-surface the operation")
		}
		return claimed, rerr
	}
	return r.applyResolution(ctx, logEntry, claimed, resolution, outcome, now)
}

// resolveUnsent releases a never-submitted operation on operator NotExecuted
// evidence. The lease is the executor's; a rejection parks the operation back
// exactly as it was.
func (r *Runner) resolveUnsent(ctx context.Context, row gen.OpenrailsRailIntent, resolution Resolution) (gen.OpenrailsRailIntent, error) {
	resolver, ok := r.Registry.Lookup(row.IntentType).(UnsentResolver)
	if !ok || !resolution.NotExecuted {
		return row, fmt.Errorf("%w (status=%s)", ErrResolutionNotUnknown, row.Status)
	}
	now := r.now()
	claimed, ok, err := r.Store.ClaimByID(ctx, row.ID, now, now.Add(r.lease()))
	if err != nil {
		return row, err
	}
	if !ok {
		return row, ErrResolutionBusy
	}
	ctx = pinIntentAddress(ctx, claimed)
	logEntry := r.resolutionLog(ctx, claimed, resolution)
	stopBeat := r.renewClaimWhile(ctx, logEntry, claimed.ID)
	outcome, rerr := resolver.ResolveUnsent(ctx, claimed, resolution)
	stopBeat()
	if rerr != nil {
		if err := r.Store.Park(ctx, claimed.ID, now.Add(ParkRetryInterval), "operator release rejected: "+rerr.Error()); err != nil {
			logEntry.WithError(err).Error("operator release: park failed; lease expiry will re-surface the operation")
		}
		return claimed, rerr
	}
	return r.applyResolution(ctx, logEntry, claimed, resolution, outcome, now)
}

func (r *Runner) resolutionLog(ctx context.Context, claimed gen.OpenrailsRailIntent, resolution Resolution) *log.Entry {
	return log.WithContext(ctx).WithFields(log.Fields{
		"intent_id": claimed.ID, "intent_type": claimed.IntentType, "provider": claimed.Rail, "actor": resolution.Actor,
	})
}

func (r *Runner) applyResolution(ctx context.Context, logEntry *log.Entry, claimed gen.OpenrailsRailIntent, resolution Resolution, outcome Outcome, now time.Time) (gen.OpenrailsRailIntent, error) {
	record := resolution.Record(now)
	evidence := map[string]any{"operator_resolution": record}
	for k, v := range outcome.Evidence {
		evidence[k] = v
	}
	outcome.Evidence = evidence
	reason := "operator resolution: " + resolution.Reason
	if outcome.Reason != "" {
		reason += "; " + outcome.Reason
	}
	logEntry.WithField("outcome", outcome.Class.String()).Warn("operator resolution applied")
	var stats Stats
	r.record(ctx, logEntry, &stats, r.Registry.Lookup(claimed.IntentType), claimed, outcome, reason, true)
	return r.Store.Get(ctx, claimed.ID)
}
