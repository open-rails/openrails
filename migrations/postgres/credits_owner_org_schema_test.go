package postgresmigrations

import (
	"strings"
	"testing"
)

func TestConsolidatedSchemaUsesTenantSubjectCreditModel(t *testing.T) {
	c := loadSchema001(t)

	for _, forbidden := range []string{
		"owner_id uuid",
		"owner_id UUID",
		"owner_id IS NULL",
		"6f1c9b3e2a445d7c8e109a2b3c4d5e6f",
		"openrails:personal tenant-subject:",
		" user_id text",
	} {
		if strings.Contains(c, forbidden) {
			t.Errorf("001 schema must not carry intermediate credit identity artifact %q", forbidden)
		}
	}

	for _, want := range []string{
		"tenant_subject_id uuid CONSTRAINT credit_transactions_tenant_subject_id_not_null NOT NULL",
		"tenant_subject_id uuid CONSTRAINT credit_blocks_tenant_subject_id_not_null NOT NULL",
		"tenant_subject_id uuid CONSTRAINT user_credit_balances_tenant_subject_id_not_null NOT NULL",
		"invoker_id text CONSTRAINT credit_transactions_invoker_id_not_null NOT NULL",
		"invoker_id text CONSTRAINT credit_blocks_invoker_id_not_null NOT NULL",
	} {
		if !strings.Contains(c, want) {
			t.Errorf("001 schema missing final credit identity definition %q", want)
		}
	}
}

func TestConsolidatedSchemaCreditIdempotencyIsTenantSubjectScoped(t *testing.T) {
	c := loadSchema001(t)

	for _, want := range []string{
		"uniq_credit_hold_idem_payer",
		"uniq_credit_deposit_idem_payer",
		"uniq_credit_withdrawal_idem_payer",
		"uq_user_credit_balances_payer_type",
		"(tenant_id, tenant_subject_id, credit_type_id, source, source_id)",
	} {
		if !strings.Contains(c, want) {
			t.Errorf("001 schema missing tenant-subject-scoped credit invariant %q", want)
		}
	}
}
