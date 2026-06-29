package postgresmigrations

import (
	"strings"
	"testing"
)

func TestIdentityAnchorsMigration(t *testing.T) {
	b, err := FS.ReadFile("033_identity_anchors.up.sql")
	if err != nil {
		t.Fatalf("read 033_identity_anchors.up.sql: %v", err)
	}
	sql := string(b)

	for _, want := range []string{
		"CREATE TABLE openrails.customer_anchors",
		"permission_group_id text NOT NULL",
		"CONSTRAINT customer_anchors_pkey PRIMARY KEY (permission_group_id)",
		"CREATE TABLE openrails.merchant_anchors",
		"CONSTRAINT merchant_anchors_pkey PRIMARY KEY (permission_group_id)",
		"ADD COLUMN owner text DEFAULT 'merchant'::text NOT NULL",
		"CONSTRAINT provider_accounts_owner_check CHECK",
		"COMMENT ON TABLE openrails.provider_accounts IS 'Merchant external account registry (#591; historical table name provider_accounts)",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("identity anchor migration missing %q", want)
		}
	}

	for _, forbidden := range []string{
		"REFERENCES profiles.",
		"REFERENCES authkit.",
		"ALTER TABLE openrails.payment_methods",
		"ALTER TABLE openrails.payments",
		"ALTER TABLE openrails.subscriptions",
		"DROP TABLE",
		"ALTER TABLE openrails.provider_accounts RENAME",
	} {
		if strings.Contains(sql, forbidden) {
			t.Errorf("identity anchor migration must stay additive/no-hot-rewrite; found %q", forbidden)
		}
	}
}
