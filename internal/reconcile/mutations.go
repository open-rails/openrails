package reconcile

import (
	"fmt"

	"github.com/google/uuid"
)

func mutationRecordsForFinding(provider Provider, findingID uuid.UUID, f *Finding, evidence map[string]any, phase string) []MutationRecord {
	if f == nil || f.Apply == nil {
		return nil
	}
	base := MutationRecord{
		Phase:       phase,
		Provider:    provider,
		FindingID:   findingID,
		FindingType: f.Type,
		SubjectKey:  f.SubjectKey,
	}
	withEvidence := func(m MutationRecord, key string) (MutationRecord, bool) {
		if phase != "applied" {
			if m.RowsAffected == 0 {
				m.RowsAffected = 1
			}
			return m, true
		}
		if len(evidence) > 0 {
			m.Evidence = evidence
		}
		if key == "" {
			if m.RowsAffected == 0 {
				m.RowsAffected = 1
			}
			return m, true
		}
		v, ok := evidence[key]
		if !ok {
			return m, false
		}
		switch x := v.(type) {
		case bool:
			if !x {
				return m, false
			}
			m.RowsAffected = 1
		case int:
			if x <= 0 {
				return m, false
			}
			m.RowsAffected = x
		case int64:
			if x <= 0 {
				return m, false
			}
			m.RowsAffected = int(x)
		case float64:
			if x <= 0 {
				return m, false
			}
			m.RowsAffected = int(x)
		default:
			m.RowsAffected = 1
		}
		return m, true
	}
	add := func(out []MutationRecord, table, op, rowID, externalID, evidenceKey string, rows int) []MutationRecord {
		m := base
		m.Table = table
		m.Operation = op
		m.RowID = rowID
		m.ExternalID = externalID
		m.RowsAffected = rows
		if rec, ok := withEvidence(m, evidenceKey); ok {
			out = append(out, rec)
		}
		return out
	}

	a := f.Apply
	var out []MutationRecord
	switch {
	case a.CancelLocal != nil:
		out = add(out, "subscriptions", "update", a.CancelLocal.SubscriptionID.String(), "", "cancelled_locally", 1)
		out = add(out, "entitlements", "update", "", "", "entitlements_revoked", 0)
	case a.AdoptStatus != nil:
		out = add(out, "subscriptions", "update", a.AdoptStatus.SubscriptionID.String(), "", "adopted_status", 1)
	case a.BackfillPayment != nil:
		out = add(out, "payments", "insert", "", a.BackfillPayment.TransactionID, "payment_backfilled", 1)
		if a.BackfillPayment.Grant != nil {
			out = add(out, "entitlements", "insert", "", a.BackfillPayment.Grant.SubscriptionID.String(), "entitlements_granted_for", len(a.BackfillPayment.Grant.Entitlements))
		}
	case a.RecordRefund != nil:
		op := "insert"
		rowID := ""
		key := "refund_recorded"
		if a.RecordRefund.MarkRefundedOnly {
			op = "update"
			if a.RecordRefund.RefundedPaymentID != nil {
				rowID = a.RecordRefund.RefundedPaymentID.String()
			}
		}
		out = add(out, "payments", op, rowID, a.RecordRefund.TransactionID, key, 1)
	case a.AdoptVault != nil:
		out = add(out, "payment_methods", "update", a.AdoptVault.PaymentMethodID.String(), "", "vault_adopted", 1)
	case a.GrantEntitlements != nil:
		out = add(out, "entitlements", "insert", "", a.GrantEntitlements.SubscriptionID.String(), "entitlements_granted", len(a.GrantEntitlements.Entitlements))
	case a.RevokeEntitlements != nil:
		out = add(out, "entitlements", "update", "", a.RevokeEntitlements.SubscriptionID.String(), "entitlements_revoked", 0)
	case a.Materialize != nil:
		rowID := ""
		if phase == "applied" {
			if id, ok := stringEvidence(evidence, "materialized_subscription_id"); ok {
				rowID = id
			}
		}
		out = add(out, "subscriptions", "insert", rowID, a.Materialize.RailSubscriptionID, "materialized_subscription_id", 1)
		out = add(out, "entitlements", "insert", "", a.Materialize.RailSubscriptionID, "entitlements_granted", 0)
		if a.Materialize.Backfill != nil {
			out = add(out, "payments", "insert", "", a.Materialize.Backfill.TransactionID, "payment_backfilled", 1)
		}
	}
	return out
}

func stringEvidence(m map[string]any, key string) (string, bool) {
	v, ok := m[key]
	if !ok || v == nil {
		return "", false
	}
	switch x := v.(type) {
	case string:
		return x, x != ""
	case fmt.Stringer:
		s := x.String()
		return s, s != ""
	default:
		s := fmt.Sprint(x)
		return s, s != ""
	}
}
