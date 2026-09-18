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
// resolve from provider reads. Exactly one of ProviderReference or NotExecuted
// is set. A reference is accepted only after the handler reads that exact
// provider object and matches it to the frozen operation; NotExecuted records
// provider-confirmed non-execution. Neither ever authorizes another send of
// the unresolved mutation.
type Resolution struct {
	// Step names the provider step of a multi-step operation (e.g. an
	// upgrade's "successor" or "proration"); empty for single-step types.
	Step              string
	ProviderReference string
	NotExecuted       bool
	Actor             string
	Reason            string
}

var (
	ErrResolutionInvalid     = errors.New("invalid operator resolution")
	ErrResolutionUnsupported = errors.New("operation type has no operator resolution")
	ErrResolutionNotUnknown  = errors.New("only an unknown_needs_verify operation can be resolved")
	ErrResolutionBusy        = errors.New("operation is leased by another worker")
	ErrResolutionRejected    = errors.New("provider evidence does not establish this operation's outcome")
)

// OperatorResolver is implemented by handlers whose unknown operations accept
// operator evidence. A returned error rejects the evidence and leaves the
// operation unchanged; an Outcome is applied exactly like a verifier result.
type OperatorResolver interface {
	Resolve(ctx context.Context, intent gen.OpenrailsRailIntent, resolution Resolution) (Outcome, error)
}

func (r Resolution) normalized() (Resolution, error) {
	r.Step = strings.TrimSpace(r.Step)
	r.ProviderReference = strings.TrimSpace(r.ProviderReference)
	r.Actor = strings.TrimSpace(r.Actor)
	r.Reason = strings.TrimSpace(r.Reason)
	switch {
	case r.Actor == "":
		return r, fmt.Errorf("%w: actor is required", ErrResolutionInvalid)
	case r.Reason == "":
		return r, fmt.Errorf("%w: reason is required", ErrResolutionInvalid)
	case (r.ProviderReference == "") == !r.NotExecuted:
		return r, fmt.Errorf("%w: supply exactly one of a provider reference or not-executed", ErrResolutionInvalid)
	}
	return r, nil
}

// Record is the durable audit form stored with the operation and mutation log.
func (r Resolution) Record(at time.Time) map[string]any {
	out := map[string]any{"actor": r.Actor, "reason": r.Reason, "resolved_at": at.UTC().Format(time.RFC3339Nano)}
	if r.Step != "" {
		out["step"] = r.Step
	}
	if r.NotExecuted {
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
	if row.Status != StatusUnknownNeedsVerify {
		return row, fmt.Errorf("%w (status=%s)", ErrResolutionNotUnknown, row.Status)
	}
	resolver, ok := r.Registry.Lookup(row.IntentType).(OperatorResolver)
	if !ok {
		return row, fmt.Errorf("%w: %s", ErrResolutionUnsupported, row.IntentType)
	}
	now := r.now()
	claimed, ok, err := r.Store.ClaimUnknownByID(ctx, id, now, now.Add(r.lease()))
	if err != nil {
		return row, err
	}
	if !ok {
		return row, ErrResolutionBusy
	}
	ctx = pinIntentAddress(ctx, claimed)
	logEntry := log.WithContext(ctx).WithFields(log.Fields{
		"intent_id": claimed.ID, "intent_type": claimed.IntentType, "provider": claimed.Rail, "actor": resolution.Actor,
	})
	stopBeat := r.renewClaimWhile(ctx, logEntry, claimed.ID)
	outcome, rerr := resolver.Resolve(ctx, claimed, resolution)
	stopBeat()
	if rerr != nil {
		releaseCtx, cancel := LedgerWriteContext(ctx)
		defer cancel()
		if _, err := r.Store.ReleaseUnknownClaim(releaseCtx, claimed.ID); err != nil {
			logEntry.WithError(err).Error("operator resolution: lease release failed; lease expiry will re-surface the operation")
		}
		return claimed, rerr
	}
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
	return r.Store.Get(ctx, id)
}
