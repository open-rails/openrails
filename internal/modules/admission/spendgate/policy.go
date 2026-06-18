// Package spendgate is the Redis-backed admission primitive for openrails #513:
// ONE atomic Lua gate that checks a payer's affordability (cached balance minus
// the shared in-flight hold gauge) and every applicable spend-cap window, places
// a per-request hold, and frees it at capture/release — replacing the Postgres
// budget_window_state / budget_inflight_holds locked-tx path AND the separate
// Redis hold store.
//
// MODEL (decided 2026-06-17, Paul):
//   - Affordability uses an IN-MEMORY cached balance passed in by the caller
//     (refreshed off the #512 ledger); only the `held` reservation gauge and the
//     window counters live in shared Redis. Stale cache → bounded over-admission,
//     never ledger corruption (the durable truth is the #512 ledger).
//   - Spend-cap windows are PER-USER-STAGGERED (#337): "session" opens at the first
//     reserve when none is active and closes Duration later; "fixed" ticks at
//     offset + k*Duration forever, where offset is a deterministic phase derived
//     from the customer-scoped window key (no stored anchor). Resets are staggered
//     per payer so demand spreads instead of every payer resetting at once.
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

// Cadence selects how a window's reset boundary is derived. Both stagger resets
// per payer (#337); neither is a globally aligned calendar bucket.
type Cadence string

const (
	// CadenceSession: the window opens at the first reserve when none is active and
	// closes exactly Duration later; the next reserve after expiry opens a fresh
	// window. Lazily opened (the key's TTL IS the window — no stored anchor).
	CadenceSession Cadence = "session"
	// CadenceFixed: boundaries tick at offset + k*Duration forever, where offset is
	// a deterministic per-(customer,window) phase — the same staggered reset each
	// period, recomputable (no stored anchor, flush-safe).
	CadenceFixed Cadence = "fixed"
)

// NormalizeCadence maps "" to session and rejects unknown values. Mirrors the
// budgets engine's cadence vocabulary (session|fixed) so the Postgres→Policy
// loader can pass cadence through verbatim.
func NormalizeCadence(c string) (Cadence, error) {
	switch strings.TrimSpace(strings.ToLower(c)) {
	case "", string(CadenceSession):
		return CadenceSession, nil
	case string(CadenceFixed):
		return CadenceFixed, nil
	default:
		return "", fmt.Errorf("spendgate: unknown window cadence %q (want session|fixed)", c)
	}
}

// Scope is whose spend a window caps.
type Scope string

const (
	ScopePayer   Scope = "payer"   // the customer overall
	ScopeInvoker Scope = "invoker" // a specific API key / principal
	ScopeRole    Scope = "role"    // a role the invoker holds
	ScopeTier    Scope = "tier"    // the payer's tier
)

// Window is one {scope, cadence, duration, limit} cap. Limit and all reserved
// amounts are in the currency's minor units.
type Window struct {
	Scope    Scope         `json:"scope"`
	Cadence  Cadence       `json:"cadence"`
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

// identity is the stable Redis key prefix for this window under a payer base. For
// CadenceSession the counter key IS this prefix (one key, TTL = Duration set at
// open, never refreshed). For CadenceFixed the Lua appends ":<bucket>" where
// bucket = floor((now-offset)/Duration) and offset is the deterministic phase
// fixedOffsetMs(prefix, Duration) — no stored anchor. The base is hash-tagged
// ({merchant:customer}) so every key the Lua touches co-locates on one Cluster slot.
func (w resolvedWindow) identity(base string) string {
	return fmt.Sprintf("%s:w:%s:%s:%s:%s", base, w.Scope, scrub(w.scopeID), scrub(w.Key), w.Cadence)
}

// cadenceCode encodes the cadence for the Lua (0=session, 1=fixed).
func (w resolvedWindow) cadenceCode() int {
	if w.Cadence == CadenceFixed {
		return 1
	}
	return 0
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
