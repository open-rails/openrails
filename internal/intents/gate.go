package intents

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/writeposture"
)

// ModeView is a merchant's effective write posture: the lower of the
// process's provider_write_mode and the merchant's stored posture, readonly in
// a copy of the billing book. writeposture.View satisfies it.
type ModeView interface {
	Posture(ctx context.Context, merchantID uuid.UUID) writeposture.Posture
}

// GateExecution decides whether an intent of the given origin may attempt a
// provider write for merchantID under its posture. blocked=true parks the
// intent with the returned reason — posture is a reason an intent stays
// pending, never an error.
//
// The matrix (origin x posture):
//
//	            full      limited   readonly
//	user        execute   execute   park
//	admin       execute   execute   park
//	system      execute   park      park
//
// user/admin-origin intents are reactive completions of something a human
// asked for (their cancel's deferred delete, an admin refund) and execute
// under limited. system-origin intents (dunning charges, proactive deletes)
// require full. Nothing attempts a provider write under readonly: the wire
// chokes are the backstop, the executor checks first and parks politely.
func GateExecution(ctx context.Context, mode ModeView, merchantID uuid.UUID, origin Origin) (blocked bool, reason string) {
	// A missing ModeView means we cannot tell which posture we are in. That is
	// a wiring bug, and a fail-closed gate must never answer "go ahead" when it
	// does not know. Parking is free (the intent is durable); executing is not.
	if mode == nil {
		return true, "operating mode is unknown (no mode view wired); refusing to attempt a provider write"
	}
	switch origin {
	case OriginUser, OriginAdmin, OriginSystem:
	default:
		// An unknown origin cannot be gated correctly; park rather than guess.
		return true, fmt.Sprintf("unknown intent origin %q", origin)
	}
	p := mode.Posture(ctx, merchantID)
	if p.ReadOnly() {
		return true, p.Reason
	}
	if p.Limited() && origin == OriginSystem {
		return true, p.Reason
	}
	return false, ""
}
