package openrails

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
)

// FailPolicy is the EXPLICIT #248 hot-path policy for when OpenRails is
// unreachable/slow on the authorize call. There is intentionally NO zero-value
// default that silently picks a behavior — config must name one (see ParseFailPolicy).
type FailPolicy int

const (
	// FailClosed rejects the request when OpenRails can't be reached/decided in
	// time. Safe default: never run uncredited work. (#248 default)
	FailClosed FailPolicy = iota + 1
	// FailOpen admits the request when OpenRails is unreachable, logging the
	// fail-open and (best-effort) leaving a deferred-reconcile breadcrumb so the
	// spend can be settled later. Use only where availability beats strict billing.
	FailOpen
)

func (p FailPolicy) String() string {
	switch p {
	case FailClosed:
		return "fail_closed"
	case FailOpen:
		return "fail_open"
	default:
		return "unset"
	}
}

// ParseFailPolicy maps a config string to a FailPolicy. An empty string is NOT
// accepted as a silent default: the caller must decide what empty means (the
// server wiring treats empty as fail_closed and logs that it did so). An unknown
// non-empty value is an error so a typo can't silently flip the policy.
func ParseFailPolicy(s string) (FailPolicy, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "fail_closed", "failclosed", "closed":
		return FailClosed, nil
	case "fail_open", "failopen", "open":
		return FailOpen, nil
	case "":
		return 0, errors.New("openrails: fail_policy not set (must be fail_open or fail_closed)")
	default:
		return 0, fmt.Errorf("openrails: unknown fail_policy %q", s)
	}
}

// ReconcileEnqueuer records a fail-open admission so the spend can be settled
// later out-of-band. Optional; when nil a fail-open admission is only logged.
type ReconcileEnqueuer interface {
	EnqueueDeferredAuthorize(ctx context.Context, req AuthorizeRequest) error
}

// Decision is the outcome of AuthorizeHold. Allowed reports whether the request
// may proceed; ReservationID is the hold handle to Capture/Release (empty when
// fail-open admitted without a hold); FailedOpen marks an admission granted
// despite an unreachable OpenRails (so the caller can skip Capture/Release).
type Decision struct {
	Allowed       bool
	ReservationID string
	DenyCode      string
	FailedOpen    bool

	// BlockedBy is the deny axis from the unified admit verdict (#403/#404):
	// "throughput" | "money" | "budget" | "blocked" | "suspended" |
	// "unverified" | "resource". Empty for a plain credit-only authorize or an
	// allowed verdict.
	BlockedBy string
	// RetryAfterSeconds is the throughput/budget backoff hint when BlockedBy is a
	// rate-limit/quota axis (0 otherwise).
	RetryAfterSeconds int64
}

// Authorizer wraps Client with the #248 fail-policy on the hot path. It is the
// thing the request lifecycle calls: AuthorizeHold before the work, then Capture
// on success / Release on failure.
type Authorizer struct {
	client    *Client
	policy    FailPolicy
	reconcile ReconcileEnqueuer
}

// NewAuthorizer builds an Authorizer. policy must be FailClosed or FailOpen — a
// zero/invalid policy is rejected so the hot path can never run without an
// explicit decision. reconcile is optional.
func NewAuthorizer(client *Client, policy FailPolicy, reconcile ReconcileEnqueuer) (*Authorizer, error) {
	if client == nil {
		return nil, errors.New("openrails: authorizer requires a client")
	}
	if policy != FailClosed && policy != FailOpen {
		return nil, errors.New("openrails: authorizer requires an explicit fail policy")
	}
	return &Authorizer{client: client, policy: policy, reconcile: reconcile}, nil
}

// Policy reports the configured fail-policy.
func (a *Authorizer) Policy() FailPolicy { return a.policy }

// AuthorizeHold runs the atomic authorize+hold and applies the fail-policy:
//
//   - OpenRails reachable, allowed   -> Decision{Allowed:true, ReservationID:...}
//   - OpenRails reachable, denied    -> Decision{Allowed:false, DenyCode:...} (ALWAYS honored)
//   - OpenRails unreachable/timeout  -> fail-policy decides:
//     fail_closed -> Decision{Allowed:false, DenyCode:"openrails_unreachable"}
//     fail_open   -> Decision{Allowed:true, FailedOpen:true} + log + deferred reconcile
//
// A definitive deny is NEVER overridden by fail-open: the policy only governs the
// unreachable/timeout case, never an explicit OpenRails decision.
func (a *Authorizer) AuthorizeHold(ctx context.Context, req AuthorizeRequest) (Decision, error) {
	resp, err := a.client.Authorize(ctx, req)
	if err != nil {
		if errors.Is(err, ErrUnreachable) {
			return a.applyFailPolicy(ctx, req, err), nil
		}
		// Contract/4xx error: not fail-policy-able. Surface it.
		return Decision{}, err
	}
	if !resp.Allowed {
		return Decision{Allowed: false, DenyCode: firstNonEmpty(resp.DenyCode, "denied")}, nil
	}
	return Decision{Allowed: true, ReservationID: resp.ReservationID}, nil
}

// AdmitHold runs the UNIFIED admission verdict (#403/#404) — rate-limit/quota +
// rolling budget + suspension/blocklist/endpoint gating + the atomic credit hold
// — in ONE call, applying the same #248 fail-policy as AuthorizeHold:
//
//   - reachable, allowed   -> Decision{Allowed:true, ReservationID:...}
//   - reachable, denied    -> Decision{Allowed:false, BlockedBy:..., DenyCode:...} (ALWAYS honored)
//   - unreachable/timeout  -> fail-policy decides (fail_closed reject / fail_open admit)
//
// This is the single money+limit hot-path verdict that replaces the legacy
// tensorhub broker admit (Path B): gen-orch sizes the cost from the in-process
// tensorhub pricing and calls this once; the returned ReservationID is what the
// gRPC job-result path captures on success / releases on failure.
func (a *Authorizer) AdmitHold(ctx context.Context, req AdmitRequest) (Decision, error) {
	resp, err := a.client.Admit(ctx, req)
	if err != nil {
		if errors.Is(err, ErrUnreachable) {
			// Reuse the authorize fail-policy on the credit-bearing fields.
			return a.applyFailPolicy(ctx, AuthorizeRequest{
				PayerTenantID: req.PayerTenantID,
				Actor:         req.Actor,
				CreditType:    req.CreditType,
				EstimateCents: req.EstimateCents,
				RequestID:     req.RequestID,
			}, err), nil
		}
		return Decision{}, err
	}
	if !resp.Allowed {
		return Decision{
			Allowed:           false,
			BlockedBy:         strings.TrimSpace(resp.BlockedBy),
			DenyCode:          firstNonEmpty(resp.DenyCode, admitDenyCodeFor(resp.BlockedBy), "denied"),
			RetryAfterSeconds: resp.RetryAfterSeconds,
		}, nil
	}
	return Decision{Allowed: true, ReservationID: resp.ReservationID}, nil
}

// admitDenyCodeFor maps the unified verdict's BlockedBy axis to a stable deny
// code the HTTP layer can translate to a status (mirrors OpenRails' 429/402/403
// split). The money axis carries its own DenyCode from the ledger.
func admitDenyCodeFor(blockedBy string) string {
	switch strings.ToLower(strings.TrimSpace(blockedBy)) {
	case "throughput":
		return "rate_limit_exceeded"
	case "budget":
		return "insufficient_budget_window"
	case "resource":
		return "resource_not_allowed"
	case "suspended":
		return "tenant_inactive"
	case "unverified":
		return "payment_method_required"
	case "blocked":
		return "blocked"
	default:
		return ""
	}
}

// applyFailPolicy is the #248 decision point for an unreachable OpenRails.
func (a *Authorizer) applyFailPolicy(ctx context.Context, req AuthorizeRequest, cause error) Decision {
	switch a.policy {
	case FailOpen:
		log.Printf("[openrails] AUTHORIZE fail-open: admitting request_id=%s tenant_subject_id=%s actor=%s estimate_cents=%d cause=%v",
			req.RequestID, req.PayerTenantID, req.Actor, req.EstimateCents, cause)
		if a.reconcile != nil {
			if err := a.reconcile.EnqueueDeferredAuthorize(ctx, req); err != nil {
				log.Printf("[openrails] fail-open deferred-reconcile enqueue failed request_id=%s: %v", req.RequestID, err)
			}
		}
		return Decision{Allowed: true, FailedOpen: true}
	default: // FailClosed
		log.Printf("[openrails] AUTHORIZE fail-closed: rejecting request_id=%s tenant_subject_id=%s actor=%s cause=%v",
			req.RequestID, req.PayerTenantID, req.Actor, cause)
		return Decision{Allowed: false, DenyCode: "openrails_unreachable"}
	}
}

// Capture settles the hold at capturedCents (best-effort; logs on failure since
// the request already succeeded). No-op for a fail-open admission (no hold).
func (a *Authorizer) Capture(ctx context.Context, reservationID string, capturedCents int64, usage *CaptureUsage) {
	if strings.TrimSpace(reservationID) == "" {
		return
	}
	if err := a.client.Capture(ctx, reservationID, capturedCents, usage); err != nil {
		log.Printf("[openrails] capture failed reservation_id=%s captured_cents=%d: %v", reservationID, capturedCents, err)
	}
}

// Release frees the hold (best-effort; logs on failure). No-op for a fail-open
// admission (no hold).
func (a *Authorizer) Release(ctx context.Context, reservationID string) {
	if strings.TrimSpace(reservationID) == "" {
		return
	}
	if err := a.client.Release(ctx, reservationID); err != nil {
		log.Printf("[openrails] release failed reservation_id=%s: %v", reservationID, err)
	}
}

// ResourceRevenueDaily returns per-day revenue for a resource (#410), delegating
// to the OpenRails client. Read-only; not on the hot path.
func (a *Authorizer) ResourceRevenueDaily(ctx context.Context, resource string, fromUnix, toUnix int64) (*ResourceRevenueResponse, error) {
	return a.client.ResourceRevenueDaily(ctx, resource, fromUnix, toUnix)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
