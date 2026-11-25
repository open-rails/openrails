package intents

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/controls"
	"github.com/open-rails/openrails/internal/db/gen"
)

// RebillHold is why a charge waits under its merchant's rebill brake: one
// OpenRails initiates (system origin) waits while the merchant's rebills are
// held; one its customer started never does.
func RebillHold(ctx context.Context, view controls.View, in gen.BillingProviderIntent) string {
	if Origin(in.Origin) != OriginSystem {
		return ""
	}
	return view.RebillHold(ctx, in.MerchantID)
}

// ModeView is a merchant's effective writes: its stored control, readonly in
// a copy of the billing book. controls.View satisfies it.
type ModeView = controls.Reader

// GateExecution decides whether an intent of the given origin may attempt a
// provider write for merchantID under its posture. blocked=true parks the
// intent with the returned reason; posture is never an error.
//
//	            full      limited   readonly
//	user        execute   execute   park
//	admin       execute   execute   park
//	provider    execute   execute   park
//	system      execute   park      park
//
// user/admin intents are reactive completions of a human request (a cancel's
// deferred delete, an admin refund); system intents (dunning charges,
// proactive deletes) need full. The wire chokes are the readonly backstop.
func GateExecution(ctx context.Context, mode ModeView, merchantID uuid.UUID, origin Origin) (blocked bool, reason string) {
	// A missing ModeView means we cannot tell which posture we are in. That is
	// a wiring bug, and a fail-closed gate must never answer "go ahead" when it
	// does not know. Parking is free (the intent is durable); executing is not.
	if mode == nil {
		return true, "write posture is unknown (none wired); refusing to attempt a provider write"
	}
	switch origin {
	case OriginUser, OriginAdmin, OriginSystem, OriginProvider:
	default:
		// An unknown origin cannot be gated correctly; park rather than guess.
		return true, fmt.Sprintf("unknown intent origin %q", origin)
	}
	p := mode.Posture(ctx, merchantID)
	if p.ReadOnly() || p.Limited() && origin == OriginSystem {
		return true, "write posture " + p.Mode.String() + ": " + p.Reason
	}
	return false, ""
}
