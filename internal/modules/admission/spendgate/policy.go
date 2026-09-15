// Package spendgate evaluates durable SQL admission operations and spend windows.
package spendgate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

// Scope is whose spend a window caps.
type Scope string

const (
	ScopePayer      Scope = "payer"       // the customer overall
	ScopeInvoker    Scope = "invoker"     // a specific API key / principal
	ScopeRole       Scope = "role"        // per-invoker cap selected by a role the invoker holds
	ScopeTrustLevel Scope = "trust_level" // the payer's trust level
)

// Window is one {scope, duration, limit} cap. Limit and all reserved amounts are
// in the currency's native units.
type Window struct {
	Scope    Scope         `json:"scope"`
	Duration time.Duration `json:"duration"`
	Limit    int64         `json:"limit"`
	// Key is a stable per-policy window identifier (e.g. "5h", "7d") so a window's
	// durable window identity survives across reserves. Distinct windows under one scope MUST
	// have distinct keys.
	Key string `json:"key"`
}

// ScopedWindows is the cap config for one concrete scope identity (e.g. invoker
// "svc:abc", or trust level "gold"). ScopeID is "" for the payer-wide scope.
type ScopedWindows struct {
	Scope   Scope    `json:"scope"`
	ScopeID string   `json:"scope_id"`
	Windows []Window `json:"windows"`
}

// Policy is the transactionally loaded cap configuration for one payer and unit.
type Policy struct {
	Scopes []ScopedWindows `json:"scopes"`
}

// Request names the principals a request runs under, for scope matching.
type Request struct {
	Invoker    string
	Roles      []string
	TrustLevel string
	Measure    string
}

// resolvedWindow binds a Window to the concrete scope identity it was configured
// for, so it maps to a stable durable window identity key.
type resolvedWindow struct {
	Window
	scopeID string
	invoker string
}

// EffectiveWindows returns every window that applies to req under collect-all
// semantics: all payer-scope windows, invoker-scope windows whose ScopeID matches
// req.Invoker, role-scope windows for any of req.Roles, and trust-level-scope
// windows for req.TrustLevel. Role windows include req.Invoker in their durable
// identity, so a role budget is independently metered for each concrete
// delegated invoker holding the role. The gate DENIES if ANY returned window is
// over its limit.
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
			match = req.Invoker != "" && roles[sw.ScopeID]
		case ScopeTrustLevel:
			match = req.TrustLevel != "" && sw.ScopeID == req.TrustLevel
		}
		if !match {
			continue
		}
		for _, w := range sw.Windows {
			w.Scope = sw.Scope
			rw := resolvedWindow{Window: w, scopeID: sw.ScopeID}
			if sw.Scope == ScopeRole {
				rw.invoker = req.Invoker
			}
			out = append(out, rw)
		}
	}
	return out
}

// identity uses unambiguous components. Neither the limit nor duration changes
// its history; a changed policy re-evaluates the appropriate bounded period.
func (w resolvedWindow) identity(base string) string {
	b, _ := json.Marshal([]string{base, string(w.Scope), w.scopeID, w.invoker, w.Key})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
