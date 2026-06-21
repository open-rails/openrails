// Package spendgate is the Redis-backed admission primitive for openrails #513:
// ONE atomic Lua gate that checks a payer's affordability (settled balance minus
// the shared in-flight hold gauge) and every applicable spend-cap window, places
// a per-request hold, and frees it at capture/release — replacing the Postgres
// budget_window_state / budget_inflight_holds locked-tx path AND the separate
// Redis hold store.
//
// MODEL (decided 2026-06-17, Paul):
//   - Affordability uses the #512 ledger account balance passed in by the caller;
//     only the `held` reservation gauge and the window counters live in shared
//     Redis. A stale/concurrent read can cause bounded over-admission, never
//     ledger corruption (the durable truth is the #512 ledger).
//   - Spend-cap windows are PER-USER-STAGGERED (#337): boundaries tick at
//     offset + k*Duration forever, where offset is a deterministic phase derived
//     from the customer-scoped window key (no stored anchor). Resets are staggered
//     per payer so demand spreads instead of every payer resetting at once. There
//     is ONE window model — no per-window cadence choice to configure.
//   - Windows are ESTIMATE-BASED: a reserve counts the ESTIMATE; capture does NOT
//     true the window up to actual (only the balance, caller-side, trues up).
//     Release (failure) frees the estimate from the windows. Caps therefore run
//     slightly conservative and self-heal at each window reset.
//
// Policy is read-mostly config (cached in memory, invalidated on policy_version
// bump); the Lua scripts + client live in gate.go.
package spendgate

import (
	"fmt"
	"strings"
	"time"
)

// Scope is whose spend a window caps.
type Scope string

const (
	ScopePayer   Scope = "payer"   // the customer overall
	ScopeInvoker Scope = "invoker" // a specific API key / principal
	ScopeRole    Scope = "role"    // a role the invoker holds
	ScopeTier    Scope = "tier"    // the payer's trust tier
)

// Window is one {scope, duration, limit} cap. Limit and all reserved amounts are
// in the currency's minor units.
type Window struct {
	Scope    Scope         `json:"scope"`
	Duration time.Duration `json:"duration"`
	Limit    int64         `json:"limit"`
	// Key is a stable per-policy window identifier (e.g. "5h", "7d") so a window's
	// Redis counter survives across reserves. Distinct windows under one scope MUST
	// have distinct keys.
	Key string `json:"key"`
}

// ScopedWindows is the cap config for one concrete scope identity (e.g. invoker
// "svc:abc", or tier "gold"). ScopeID is "" for the payer-wide scope.
type ScopedWindows struct {
	Scope   Scope    `json:"scope"`
	ScopeID string   `json:"scope_id"`
	Windows []Window `json:"windows"`
}

// Policy is the full cached cap config for one payer+currency (all scopes). It is
// read-mostly config loaded from Postgres and cached in memory, invalidated on the
// existing policy_version bump — never read from Postgres on the hot path.
type Policy struct {
	Scopes []ScopedWindows `json:"scopes"`
}

// Request names the principals a request runs under, for scope matching.
type Request struct {
	Invoker string
	Roles   []string
	Tier    string
}

// resolvedWindow binds a Window to the concrete scope identity it was configured
// for, so it maps to a stable Redis counter key.
type resolvedWindow struct {
	Window
	scopeID string
}

// EffectiveWindows returns every window that applies to req under collect-all
// semantics: all payer-scope windows, invoker-scope windows whose ScopeID matches
// req.Invoker, role-scope windows for any of req.Roles, and tier-scope windows for
// req.Tier. The gate DENIES if ANY returned window is over its limit.
func (p Policy) EffectiveWindows(req Request) []resolvedWindow {
	roles := make(map[string]bool, len(req.Roles))
	for _, r := range req.Roles {
		roles[r] = true
	}
	var out []resolvedWindow
	for _, sw := range p.Scopes {
		match := false
		switch sw.Scope {
		case ScopePayer:
			match = true
		case ScopeInvoker:
			match = req.Invoker != "" && sw.ScopeID == req.Invoker
		case ScopeRole:
			match = roles[sw.ScopeID]
		case ScopeTier:
			match = req.Tier != "" && sw.ScopeID == req.Tier
		}
		if !match {
			continue
		}
		for _, w := range sw.Windows {
			out = append(out, resolvedWindow{Window: w, scopeID: sw.ScopeID})
		}
	}
	return out
}

// identity is the stable Redis key prefix for this window under a payer base. The
// Lua appends ":<bucket>" where bucket = floor((now-offset)/Duration) and offset is
// the deterministic phase fixedOffsetMs(prefix, Duration) — no stored anchor. The
// base is hash-tagged ({merchant:customer}) so every key the Lua touches co-locates
// on one Cluster slot.
func (w resolvedWindow) identity(base string) string {
	return fmt.Sprintf("%s:w:%s:%s:%s", base, w.Scope, scrub(w.scopeID), scrub(w.Key))
}

// durationMillis is the window length passed to the Lua.
func (w resolvedWindow) durationMillis() int64 {
	ms := w.Duration.Milliseconds()
	if ms <= 0 {
		ms = 1000
	}
	return ms
}

// scrub replaces the key separator so a scope id / window key can't break the
// composite Redis key structure.
func scrub(s string) string {
	if s == "" {
		return "-"
	}
	return strings.ReplaceAll(s, ":", "_")
}
