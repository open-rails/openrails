package intents

import "fmt"

// ModeView is the minimal operating-mode surface the executor's gate needs
// (#346). *config.Config satisfies it.
type ModeView interface {
	// IsProviderReadOnly reports mode=readonly: EVERY provider write is
	// blocked, even reactive user-initiated ones.
	IsProviderReadOnly() bool
	// IsLimitedMode reports mode=limited OR readonly: proactive
	// (system-origin) provider operations are blocked.
	IsLimitedMode() bool
}

// GateExecution decides whether an intent of the given origin may attempt a
// provider write under the current operating mode. blocked=true parks the
// intent with the returned reason — mode is a reason an intent stays pending,
// never an error.
//
// The matrix (origin x mode):
//
//	            full      limited   readonly
//	user        execute   execute   park
//	admin       execute   execute   park
//	system      execute   park      park
//
// user/admin-origin intents are reactive completions of something a human
// asked for (their cancel's deferred delete, an admin refund) and execute
// under limited — matching the pre-ledger deferred-delete behavior.
// system-origin intents (dunning charges, proactive deletes) require full.
// Nothing attempts a provider write under readonly: the wire chokes are the
// backstop, the executor checks first and parks politely.
func GateExecution(cfg ModeView, origin Origin) (blocked bool, reason string) {
	// A missing ModeView means we cannot tell which mode we are in. That is a
	// wiring bug, and the ONE thing a fail-closed gate must never do is answer
	// "go ahead" when it does not know. Parking is free (the intent is durable
	// and re-executes once the runner is wired); executing is not.
	if cfg == nil {
		return true, "operating mode is unknown (no mode view wired); refusing to attempt a provider write"
	}
	if cfg.IsProviderReadOnly() {
		return true, "mode=readonly blocks all provider writes"
	}
	if cfg.IsLimitedMode() && origin == OriginSystem {
		return true, "mode=limited blocks proactive (system-origin) provider writes"
	}
	switch origin {
	case OriginUser, OriginAdmin, OriginSystem:
		return false, ""
	default:
		// An unknown origin cannot be gated correctly; park rather than guess.
		return true, fmt.Sprintf("unknown intent origin %q", origin)
	}
}
