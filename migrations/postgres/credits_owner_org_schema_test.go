package postgresmigrations

import (
	"strings"
	"testing"
)

// TestConsolidatedSchemaUsesTenantSubjectCreditModel pins the #332 billing
// identity model: payable identity = tenant_subject_id; the caller-supplied
// opaque attribution/enforcement string is `actor` (generic — no tensorhub
// vocabulary like invoker/endpoint); `resource` is the optional opaque
// what-was-it-for string. Balance/state tables carry NO actor (they are keyed
// per payer); user_credit_balances was renamed credit_balances.
func TestConsolidatedSchemaUsesTenantSubjectCreditModel(t *testing.T) {
	c := loadSchema001(t)

	for _, forbidden := range []string{
		"owner_id uuid",
		"owner_id UUID",
		"owner_id IS NULL",
		"6f1c9b3e2a445d7c8e109a2b3c4d5e6f",
		"openrails:personal tenant-subject:",
		" user_id text",
		"invoker",              // tensorhub vocabulary; generic term is actor
		"user_credit_balances", // renamed credit_balances (#332)
		"endpoint text",        // tensorhub vocabulary; generic term is resource
		"delegated_user_id",    // retired term (tenant_subject_id / actor)
	} {
		if strings.Contains(c, forbidden) {
			t.Errorf("001 schema must not carry retired credit identity artifact %q", forbidden)
		}
	}

	for _, want := range []string{
		// payer identity
		"tenant_subject_id uuid CONSTRAINT credit_transactions_tenant_subject_id_not_null NOT NULL",
		"tenant_subject_id uuid CONSTRAINT credit_blocks_tenant_subject_id_not_null NOT NULL",
		"tenant_subject_id uuid CONSTRAINT credit_balances_tenant_subject_id_not_null NOT NULL",
		// actor: ledger (attribution + per-actor cap sums) and usage log + budget enforcement
		"actor text CONSTRAINT credit_transactions_actor_not_null NOT NULL",
		"actor text CONSTRAINT usage_events_actor_not_null NOT NULL",
		"actor text CONSTRAINT budget_reservations_actor_not_null NOT NULL",
		"actor text CONSTRAINT credit_spend_limits_actor_not_null NOT NULL",
		// per-actor cap sum index on the ledger (spentInWindow)
		"idx_credit_transactions_payer_actor",
	} {
		if !strings.Contains(c, want) {
			t.Errorf("001 schema missing final credit identity definition %q", want)
		}
	}

	// Balance/state tables are payer-keyed: no actor column.
	for _, table := range []string{"credit_blocks", "credit_balances"} {
		start := strings.Index(c, "CREATE TABLE openrails."+table+" (")
		if start < 0 {
			t.Fatalf("001 schema missing table %s", table)
		}
		body := c[start:]
		body = body[:strings.Index(body, ");")]
		if strings.Contains(body, "actor") {
			t.Errorf("openrails.%s must not carry an actor column (payer-keyed, #332)", table)
		}
	}
}

func TestConsolidatedSchemaCreditIdempotencyIsTenantSubjectScoped(t *testing.T) {
	c := loadSchema001(t)

	for _, want := range []string{
		"uniq_credit_hold_idem_payer",
		"uniq_credit_deposit_idem_payer",
		"uniq_credit_withdrawal_idem_payer",
		"uq_credit_balances_payer_type",
		"(tenant_id, tenant_subject_id, credit_type_id, source, source_id)",
	} {
		if !strings.Contains(c, want) {
			t.Errorf("001 schema missing tenant-subject-scoped credit invariant %q", want)
		}
	}
}
