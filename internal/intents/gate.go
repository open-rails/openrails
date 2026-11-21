package intents

import (
	"context"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/writeposture"
)

// ModeView is a merchant's effective write posture: the lower of the
// process's provider_write_mode and the merchant's stored posture, readonly in
// a copy of the billing book. writeposture.View satisfies it.
type ModeView interface {
	Posture(ctx context.Context, merchantID uuid.UUID) writeposture.Posture
	Provider(ctx context.Context, merchantID, pspID uuid.UUID) writeposture.Posture
}

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
		return true, "operating mode is unknown (no mode view wired); refusing to attempt a provider write"
	}
	var p writeposture.Posture
	if pspID := db.PSPIDFromContext(ctx); pspID != uuid.Nil {
		p = mode.Provider(ctx, merchantID, pspID)
	} else {
		p = mode.Posture(ctx, merchantID)
	}
	reason = p.HoldReason(origin)
	return reason != "", reason
}
