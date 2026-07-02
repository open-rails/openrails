// Package recommend is the structured-recommendation contract for operator
// findings (#692). Checks that emit ADMIN/OPERATOR findings write a
// machine-executable Recommendation into finding evidence under EvidenceKey,
// alongside the human prose in reconciliation_findings.recommended_action.
// The admin findings queue (POST /merchant/findings/{id}/resolve, outcome
// approve) executes it through existing machinery.
//
// Leaf package by design: importable from internal/intents,
// internal/reconcile and internal/reconcile/converge without cycles.
package recommend

import "encoding/json"

// Known actions. Params are plain JSON objects; ids travel as strings.
const (
	// ActionCancelAndRefund cancels a local subscription (the remote side
	// rides the durable rail-intents ledger — queue-always #679,
	// breaker-guarded) and refunds one payment through the rail's refund API
	// via the intents log.
	// Params: subscription_id (uuid, optional — no cancel when absent, e.g. a
	// pure one-off ownership duplicate); refund_payment_id (uuid, optional —
	// no refund when absent); at least one of the two is required; amount
	// (micros number, optional — defaults to the payment's full amount).
	ActionCancelAndRefund = "cancel_and_refund"
	// ActionRevokeEntitlement revokes one entitlement window as-of a time via
	// the existing entitlement service.
	// Params: entitlement_id (uuid, required); as_of (RFC3339, optional = now).
	ActionRevokeEntitlement = "revoke_entitlement"
	// ActionRecordAdminGrant records an admin-sourced product-access grant
	// (makes freeloader access legitimate).
	// Params: customer_id (string user id, required); product_id (uuid,
	// required); reason (string, optional — recorded in operator notes).
	ActionRecordAdminGrant = "record_admin_grant"
	// ActionAckResume is a plain resolution with no side effects; machinery
	// keyed off the finding STATUS (e.g. the #679 destructive-volume breaker)
	// re-arms itself when the finding leaves the open states. Params: none.
	ActionAckResume = "ack_resume"
)

// EvidenceKey is where a Recommendation lives inside finding evidence.
// Canonical location: top-level `evidence.recommendation`. Converge passes
// nest their Evidence map under "local", so `evidence.local.recommendation`
// is honored too (FromEvidence checks both).
const EvidenceKey = "recommendation"

// Recommendation is the machine-executable shape. Alternatives are
// informational (surfaced to the operator/dashboard); approve executes
// Action only — override_params can adjust params, never swap the action.
type Recommendation struct {
	Action       string           `json:"action"`
	Params       map[string]any   `json:"params,omitempty"`
	Alternatives []Recommendation `json:"alternatives,omitempty"`
}

// Map renders the recommendation as a plain map for embedding into an
// evidence map[string]any before JSON marshalling.
func (r Recommendation) Map() map[string]any {
	out := map[string]any{"action": r.Action}
	if len(r.Params) > 0 {
		out["params"] = r.Params
	}
	if len(r.Alternatives) > 0 {
		alts := make([]map[string]any, 0, len(r.Alternatives))
		for _, a := range r.Alternatives {
			alts = append(alts, a.Map())
		}
		out["alternatives"] = alts
	}
	return out
}

// FromEvidence extracts the structured recommendation from a finding's
// decoded evidence. ok=false when none is present or the action is empty.
func FromEvidence(evidence map[string]any) (Recommendation, bool) {
	if len(evidence) == 0 {
		return Recommendation{}, false
	}
	raw, found := evidence[EvidenceKey]
	if !found {
		if local, isMap := evidence["local"].(map[string]any); isMap {
			raw, found = local[EvidenceKey]
		}
	}
	if !found || raw == nil {
		return Recommendation{}, false
	}
	// Round-trip through JSON: evidence arrives as generic decoded maps.
	b, err := json.Marshal(raw)
	if err != nil {
		return Recommendation{}, false
	}
	var rec Recommendation
	if err := json.Unmarshal(b, &rec); err != nil || rec.Action == "" {
		return Recommendation{}, false
	}
	return rec, true
}

// --- Builders for the #690 detector emissions ---

// RevokeEntitlementRec recommends revoking one window; asOf empty = now at
// approve time. alternative (usually RecordAdminGrantRec) is informational.
func RevokeEntitlementRec(entitlementID, asOf string, alternative *Recommendation) Recommendation {
	params := map[string]any{"entitlement_id": entitlementID}
	if asOf != "" {
		params["as_of"] = asOf
	}
	rec := Recommendation{Action: ActionRevokeEntitlement, Params: params}
	if alternative != nil {
		rec.Alternatives = []Recommendation{*alternative}
	}
	return rec
}

// RecordAdminGrantRec recommends legitimizing access with an admin-sourced
// grant. productID may be empty when the source is gone (operator overrides).
func RecordAdminGrantRec(customerID, productID, reason string) Recommendation {
	params := map[string]any{"customer_id": customerID}
	if productID != "" {
		params["product_id"] = productID
	}
	if reason != "" {
		params["reason"] = reason
	}
	return Recommendation{Action: ActionRecordAdminGrant, Params: params}
}

// CancelAndRefundRec recommends cancelling a subscription and/or refunding
// one payment. Either id may be empty (refund-only for one-off duplicates
// without a subscription; cancel-only when no payment linkage exists) — the
// executor treats both as optional but requires at least one.
func CancelAndRefundRec(subscriptionID, refundPaymentID string) Recommendation {
	params := map[string]any{}
	if subscriptionID != "" {
		params["subscription_id"] = subscriptionID
	}
	if refundPaymentID != "" {
		params["refund_payment_id"] = refundPaymentID
	}
	return Recommendation{Action: ActionCancelAndRefund, Params: params}
}

// MergeParams overlays operator override_params onto the recommendation's
// params (override wins). Neither input is mutated.
func MergeParams(params, overrides map[string]any) map[string]any {
	merged := make(map[string]any, len(params)+len(overrides))
	for k, v := range params {
		merged[k] = v
	}
	for k, v := range overrides {
		merged[k] = v
	}
	return merged
}
