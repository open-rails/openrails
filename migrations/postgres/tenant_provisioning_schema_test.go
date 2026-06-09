package postgresmigrations

import (
	"strings"
	"testing"
)

func TestConsolidatedSchemaIncludesTenantProvisioningState(t *testing.T) {
	c := loadSchema001(t)

	for _, want := range []string{
		"billing_tier text",
		"webhook_host text",
		"webhook_path text",
		"CREATE TABLE billing.tenant_secrets",
		"CREATE TABLE billing.tenant_credential_audit",
		"CREATE TABLE billing.tenant_exports",
		"uq_tenants_webhook_host",
	} {
		if !strings.Contains(c, want) {
			t.Errorf("001 schema missing tenant provisioning final state %q", want)
		}
	}
}

func TestConsolidatedSchemaOmitsProvisioningTransitionDDL(t *testing.T) {
	c := loadSchema001(t)

	for _, forbidden := range []string{
		"ADD COLUMN IF NOT EXISTS billing_tier",
		"ADD COLUMN IF NOT EXISTS webhook_host",
		"DROP TABLE billing.tenants",
		"DROP COLUMN",
		"ALTER COLUMN status",
	} {
		if strings.Contains(c, forbidden) {
			t.Errorf("001 schema must not carry tenant provisioning transition DDL %q", forbidden)
		}
	}
}
